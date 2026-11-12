package scope

import "net/netip"

// cidrTrie is a binary prefix trie over address bits, one per address
// family. Nodes live in a slice and refer to children by index, which
// keeps the structure compact and cheap for the garbage collector. A
// lookup visits at most one node per prefix bit (32 or 128), whatever the
// number of rules.
type cidrTrie struct {
	nodes []cidrNode
}

type cidrNode struct {
	child [2]int32 // 0 = absent (the root is never a child)
	own   ruleSet  // rules whose prefix ends at this node
	sub   ruleSet  // smallest rule index in this node's subtree, own included
}

func newCIDRTrie() *cidrTrie {
	return &cidrTrie{nodes: []cidrNode{{own: emptyRuleSet(), sub: emptyRuleSet()}}}
}

// addrBit returns bit i (0 = most significant) of a.
func addrBit(a *[16]byte, i int) int { return int(a[i>>3]>>(7-uint(i&7))) & 1 }

func addrBytes(a netip.Addr) [16]byte {
	if a.Is4() {
		var b [16]byte
		v4 := a.As4()
		copy(b[:], v4[:])
		return b
	}
	return a.As16()
}

func (t *cidrTrie) add(p netip.Prefix, e RuleEffect, idx int32) {
	b := addrBytes(p.Addr())
	n := int32(0)
	t.nodes[n].sub.add(e, idx)
	for i := 0; i < p.Bits(); i++ {
		bit := addrBit(&b, i)
		next := t.nodes[n].child[bit]
		if next == 0 {
			t.nodes = append(t.nodes, cidrNode{own: emptyRuleSet(), sub: emptyRuleSet()})
			next = int32(len(t.nodes) - 1)
			t.nodes[n].child[bit] = next
		}
		n = next
		t.nodes[n].sub.add(e, idx)
	}
	t.nodes[n].own.add(e, idx)
}

// walk follows the first bits of addr and returns the smallest index of a
// rule with effect e whose prefix contains that path, together with the
// node reached after exactly bits steps (-1 if the path leaves the trie).
func (t *cidrTrie) walk(addr netip.Addr, bits int, e RuleEffect) (best int32, end int32) {
	b := addrBytes(addr)
	n := int32(0)
	best = t.nodes[0].own.get(e)
	for i := 0; i < bits; i++ {
		n = t.nodes[n].child[addrBit(&b, i)]
		if n == 0 {
			return best, -1
		}
		best = min(best, t.nodes[n].own.get(e))
	}
	return best, n
}

// containing returns the smallest index of a rule with effect e whose
// prefix contains every address of p (p itself may be a single address).
func (t *cidrTrie) containing(p netip.Prefix, e RuleEffect) int32 {
	best, _ := t.walk(p.Addr(), p.Bits(), e)
	return best
}

// overlapping returns the smallest index of a rule with effect e whose
// prefix shares at least one address with p: an ancestor of p or any
// prefix inside p.
func (t *cidrTrie) overlapping(p netip.Prefix, e RuleEffect) int32 {
	best, end := t.walk(p.Addr(), p.Bits(), e)
	if end >= 0 {
		best = min(best, t.nodes[end].sub.get(e))
	}
	return best
}

// familyTries holds one trie per address family.
type familyTries struct {
	v4, v6 *cidrTrie
}

func newFamilyTries() familyTries { return familyTries{v4: newCIDRTrie(), v6: newCIDRTrie()} }

func (f familyTries) of(a netip.Addr) *cidrTrie {
	if a.Is4() {
		return f.v4
	}
	return f.v6
}

func (f familyTries) add(p netip.Prefix, e RuleEffect, idx int32) { f.of(p.Addr()).add(p, e, idx) }

func (f familyTries) containsAddr(a netip.Addr, e RuleEffect) int32 {
	return f.of(a).containing(netip.PrefixFrom(a, a.BitLen()), e)
}

func (f familyTries) containing(p netip.Prefix, e RuleEffect) int32 {
	return f.of(p.Addr()).containing(p, e)
}

func (f familyTries) overlapping(p netip.Prefix, e RuleEffect) int32 {
	return f.of(p.Addr()).overlapping(p, e)
}
