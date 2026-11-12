package scope

import (
	"net/netip"
	"strings"
)

// urlIndex indexes url_prefix rules by (scheme, host, port) and then by
// exact path prefix. A lookup probes one map entry per path segment of the
// target, so its cost depends on the path depth, not on the rule count.
type urlIndex struct {
	byOrigin map[urlOrigin]*originPaths
	// hostDeny holds, per canonical host, the smallest deny url_prefix
	// index: host and IP targets naming that host are denied.
	hostDeny map[string]int32
	// ipDeny holds the IP hosts of deny url_prefix rules, so CIDR targets
	// that contain one are denied.
	ipDeny familyTries
}

// originPaths holds the rule paths of one origin. maxLen bounds the
// probes of a lookup, so a very long target path costs no more than the
// longest rule path.
type originPaths struct {
	paths  map[string]ruleSet
	maxLen int
}

type urlOrigin struct {
	scheme string
	host   string
	port   int
}

func newURLIndex() *urlIndex {
	return &urlIndex{
		byOrigin: make(map[urlOrigin]*originPaths),
		hostDeny: make(map[string]int32),
		ipDeny:   newFamilyTries(),
	}
}

func (x *urlIndex) add(r *compiledRule, idx int32) {
	o := urlOrigin{r.url.Scheme, r.urlHost, r.urlPort}
	op := x.byOrigin[o]
	if op == nil {
		op = &originPaths{paths: make(map[string]ruleSet)}
		x.byOrigin[o] = op
	}
	rs, ok := op.paths[r.url.Path]
	if !ok {
		rs = emptyRuleSet()
	}
	rs.add(r.effect, idx)
	op.paths[r.url.Path] = rs
	op.maxLen = max(op.maxLen, len(r.url.Path))
	if r.effect == EffectDeny {
		if cur, ok := x.hostDeny[r.urlHost]; !ok || idx < cur {
			x.hostDeny[r.urlHost] = idx
		}
		if r.url.Host.IsIP() {
			a := r.url.Host.IP
			x.ipDeny.add(netip.PrefixFrom(a, a.BitLen()), EffectDeny, idx)
		}
	}
}

// lookupURL returns the smallest index of a url_prefix rule with effect e
// matching URL target t. The candidate rule paths for a target path P are
// P itself and, for every "/" at position i, P[:i] and P[:i+1]: exactly
// the prefixes pathHasPrefix accepts. Candidates longer than the longest
// rule path of the origin are skipped.
func (x *urlIndex) lookupURL(t *Target, e RuleEffect) int32 {
	op := x.byOrigin[urlOrigin{t.URL.Scheme, t.hostKey, t.URL.EffectivePort()}]
	if op == nil {
		return noRule
	}
	p := t.URL.Path
	best := noRule
	probe := func(s string) {
		if len(s) > op.maxLen {
			return
		}
		if rs, ok := op.paths[s]; ok {
			best = min(best, rs.get(e))
		}
	}
	probe(p)
	for i := 0; i < len(p) && i < op.maxLen; {
		j := strings.IndexByte(p[i:], '/')
		if j < 0 {
			break
		}
		i += j
		probe(p[:i])
		probe(p[:i+1])
		i++
	}
	return best
}

// lookupHostDeny returns the smallest deny url_prefix index for a host or
// IP target.
func (x *urlIndex) lookupHostDeny(host string) int32 {
	if idx, ok := x.hostDeny[host]; ok {
		return idx
	}
	return noRule
}
