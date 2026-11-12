package netnorm

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/idna"
	"golang.org/x/net/publicsuffix"
)

// Normalization errors. All of them mean "invalid_target" to the scope
// decision algorithm.
var (
	ErrEmpty         = errors.New("netnorm: empty target")
	ErrEmptyLabel    = errors.New("netnorm: empty label")
	ErrLabelTooLong  = errors.New("netnorm: label longer than 63 octets")
	ErrNameTooLong   = errors.New("netnorm: name longer than 253 octets")
	ErrBadCharacter  = errors.New("netnorm: character not allowed in hostname")
	ErrIDNA          = errors.New("netnorm: name is not valid IDNA")
	ErrAmbiguousIPv4 = errors.New("netnorm: numeric hostname looks like a non-canonical IPv4 form")
	ErrBadIP         = errors.New("netnorm: invalid IP address")
	ErrIPZone        = errors.New("netnorm: IPv6 zones are not allowed")
	ErrBadCIDR       = errors.New("netnorm: invalid CIDR prefix")
	ErrBadURL        = errors.New("netnorm: invalid URL")
	ErrUserinfo      = errors.New("netnorm: URL carries userinfo")
	ErrBadScheme     = errors.New("netnorm: URL scheme must be http or https")
)

// profile converts Unicode names to A-labels with lookup mapping
// (lowercasing, width folding). Character validation is done separately so
// that underscores, which occur in real DNS names, are accepted.
var profile = idna.New(
	idna.MapForLookup(),
	idna.Transitional(false),
	idna.StrictDomainName(false),
	idna.VerifyDNSLength(false),
	idna.BidiRule(),
)

// Hostname normalizes a DNS name: trim surrounding space, lowercase,
// remove one trailing dot, convert to IDNA A-labels, and reject empty
// labels, labels over 63 octets, names over 253 octets, characters outside
// [a-z0-9-_] and numeric names that a resolver could read as an IPv4
// address in a non-canonical form (decimal integer, hex, octal).
func Hostname(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return "", ErrEmpty
	}
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: invalid UTF-8", ErrBadCharacter)
	}
	for _, label := range strings.Split(s, ".") {
		if label == "" {
			return "", fmt.Errorf("%w in %q", ErrEmptyLabel, s)
		}
	}
	a, err := profile.ToASCII(s)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrIDNA, err)
	}
	if len(a) > 253 {
		return "", ErrNameTooLong
	}
	labels := strings.Split(a, ".")
	for _, label := range labels {
		if label == "" {
			return "", fmt.Errorf("%w in %q", ErrEmptyLabel, s)
		}
		if len(label) > 63 {
			return "", ErrLabelTooLong
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return "", fmt.Errorf("%w: %q", ErrBadCharacter, c)
			}
		}
	}
	if looksNumeric(labels[len(labels)-1]) {
		return "", fmt.Errorf("%w: %q", ErrAmbiguousIPv4, a)
	}
	return a, nil
}

// looksNumeric reports whether a label is a number in decimal, octal or
// hex notation. The WHATWG URL host parser and many resolvers treat a name
// whose last label is numeric as an IPv4 address.
func looksNumeric(label string) bool {
	if strings.HasPrefix(label, "0x") {
		_, err := strconv.ParseUint(label[2:], 16, 64)
		return err == nil || label == "0x"
	}
	_, err := strconv.ParseUint(label, 10, 64)
	return err == nil
}

// IP parses an address strictly (dotted-quad IPv4 without leading zeros,
// or IPv6), rejects zones, and maps IPv4-mapped IPv6 to IPv4.
func IP(s string) (netip.Addr, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return netip.Addr{}, ErrEmpty
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: %v", ErrBadIP, err)
	}
	if a.Zone() != "" {
		return netip.Addr{}, ErrIPZone
	}
	return a.Unmap(), nil
}

// CIDR parses a prefix, zeroes host bits and maps IPv4-mapped IPv6
// prefixes to IPv4.
func CIDR(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	p, err := netip.ParsePrefix(s)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("%w: %v", ErrBadCIDR, err)
	}
	if p.Addr().Zone() != "" {
		return netip.Prefix{}, ErrIPZone
	}
	if a := p.Addr(); a.Is4In6() {
		if p.Bits() < 96 {
			return netip.Prefix{}, fmt.Errorf("%w: mapped prefix shorter than /96", ErrBadCIDR)
		}
		p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
	}
	return p.Masked(), nil
}

// Host is a normalized host: exactly one of Name or IP is set.
type Host struct {
	Name string     // normalized DNS name, if the host is a name
	IP   netip.Addr // canonical address, if the host is an IP literal
}

// IsIP reports whether h is an IP literal.
func (h Host) IsIP() bool { return h.IP.IsValid() }

// String returns the canonical text form (IPv6 without brackets).
func (h Host) String() string {
	if h.IsIP() {
		return h.IP.String()
	}
	return h.Name
}

// ParseHost normalizes a host that may be a name or an IP literal. IPv6
// literals may be bracketed. Anything that looks like an IP but is not a
// strict one is rejected rather than treated as a name.
func ParseHost(s string) (Host, error) {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		a, err := IP(s[1 : len(s)-1])
		if err != nil {
			return Host{}, err
		}
		return Host{IP: a}, nil
	}
	if strings.Contains(s, ":") {
		a, err := IP(s)
		if err != nil {
			return Host{}, err
		}
		return Host{IP: a}, nil
	}
	if a, err := IP(s); err == nil {
		return Host{IP: a}, nil
	}
	n, err := Hostname(s)
	if err != nil {
		return Host{}, err
	}
	return Host{Name: n}, nil
}

// URL is a normalized http(s) URL.
type URL struct {
	Scheme string // "http" or "https"
	Host   Host
	Port   int    // 0 when the scheme's default port applies
	Path   string // always begins with "/"
	Query  string // raw query without "?", unreserved escapes decoded
}

// String renders the canonical URL text (without fragment).
func (u URL) String() string {
	var b strings.Builder
	b.WriteString(u.Scheme)
	b.WriteString("://")
	if u.Host.IsIP() && u.Host.IP.Is6() {
		b.WriteString("[" + u.Host.IP.String() + "]")
	} else {
		b.WriteString(u.Host.String())
	}
	if u.Port != 0 {
		b.WriteString(":" + strconv.Itoa(u.Port))
	}
	b.WriteString(u.Path)
	if u.Query != "" {
		b.WriteString("?" + u.Query)
	}
	return b.String()
}

// EffectivePort returns the port the URL connects to.
func (u URL) EffectivePort() int {
	if u.Port != 0 {
		return u.Port
	}
	if u.Scheme == "https" {
		return 443
	}
	return 80
}

// ParseURL normalizes a URL: lowercase scheme and host, strip the scheme's
// default port, drop the fragment, reject userinfo, percent-decode
// unreserved characters and remove "." and ".." path segments.
func ParseURL(s string) (URL, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return URL{}, ErrEmpty
	}
	pu, err := url.Parse(s)
	if err != nil {
		return URL{}, fmt.Errorf("%w: %v", ErrBadURL, err)
	}
	if pu.User != nil {
		return URL{}, ErrUserinfo
	}
	scheme := strings.ToLower(pu.Scheme)
	if scheme != "http" && scheme != "https" {
		return URL{}, ErrBadScheme
	}
	if pu.Opaque != "" || pu.Host == "" {
		return URL{}, fmt.Errorf("%w: missing host", ErrBadURL)
	}
	hostPart := pu.Hostname()
	if strings.Contains(hostPart, "%") {
		return URL{}, ErrIPZone
	}
	host, err := ParseHost(hostPart)
	if err != nil {
		return URL{}, err
	}
	port := 0
	if p := pu.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > 65535 {
			return URL{}, fmt.Errorf("%w: bad port %q", ErrBadURL, p)
		}
		if !(scheme == "http" && n == 80) && !(scheme == "https" && n == 443) {
			port = n
		}
	}
	path, err := normalizePath(pu.EscapedPath())
	if err != nil {
		return URL{}, err
	}
	query, err := decodeUnreserved(pu.RawQuery)
	if err != nil {
		return URL{}, err
	}
	return URL{Scheme: scheme, Host: host, Port: port, Path: path, Query: query}, nil
}

func normalizePath(escaped string) (string, error) {
	p, err := decodeUnreserved(escaped)
	if err != nil {
		return "", err
	}
	if p == "" {
		return "/", nil
	}
	segs := strings.Split(p, "/")
	out := make([]string, 0, len(segs))
	for i, seg := range segs {
		switch seg {
		case ".":
			if i == len(segs)-1 {
				out = append(out, "")
			}
		case "..":
			if len(out) > 1 {
				out = out[:len(out)-1]
			}
			if i == len(segs)-1 {
				out = append(out, "")
			}
		default:
			out = append(out, seg)
		}
	}
	res := strings.Join(out, "/")
	if !strings.HasPrefix(res, "/") {
		res = "/" + res
	}
	return res, nil
}

// decodeUnreserved decodes %XX escapes of unreserved characters
// (ALPHA / DIGIT / "-" / "." / "_" / "~") and uppercases the hex digits of
// the escapes that remain (RFC 3986 section 6.2.2).
func decodeUnreserved(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("%w: truncated escape", ErrBadURL)
		}
		v, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
		if err != nil {
			return "", fmt.Errorf("%w: bad escape %q", ErrBadURL, s[i:i+3])
		}
		c := byte(v)
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(s[i+1:i+3]))
		}
		i += 2
	}
	return b.String(), nil
}

// RegistrableDomain returns the public-suffix-plus-one of a normalized
// name (for example api.eu.acme.co.uk -> acme.co.uk). It is used to key
// per-target rate limits (spec 14.3). If the name is itself a public
// suffix, the name is returned unchanged.
func RegistrableDomain(name string) string {
	d, err := publicsuffix.EffectiveTLDPlusOne(name)
	if err != nil {
		return name
	}
	return d
}
