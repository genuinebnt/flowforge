package scope

import (
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/core/netnorm"
)

// compiledRule is a rule with its value normalized once at compile time.
type compiledRule struct {
	id     string
	effect RuleEffect
	kind   RuleKind

	name    string       // RuleDomain: the name; RuleWildcardDomain: the parent below which names match
	prefix  netip.Prefix // RuleCIDR
	url     netnorm.URL  // RuleURLPrefix
	urlHost string       // RuleURLPrefix: canonical host text
	urlPort int          // RuleURLPrefix: effective port
	asn     uint32       // RuleASN
}

// compileRule normalizes r.Value according to r.Kind.
func compileRule(r Rule) (compiledRule, error) {
	c := compiledRule{id: r.ID, effect: r.Effect, kind: r.Kind}
	if r.ID == "" {
		return c, errors.New("empty id")
	}
	if r.Effect != EffectAllow && r.Effect != EffectDeny {
		return c, fmt.Errorf("unknown effect %s", r.Effect)
	}
	v := strings.TrimSpace(r.Value)
	switch r.Kind {
	case RuleDomain:
		n, err := netnorm.Hostname(v)
		if err != nil {
			return c, fmt.Errorf("domain %q: %w", r.Value, err)
		}
		c.name = n
	case RuleWildcardDomain:
		rest, ok := strings.CutPrefix(v, "*.")
		if !ok {
			return c, fmt.Errorf("wildcard_domain %q must start with \"*.\"", r.Value)
		}
		n, err := netnorm.Hostname(rest)
		if err != nil {
			return c, fmt.Errorf("wildcard_domain %q: %w", r.Value, err)
		}
		c.name = n
	case RuleCIDR:
		p, err := netnorm.CIDR(v)
		if err != nil {
			return c, fmt.Errorf("cidr %q: %w", r.Value, err)
		}
		c.prefix = p
	case RuleURLPrefix:
		u, err := netnorm.ParseURL(v)
		if err != nil {
			return c, fmt.Errorf("url_prefix %q: %w", r.Value, err)
		}
		if u.Query != "" || strings.Contains(v, "?") {
			return c, fmt.Errorf("url_prefix %q must not carry a query", r.Value)
		}
		c.url, c.urlHost, c.urlPort = u, u.Host.String(), u.EffectivePort()
	case RuleASN:
		digits, ok := strings.CutPrefix(strings.ToUpper(v), "AS")
		n, err := strconv.ParseUint(digits, 10, 32)
		if !ok || err != nil {
			return c, fmt.Errorf("asn %q must look like AS64500", r.Value)
		}
		c.asn = uint32(n)
	default:
		return c, fmt.Errorf("unknown kind %s", r.Kind)
	}
	return c, nil
}

// matches is the reference (specification-level) definition of whether
// rule r matches normalized target t. The indexed matcher must agree with
// it on every input; the linear matcher uses it directly.
//
// Semantics, beyond the obvious name and address comparisons:
//   - name rules match host and URL targets by host only;
//   - cidr rules match IP literals (bare, as host, or as URL host); for a
//     CIDR target an allow rule must contain the whole prefix while a deny
//     rule matches on any overlap;
//   - url_prefix rules match URL targets on scheme, host, port and a path
//     prefix at a segment boundary; a deny url_prefix additionally matches
//     host and IP targets naming its host, and CIDR targets containing its
//     IP host, because a host-level action may reach the excluded path;
//   - asn rules never match.
func (r *compiledRule) matches(t *Target) bool {
	switch r.kind {
	case RuleDomain:
		return t.Kind != TargetCIDR && t.Host.Name != "" && t.Host.Name == r.name
	case RuleWildcardDomain:
		n := t.Name()
		return len(n) > len(r.name)+1 && strings.HasSuffix(n, r.name) && n[len(n)-len(r.name)-1] == '.'
	case RuleCIDR:
		if t.Kind == TargetCIDR {
			if r.effect == EffectDeny {
				return r.prefix.Overlaps(t.Prefix)
			}
			return r.prefix.Bits() <= t.Prefix.Bits() && r.prefix.Contains(t.Prefix.Addr())
		}
		a, ok := t.Addr()
		return ok && r.prefix.Contains(a)
	case RuleURLPrefix:
		switch t.Kind {
		case TargetURL:
			return t.URL.Scheme == r.url.Scheme && t.hostKey == r.urlHost &&
				t.URL.EffectivePort() == r.urlPort && pathHasPrefix(t.URL.Path, r.url.Path)
		case TargetHost, TargetIP:
			return r.effect == EffectDeny && t.hostKey == r.urlHost
		case TargetCIDR:
			return r.effect == EffectDeny && r.url.Host.IsIP() && t.Prefix.Contains(r.url.Host.IP)
		}
	}
	return false
}

// pathHasPrefix reports whether path starts with prefix on a segment
// boundary: equal, or prefix ends in "/" and path extends it, or path
// continues prefix with "/".
func pathHasPrefix(path, prefix string) bool {
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	if len(path) == len(prefix) || strings.HasSuffix(prefix, "/") {
		return true
	}
	return path[len(prefix)] == '/'
}
