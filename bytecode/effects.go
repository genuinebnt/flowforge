package bytecode

import (
	"fmt"
	"strings"
)

// EffectKind is the kind operand of the EFFECT instruction (spec 8.4).
type EffectKind uint8

// Effect kinds with the exact numbering of spec 8.4.
const (
	EffectToolCall        EffectKind = 1
	EffectScopeCheck      EffectKind = 2
	EffectApproval        EffectKind = 3
	EffectSleep           EffectKind = 4
	EffectAgentCall       EffectKind = 5
	EffectExecutePlan     EffectKind = 6
	EffectEmitAlert       EffectKind = 7
	EffectNow             EffectKind = 8
	EffectLog             EffectKind = 9
	EffectParMap          EffectKind = 10
	EffectEmbed           EffectKind = 11
	EffectSimilarityQuery EffectKind = 12
)

var effectNames = [...]string{
	EffectToolCall:        "tool_call",
	EffectScopeCheck:      "scope_check",
	EffectApproval:        "approval",
	EffectSleep:           "sleep",
	EffectAgentCall:       "agent_call",
	EffectExecutePlan:     "execute_plan",
	EffectEmitAlert:       "emit_alert",
	EffectNow:             "now",
	EffectLog:             "log",
	EffectParMap:          "par_map",
	EffectEmbed:           "embed",
	EffectSimilarityQuery: "similarity_query",
}

// Valid reports whether k is a known effect kind.
func (k EffectKind) Valid() bool {
	return int(k) < len(effectNames) && effectNames[k] != ""
}

// String returns the spec 8.4 name ("tool_call", ...), or "effect(N)".
func (k EffectKind) String() string {
	if k.Valid() {
		return effectNames[k]
	}
	return fmt.Sprintf("effect(%d)", uint8(k))
}

// ParseEffectKind maps the lower snake case name back to the kind.
func ParseEffectKind(s string) (EffectKind, bool) {
	for i, n := range effectNames {
		if n != "" && n == s {
			return EffectKind(i), true
		}
	}
	return 0, false
}

// Argc returns the fixed argument count of an effect kind, or -1 when the
// count is variable (tool_call, agent_call, embed, similarity_query). The
// verifier enforces fixed counts.
//
// Argument layouts (bottom of the stack first):
//
//	tool_call         inputs..., tool_name      (tool name is a CONST String naming a TOOLS entry)
//	scope_check       target
//	approval          role, reason, timeout
//	sleep             duration
//	execute_plan      plan
//	emit_alert        severity, message
//	now               (none)
//	log               level, message
//	par_map           list, closure, limit, on_error (Int: 0 = fail, 1 = collect)
//	agent_call, embed, similarity_query: engine-defined, at least one argument
func (k EffectKind) Argc() int {
	switch k {
	case EffectScopeCheck, EffectSleep, EffectExecutePlan:
		return 1
	case EffectApproval:
		return 3
	case EffectEmitAlert, EffectLog:
		return 2
	case EffectNow:
		return 0
	case EffectParMap:
		return 4
	}
	return -1
}

// ParMap on_error modes (fourth argument of EFFECT par_map).
const (
	ParMapFail    = 0
	ParMapCollect = 1
)

// EffectMask is the declared effect set of a workflow (EFFECTS section).
// Bits 0-7 are the DSL effects of spec 7.5 in their documented order; bits
// 8-11 are engine effects that the DSL does not name but the verifier must
// still account for.
type EffectMask uint16

// Effect bits.
const (
	EffRead           EffectMask = 1 << 0
	EffPassive        EffectMask = 1 << 1
	EffActive         EffectMask = 1 << 2
	EffIntrusive      EffectMask = 1 << 3
	EffWrite          EffectMask = 1 << 4
	EffFinancialWrite EffectMask = 1 << 5
	EffLLM            EffectMask = 1 << 6
	EffTime           EffectMask = 1 << 7
	EffScope          EffectMask = 1 << 8  // scope_check
	EffApproval       EffectMask = 1 << 9  // approval
	EffAlert          EffectMask = 1 << 10 // emit_alert, log
	EffPlan           EffectMask = 1 << 11 // execute_plan

	// EffAll is every defined bit; other bits are rejected by the verifier.
	EffAll EffectMask = 1<<12 - 1
)

var maskNames = [...]string{
	"read", "passive", "active", "intrusive", "write", "financial_write",
	"llm", "time", "scope", "approval", "alert", "plan",
}

// ParseEffect returns the bit for one effect name of spec 7.5 (or one of
// the engine effect names scope, approval, alert, plan).
func ParseEffect(name string) (EffectMask, bool) {
	for i, n := range maskNames {
		if n == name {
			return 1 << i, true
		}
	}
	return 0, false
}

// String lists the set bits by name in bit order, comma-separated inside
// braces ("{read,active}"), with any unknown bits appended as 0xNNNN.
func (m EffectMask) String() string {
	var parts []string
	for i, n := range maskNames {
		if m&(1<<i) != 0 {
			parts = append(parts, n)
		}
	}
	if rest := m &^ EffAll; rest != 0 {
		parts = append(parts, fmt.Sprintf("0x%04x", uint16(rest)))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// Mask returns the effect bits an EFFECT instruction of kind k contributes
// on its own. tool_call contributes the tool's declared effects (from the
// ToolResolver) instead, and par_map contributes nothing itself: its
// closure's effects are counted because the closure's function is
// reachable through the CLOSURE instruction that built it.
func (k EffectKind) Mask() EffectMask {
	switch k {
	case EffectScopeCheck:
		return EffScope
	case EffectApproval:
		return EffApproval
	case EffectSleep, EffectNow:
		return EffTime
	case EffectAgentCall, EffectEmbed:
		return EffLLM
	case EffectExecutePlan:
		return EffPlan
	case EffectEmitAlert, EffectLog:
		return EffAlert
	case EffectSimilarityQuery:
		return EffRead
	}
	return 0
}
