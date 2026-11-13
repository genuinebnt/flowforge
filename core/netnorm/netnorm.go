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

// profile converts Unicode names to A-labels with UTS 46 lookup mapping
// (lowercasing, width folding), non-transitional processing (ß and ς are
// kept, not mapped to ss and σ) and the RFC 5893 Bidi rule. STD3 rules
// and DNS length checks are off: character and length validation is done
// afterwards on the ASCII result, so that underscores, which occur in real
// DNS names, are accepted and the length errors are our own.
//
// PORT: the Unicode and UTS 46 table versions of golang.org/x/net/idna and
// the Rust idna crate differ; conformance/scope/normalize pins the cases
// found so far (docs/PORTING_RUST.md 5.1).
var profile = idna.New(
	idna.MapForLookup(),
	idna.Transitional(false),
	idna.StrictDomainName(false),
	idna.VerifyDNSLength(false),
	idna.BidiRule(),
)

// Hostname normalizes a DNS name (spec 4.2): trim surrounding white space,
// remove one trailing dot, reject invalid UTF-8 and empty labels, convert
// to IDNA A-labels (which also lowercases), and reject labels over 63
// octets, names over 253 octets, characters outside [a-z0-9-_] and names
// whose last label is numeric, which resolvers and WHATWG URL parsers
// read as an IPv4 address (2130706433, 0x7f.1, 0177.0.0.1). The result is
// lowercase ASCII without a trailing dot; errors wrap the package's
// sentinels and all mean invalid_target. Hostname does not accept IP
// literals as such: "1.2.3.4" fails as numeric (use ParseHost).
func Hostname(s string) (string, error) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".")
	if s == "" {
		return "", ErrEmpty
	}
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: invalid UTF-8", ErrBadCharacter)
	}
	// Empty labels are rejected before IDNA because UTS 46 mapping can
	// turn other characters into dots (U+3002 "。" maps to "."); the second
	// check below catches empty labels created that way.
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
// whose last label is numeric as an IPv4 address. Octal needs no case of
// its own: a leading-zero octal label is also all decimal digits. A bare
// "0x" counts as numeric because WHATWG reads it as zero.
//
// PORT: both checks parse into a uint64, so an all-digit (or 0x + all-hex)
// label too large for 64 bits is NOT numeric here and the name is
// accepted (a.99999999999999999999). Rust's u64::from_str_radix fails at
// the same bound; keep it rather than switching to an "all digits" test
// without changing the spec and conformance data together.
func looksNumeric(label string) bool {
	if strings.HasPrefix(label, "0x") {
		_, err := strconv.ParseUint(label[2:], 16, 64)
		return err == nil || label == "0x"
	}
	_, err := strconv.ParseUint(label, 10, 64)
	return err == nil
}

// IP parses an address strictly (dotted-quad IPv4 without leading zeros,
// or IPv6), rejects zones, and maps IPv4-mapped IPv6 to IPv4 (spec 4.2).
// Surrounding white space is trimmed; brackets are not accepted here.
//
// PORT: netip.ParseAddr rejects octal-looking octets ("01.2.3.4"), short
// forms ("127.1") and hex, like Rust's IpAddr::from_str; it accepts
// embedded IPv4 in IPv6 ("::ffff:1.2.3.4"), which Unmap then turns into
// 1.2.3.4. Use Ipv6Addr::to_ipv4_mapped, not to_ipv4 (which also maps
// the deprecated compatible form ::1.2.3.4).
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

// CIDR parses a prefix in a/n form, zeroes host bits and maps IPv4-mapped
// IPv6 prefixes (::ffff:a.b.c.d/n, n >= 96) to the IPv4 prefix /(n-96).
// A mapped prefix shorter than /96 covers non-mapped addresses too and is
// rejected rather than silently widened. Errors wrap ErrBadCIDR or
// ErrIPZone.
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

// Host is a normalized host: exactly one of Name or IP is set. The zero
// value is not a valid host.
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
// strict one is rejected rather than treated as a name: input containing
// ":" must parse as IPv6, and a non-strict IPv4 form such as 127.1 fails
// in Hostname because its last label is numeric.
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

// URL is a normalized http(s) URL, as returned by ParseURL.
type URL struct {
	Scheme string // "http" or "https"
	Host   Host
	Port   int    // 0 when the scheme's default port applies
	Path   string // escaped, always begins with "/", dot segments removed
	Query  string // raw query without "?", unreserved escapes decoded
}

// String renders the canonical URL text (without fragment). IPv6 hosts are
// bracketed; ParseURL(u.String()) returns u again.
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

// EffectivePort returns the port the URL connects to: Port, or the
// scheme's default (443 for https, 80 for http) when Port is 0.
func (u URL) EffectivePort() int {
	if u.Port != 0 {
		return u.Port
	}
	if u.Scheme == "https" {
		return 443
	}
	return 80
}

// ParseURL normalizes a URL with RFC 3986 semantics (spec 4.2): lowercase
// scheme and host, only http and https, strip the scheme's own default port
// (:80 for http, :443 for https; https://x:80 keeps its port), drop the
// fragment, reject userinfo (even an empty "@"), reject ports outside
// 1-65535, percent-decode unreserved characters, uppercase the remaining
// escapes, remove "." and ".." path segments and turn an empty path into
// "/". The host is normalized with ParseHost. Errors wrap the package
// sentinels and all mean invalid_target.
//
// PORT: this is deliberately not WHATWG (docs/PORTING_RUST.md 5.1): do
// not use the url crate's normalization. Go's url.Parse is also lenient
// in ways the port must copy: it accepts spaces and non-ASCII bytes in
// the path, which EscapedPath then percent-encodes ("/b c" becomes
// "/b%20c", "/ü" becomes "/%C3%BC"), while the raw query is kept as
// written, spaces included. It rejects invalid escapes ("%zz") and "\"
// in the host.
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
	// url.Parse accepts an IPv6 zone ("[fe80::1%25eth0]"); a percent sign
	// can appear in no valid host, so reject before ParseHost sees it.
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

// normalizePath decodes unreserved escapes first and only then removes dot
// segments (RFC 3986 6.2.2 order), so "%2E%2E" is treated as "..". It
// follows RFC 3986 5.2.4: ".." never climbs above the root, and a final
// "." or ".." leaves a trailing slash ("/a/b/.." is "/a/").
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
			// out[0] is the empty segment before the leading "/", so
			// keep it: ".." at the root is dropped.
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
// suffix, or has no registrable part, the name is returned unchanged.
//
// PORT: the public suffix list is the snapshot compiled into
// golang.org/x/net/publicsuffix (ICANN and private sections, with the
// implicit "*" rule for unknown TLDs). Pin the same snapshot in Rust or
// accept that limiter keys can differ for new suffixes.
func RegistrableDomain(name string) string {
	d, err := publicsuffix.EffectiveTLDPlusOne(name)
	if err != nil {
		return name
	}
	return d
}
