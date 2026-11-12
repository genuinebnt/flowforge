package alerts

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"

	"github.com/genuinebnt/flowforge/core/netnorm"
)

// Webhook destination errors (SSRF protection, spec 15.4).
var (
	ErrWebhookURL          = errors.New("alerts: invalid webhook url")
	ErrWebhookScheme       = errors.New("alerts: webhook url must use https")
	ErrForbiddenHost       = errors.New("alerts: webhook host is not allowed")
	ErrForbiddenAddress    = errors.New("alerts: webhook destination address is not allowed")
	ErrNoResolvedAddresses = errors.New("alerts: webhook host resolved to no addresses")
)

// ValidateWebhookURL checks a webhook channel URL before it is stored or
// used: it must parse (netnorm rules: no userinfo), use https, and name a
// host that is not obviously internal. IP-literal hosts are checked with
// CheckAddr; names must be resolved by the caller and checked with
// CheckResolved immediately before connecting (and the connection pinned
// to the checked addresses, so DNS rebinding cannot swap them).
func ValidateWebhookURL(raw string) (netnorm.URL, error) {
	u, err := netnorm.ParseURL(raw)
	if err != nil {
		return netnorm.URL{}, fmt.Errorf("%w: %w", ErrWebhookURL, err)
	}
	if u.Scheme != "https" {
		return netnorm.URL{}, ErrWebhookScheme
	}
	if u.Host.IsIP() {
		if err := CheckAddr(u.Host.IP); err != nil {
			return netnorm.URL{}, err
		}
		return u, nil
	}
	name := u.Host.Name
	switch {
	case !strings.Contains(name, "."):
		return netnorm.URL{}, fmt.Errorf("%w: single-label name %q", ErrForbiddenHost, name)
	case name == "localhost" || strings.HasSuffix(name, ".localhost"),
		strings.HasSuffix(name, ".internal"), strings.HasSuffix(name, ".local"),
		strings.HasSuffix(name, ".home.arpa"):
		return netnorm.URL{}, fmt.Errorf("%w: %q is a local name", ErrForbiddenHost, name)
	}
	return u, nil
}

// CheckResolved fails unless every resolved address is public. An empty
// list fails closed.
func CheckResolved(addrs []netip.Addr) error {
	if len(addrs) == 0 {
		return ErrNoResolvedAddresses
	}
	for _, a := range addrs {
		if err := CheckAddr(a); err != nil {
			return err
		}
	}
	return nil
}

// CheckAddr fails if a is not a public unicast address.
func CheckAddr(a netip.Addr) error {
	if IsForbiddenAddr(a) {
		return fmt.Errorf("%w: %s", ErrForbiddenAddress, a)
	}
	return nil
}

// IsForbiddenAddr reports whether a webhook may not connect to a:
// invalid, zoned, unspecified, loopback, private (RFC 1918, ULA),
// link-local, multicast, CGNAT, documentation, benchmarking, reserved,
// IETF protocol assignment, discard-only, deprecated site-local, and
// IPv6 transition forms (NAT64, 6to4, IPv4-compatible, mapped) whose
// embedded IPv4 address is itself forbidden.
func IsForbiddenAddr(a netip.Addr) bool {
	if !a.IsValid() || a.Zone() != "" {
		return true
	}
	a = a.Unmap()
	if a.IsUnspecified() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() ||
		a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() || a.IsMulticast() {
		return true
	}
	if a.Is4() {
		for _, p := range deniedV4() {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
	for _, p := range deniedV6() {
		if p.Contains(a) {
			return true
		}
	}
	b := a.As16()
	switch {
	case prefix6(0x0064, 0xff9b, 0, 0, 0, 0, 96).Contains(a): // NAT64 well-known prefix
		return IsForbiddenAddr(netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]}))
	case prefix6(0x2002, 0, 0, 0, 0, 0, 16).Contains(a): // 6to4
		return IsForbiddenAddr(netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]}))
	}
	return false
}

func prefix4(a, b, c, d byte, bits int) netip.Prefix {
	return netip.PrefixFrom(netip.AddrFrom4([4]byte{a, b, c, d}), bits)
}

// prefix6 builds a prefix from the first six 16-bit groups.
func prefix6(g0, g1, g2, g3, g4, g5 uint16, bits int) netip.Prefix {
	var b [16]byte
	for i, g := range []uint16{g0, g1, g2, g3, g4, g5} {
		b[2*i], b[2*i+1] = byte(g>>8), byte(g)
	}
	return netip.PrefixFrom(netip.AddrFrom16(b), bits)
}

func deniedV4() []netip.Prefix {
	return []netip.Prefix{
		prefix4(0, 0, 0, 0, 8),       // "this network"
		prefix4(100, 64, 0, 0, 10),   // CGNAT shared address space
		prefix4(192, 0, 0, 0, 24),    // IETF protocol assignments
		prefix4(192, 0, 2, 0, 24),    // TEST-NET-1
		prefix4(192, 88, 99, 0, 24),  // deprecated 6to4 relay anycast
		prefix4(198, 18, 0, 0, 15),   // benchmarking
		prefix4(198, 51, 100, 0, 24), // TEST-NET-2
		prefix4(203, 0, 113, 0, 24),  // TEST-NET-3
		prefix4(240, 0, 0, 0, 4),     // reserved, including broadcast
	}
}

func deniedV6() []netip.Prefix {
	return []netip.Prefix{
		prefix6(0, 0, 0, 0, 0, 0, 96),                // IPv4-compatible (deprecated), ::, ::1
		prefix6(0x0064, 0xff9b, 0x0001, 0, 0, 0, 48), // local-use NAT64
		prefix6(0x0100, 0, 0, 0, 0, 0, 64),           // discard-only
		prefix6(0x2001, 0, 0, 0, 0, 0, 23),           // IETF protocol assignments (Teredo, ORCHID, ...)
		prefix6(0x2001, 0x0db8, 0, 0, 0, 0, 32),      // documentation
		prefix6(0x3fff, 0, 0, 0, 0, 0, 20),           // documentation (RFC 9637)
		prefix6(0xfec0, 0, 0, 0, 0, 0, 10),           // deprecated site-local
	}
}
