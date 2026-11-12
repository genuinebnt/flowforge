package scope

import "net/netip"

// Two matchers find the first rule (in document order) matching a target:
// indexedMatcher in production and linearMatcher as the reference it is
// tested and benchmarked against. Both offer
//
//	first(t, e)         smallest index of a rule with effect e matching t
//	firstDenyCIDR(a)    smallest index of a deny cidr rule containing a
//
// returning noRule when nothing matches. Policy dispatches on the concrete
// type so the target does not escape to the heap.

// linearMatcher scans every rule. It is a direct transcription of the
// spec and of compiledRule.matches.
type linearMatcher struct {
	rules []compiledRule
}

func (m *linearMatcher) first(t *Target, e RuleEffect) int32 {
	for i := range m.rules {
		if m.rules[i].effect == e && m.rules[i].matches(t) {
			return int32(i)
		}
	}
	return noRule
}

func (m *linearMatcher) firstDenyCIDR(a netip.Addr) int32 {
	for i := range m.rules {
		r := &m.rules[i]
		if r.effect == EffectDeny && r.kind == RuleCIDR && r.prefix.Contains(a) {
			return int32(i)
		}
	}
	return noRule
}

// indexedMatcher answers the same questions as linearMatcher from three
// indexes: a reversed-label trie for names, binary tries for prefixes and
// a per-origin path map for URL prefixes.
type indexedMatcher struct {
	names *domainTrie
	cidrs familyTries
	urls  *urlIndex
}

func newIndexedMatcher(rules []compiledRule) *indexedMatcher {
	m := &indexedMatcher{names: newDomainTrie(), cidrs: newFamilyTries(), urls: newURLIndex()}
	for i := range rules {
		r, idx := &rules[i], int32(i)
		switch r.kind {
		case RuleDomain:
			m.names.addExact(r.name, r.effect, idx)
		case RuleWildcardDomain:
			m.names.addBelow(r.name, r.effect, idx)
		case RuleCIDR:
			m.cidrs.add(r.prefix, r.effect, idx)
		case RuleURLPrefix:
			m.urls.add(r, idx)
		case RuleASN:
			// Informational only: never matches a target.
		}
	}
	return m
}

func (m *indexedMatcher) first(t *Target, e RuleEffect) int32 {
	best := noRule
	switch t.Kind {
	case TargetCIDR:
		if e == EffectDeny {
			// ipDeny holds single addresses, so overlapping means
			// "inside the target prefix".
			best = min(m.cidrs.overlapping(t.Prefix, e), m.urls.ipDeny.overlapping(t.Prefix, e))
		} else {
			best = m.cidrs.containing(t.Prefix, e)
		}
		return best
	case TargetURL:
		best = m.urls.lookupURL(t, e)
	default:
		if e == EffectDeny {
			best = m.urls.lookupHostDeny(t.hostKey)
		}
	}
	if a, ok := t.Addr(); ok {
		return min(best, m.cidrs.containsAddr(a, e))
	}
	return min(best, m.names.lookup(t.Host.Name, e))
}

func (m *indexedMatcher) firstDenyCIDR(a netip.Addr) int32 {
	return m.cidrs.containsAddr(a, EffectDeny)
}
