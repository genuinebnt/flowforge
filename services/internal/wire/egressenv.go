package wire

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/services/egressproxy"
)

// EgressConfigFromEnv reads the standalone egress proxy's v2 settings
// through getenv (services/egressproxy/README.md lists the variables):
//
//	FF_EGRESS_PORTS           port policy: "80,443,8000-8099" (default 80,443)
//	FF_EGRESS_INTEL           "default", "none" (the default) or a list of
//	                          sources: host[:port...], "*.host" for subdomains,
//	                          "host=ip" to pin an address; "default" may be
//	                          combined with extra entries
//	FF_EGRESS_SOCKS_LISTEN    SOCKS5 listen address ("" disables it)
//	FF_EGRESS_SOCKS_ADVERTISE address put in tickets
//	FF_EGRESS_DNS_LISTEN      DNS forwarder listen address, UDP and TCP
//	FF_EGRESS_DNS_ADVERTISE   address put in tickets
//	FF_EGRESS_DNS_UPSTREAM    recursive resolver host:port (default: the
//	                          system resolver)
//	FF_REDIS_ADDR, FF_REDIS_AUTH  shared per-target limiter
func EgressConfigFromEnv(getenv func(string) string) (EgressOptions, error) {
	o := EgressOptions{
		SOCKSListen: getenv("FF_EGRESS_SOCKS_LISTEN"), SOCKSAdvertise: getenv("FF_EGRESS_SOCKS_ADVERTISE"),
		DNSListen: getenv("FF_EGRESS_DNS_LISTEN"), DNSAdvertise: getenv("FF_EGRESS_DNS_ADVERTISE"),
		RedisAddr: getenv("FF_REDIS_ADDR"), RedisAuth: getenv("FF_REDIS_AUTH"),
	}
	if v := getenv("FF_EGRESS_PORTS"); v != "" {
		rs, err := ParsePortRanges(v)
		if err != nil {
			return o, fmt.Errorf("FF_EGRESS_PORTS: %w", err)
		}
		o.Ports = &egressproxy.PortPolicy{Default: rs}
	}
	intel, err := ParseIntelSources(getenv("FF_EGRESS_INTEL"))
	if err != nil {
		return o, fmt.Errorf("FF_EGRESS_INTEL: %w", err)
	}
	o.Intel = intel
	if v := getenv("FF_EGRESS_DNS_UPSTREAM"); v != "" {
		o.Upstream = egressproxy.ServerUpstream{Addr: v}
	}
	return o, nil
}

// ParsePortRanges parses "80,443,8000-8099". An empty policy is refused:
// it would make every host-level tunnel fail without saying why.
func ParsePortRanges(s string) ([]egressproxy.PortRange, error) {
	var out []egressproxy.PortRange
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		if !isRange {
			hi = lo
		}
		a, err1 := strconv.Atoi(lo)
		b, err2 := strconv.Atoi(hi)
		if err1 != nil || err2 != nil || a < 1 || b > 65535 || a > b {
			return nil, fmt.Errorf("bad port range %q", part)
		}
		out = append(out, egressproxy.PortRange{From: a, To: b})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no ports")
	}
	return out, nil
}

// ParseIntelSources parses FF_EGRESS_INTEL (see EgressConfigFromEnv).
func ParseIntelSources(s string) ([]egressproxy.IntelSource, error) {
	var out []egressproxy.IntelSource
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		switch part {
		case "", "none":
			continue
		case "default":
			out = append(out, egressproxy.DefaultIntelSources()...)
			continue
		}
		var src egressproxy.IntelSource
		spec, pin, hasPin := strings.Cut(part, "=")
		if hasPin {
			a, err := netip.ParseAddr(pin)
			if err != nil {
				return nil, fmt.Errorf("bad pinned address in %q", part)
			}
			src.Addrs = []netip.Addr{a}
		}
		fields := strings.Split(spec, ":")
		src.Host = fields[0]
		if h, ok := strings.CutPrefix(src.Host, "*."); ok {
			src.Host, src.Subdomains = h, true
		}
		if src.Host == "" {
			return nil, fmt.Errorf("empty host in %q", part)
		}
		for _, ps := range fields[1:] {
			p, err := strconv.Atoi(ps)
			if err != nil || p < 1 || p > 65535 {
				return nil, fmt.Errorf("bad port in %q", part)
			}
			src.Ports = append(src.Ports, p)
		}
		out = append(out, src)
	}
	return out, nil
}
