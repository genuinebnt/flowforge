package vm

import (
	"fmt"

	"github.com/genuinebnt/flowforge/bytecode"
)

// Outcome is the result of Run or Resume: exactly one of Suspended,
// Completed, Failed or OutOfFuel (spec 8.1).
type Outcome interface{ isOutcome() }

// Suspended reports an EFFECT instruction. The engine performs (or
// replays) the effect and calls Resume with its result.
type Suspended struct {
	// Path is the hierarchical effect id; Path[len(Path)-1] is Seq.
	Path Path
	// Seq is this VM's effect counter value for the effect.
	Seq  uint64
	Kind bytecode.EffectKind
	// Args are the effect arguments in push order, except that for
	// tool_call the tool name (pushed last) is moved to Args[0], so
	// Args = [tool_name, inputs...].
	Args []Value
}

// Tool returns the tool name of a tool_call effect.
func (s Suspended) Tool() string {
	if s.Kind != bytecode.EffectToolCall || len(s.Args) == 0 {
		return ""
	}
	return s.Args[0].Str()
}

// Completed reports that the run (or child) finished with a value.
type Completed struct{ Value Value }

// Failed reports a run failure with a code from the failure table.
type Failed struct {
	Code    bytecode.FailCode
	Message string
	Span    Span
}

func (f Failed) Error() string { return fmt.Sprintf("%s: %s", f.Code, f.Message) }

// OutOfFuel reports that the fuel slice given to Run or Resume ran out.
// The VM can be run again with more fuel; it continues exactly where it
// stopped.
type OutOfFuel struct{}

func (Suspended) isOutcome() {}
func (Completed) isOutcome() {}
func (Failed) isOutcome()    {}
func (OutOfFuel) isOutcome() {}

// Span locates the failing instruction: the function, the code offset and
// the source range of the closest span table entry at or before it
// (HasSource false when the function has none).
type Span struct {
	Func         int
	CodeOffset   int
	SourceOffset uint32
	Len          uint32
	HasSource    bool
}

// EffectResult is what the engine feeds back into a suspended VM: either a
// value (Ok) or a failure (the effect failed permanently).
type EffectResult struct {
	Value Value
	// Failed marks an effect failure. The run then fails with Code
	// (effect_failed when zero) and Message. Domain-level errors such as a
	// scope denial or an approval denial are not failures: they are Ok
	// values of type Result.
	Failed  bool
	Code    bytecode.FailCode
	Message string
}

// OkResult wraps a successful effect result.
func OkResult(v Value) EffectResult { return EffectResult{Value: v} }

// FailResult wraps an effect failure.
func FailResult(code bytecode.FailCode, msg string) EffectResult {
	return EffectResult{Failed: true, Code: code, Message: msg}
}
