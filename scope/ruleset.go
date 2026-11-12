package scope

import "math"

// noRule is the "no match" rule index. Using the largest index lets
// matchers combine candidates with a plain min, which keeps the reported
// rule the first matching one in document order.
const noRule int32 = math.MaxInt32

// ruleSet holds, per effect, the smallest index of a rule attached to an
// index node.
type ruleSet [2]int32

func emptyRuleSet() ruleSet { return ruleSet{noRule, noRule} }

// slot maps an effect to its ruleSet position. Only EffectAllow and
// EffectDeny reach the index (Validate rejects anything else).
func slot(e RuleEffect) int {
	if e == EffectDeny {
		return 1
	}
	return 0
}

func (s *ruleSet) add(e RuleEffect, idx int32) { s[slot(e)] = min(s[slot(e)], idx) }

func (s ruleSet) get(e RuleEffect) int32 { return s[slot(e)] }
