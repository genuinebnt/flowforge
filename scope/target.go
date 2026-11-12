package scope

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/genuinebnt/flowforge/core/netnorm"
)

// Target is a normalized target (spec 4.2). Exactly one representation is
// populated, according to Kind:
//
//   - TargetHost and TargetIP: Host (a name, or an IP for IP literals);
//   - TargetURL: URL, with Host mirroring URL.Host;
//   - TargetCIDR: Prefix.
type Target struct {
	Kind   TargetKind
	Host   netnorm.Host
	URL    netnorm.URL
	Prefix netip.Prefix

	text    string // canonical text
	hostKey string // canonical host text, "" for CIDR targets
}

// Normalize parses raw as a target of the given kind and brings it to the
// canonical form of spec 4.2. TargetUnspecified infers the kind: text with
// "://" is a URL, text with "/" a CIDR prefix, anything else a host. Every
// failure wraps ErrInvalidTarget and the netnorm cause.
func Normalize(raw string, kind TargetKind) (Target, error) {
	if kind == TargetUnspecified {
		kind = inferKind(raw)
	}
	t := Target{Kind: kind}
	switch kind {
	case TargetHost:
		h, err := netnorm.ParseHost(raw)
		if err != nil {
			return Target{}, invalidTarget(raw, err)
		}
		t.Host = h
	case TargetIP:
		a, err := netnorm.IP(raw)
		if err != nil {
			return Target{}, invalidTarget(raw, err)
		}
		t.Host = netnorm.Host{IP: a}
	case TargetURL:
		u, err := netnorm.ParseURL(raw)
		if err != nil {
			return Target{}, invalidTarget(raw, err)
		}
		t.URL, t.Host = u, u.Host
		t.text = u.String()
	case TargetCIDR:
		p, err := netnorm.CIDR(raw)
		if err != nil {
			return Target{}, invalidTarget(raw, err)
		}
		t.Prefix = p
		t.text = p.String()
		return t, nil
	default:
		return Target{}, fmt.Errorf("%w: unknown target kind %d", ErrInvalidTarget, int(kind))
	}
	t.hostKey = t.Host.String()
	if t.text == "" {
		t.text = t.hostKey
	}
	return t, nil
}

func inferKind(raw string) TargetKind {
	switch {
	case strings.Contains(raw, "://"):
		return TargetURL
	case strings.Contains(raw, "/"):
		return TargetCIDR
	default:
		return TargetHost
	}
}

func invalidTarget(raw string, cause error) error {
	return fmt.Errorf("%w %q: %w", ErrInvalidTarget, raw, cause)
}

// String returns the canonical text of t: a name, an IP address (IPv6
// without brackets), a canonical URL or a masked prefix.
func (t Target) String() string { return t.text }

// Name returns the DNS name of a host or URL target, or "" when the target
// is an IP literal or a prefix.
func (t Target) Name() string {
	if t.Kind == TargetCIDR {
		return ""
	}
	return t.Host.Name
}

// Addr returns the address of an IP literal target (a bare IP, a host that
// is an IP, or a URL whose host is an IP).
func (t Target) Addr() (netip.Addr, bool) {
	if t.Kind == TargetCIDR || !t.Host.IsIP() {
		return netip.Addr{}, false
	}
	return t.Host.IP, true
}

// IsName reports whether the connection behind t goes to a DNS name, which
// is when active checks resolve and pin addresses.
func (t Target) IsName() bool { return t.Name() != "" }
