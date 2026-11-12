package scope

import "strings"

// domainTrie indexes domain and wildcard_domain rules by reversed labels
// ("api.acme.com" is stored as com -> acme -> api). A lookup walks one
// node per label of the target, so its cost depends on the name's depth,
// not on the number of rules.
type domainTrie struct {
	root domainNode
}

type domainNode struct {
	children map[string]*domainNode
	exact    ruleSet // domain rules naming exactly this node
	below    ruleSet // wildcard rules "*.<this node>"
}

func newDomainTrie() *domainTrie {
	return &domainTrie{root: domainNode{exact: emptyRuleSet(), below: emptyRuleSet()}}
}

// node returns the node for name, creating the path as needed.
func (t *domainTrie) node(name string) *domainNode {
	n := &t.root
	for end := len(name); end > 0; {
		i := strings.LastIndexByte(name[:end], '.')
		label := name[i+1 : end]
		next := n.children[label]
		if next == nil {
			if n.children == nil {
				n.children = make(map[string]*domainNode)
			}
			next = &domainNode{exact: emptyRuleSet(), below: emptyRuleSet()}
			n.children[label] = next
		}
		n = next
		end = max(i, 0)
	}
	return n
}

func (t *domainTrie) addExact(name string, e RuleEffect, idx int32) { t.node(name).exact.add(e, idx) }

func (t *domainTrie) addBelow(name string, e RuleEffect, idx int32) { t.node(name).below.add(e, idx) }

// lookup returns the smallest index of a rule with effect e matching the
// normalized name: an exact rule for the name itself, or a wildcard rule
// for any strict ancestor.
func (t *domainTrie) lookup(name string, e RuleEffect) int32 {
	best := noRule
	n := &t.root
	for end := len(name); end > 0; {
		i := strings.LastIndexByte(name[:end], '.')
		n = n.children[name[i+1:end]]
		if n == nil {
			return best
		}
		if i < 0 {
			return min(best, n.exact.get(e))
		}
		best = min(best, n.below.get(e))
		end = i
	}
	return best
}
