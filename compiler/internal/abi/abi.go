// Package abi fixes how checked DSL values are represented as VM values:
// which bytecode type id a DSL record or enum gets, which variant index a
// DSL variant gets, and which native builtin implements a pure DSL
// library function. The compiler and the tree-walking evaluator both use
// it, so the two agree by construction on value layout and differ only in
// how they evaluate.
package abi

import (
	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/dsl"
)

// reserved maps the DSL standard-library types that have a fixed bytecode
// type id (bytecode/types.go) to that id. Their field layouts agree with
// the DSL prelude.
var reserved = map[string]bytecode.TypeID{
	"Option":      bytecode.TypeOption,
	"Result":      bytecode.TypeResult,
	"Host":        bytecode.TypeHost,
	"Severity":    bytecode.TypeSeverity,
	"ScopeDenied": bytecode.TypeScopeDenied,
	"Approval":    bytecode.TypeApproval,
	"Denied":      bytecode.TypeDenied,
	"PlanError":   bytecode.TypePlanError,
	"TaskError":   bytecode.TypeTaskError,
	"Budget":      bytecode.TypeBudget,
}

// Types maps the record and enum definitions of a checked program to
// bytecode type ids.
type Types struct {
	ids map[*dsl.TypeDef]bytecode.TypeID
	// Module lists the definitions that need a TYPES entry, in type id
	// order (id FirstUserType + index): standard-library records without a
	// reserved id, then the program's records and enums in declaration
	// order.
	Module []*dsl.TypeDef
}

// NewTypes assigns ids to every record and enum of prog.
func NewTypes(prog *dsl.Program) *Types {
	t := &Types{ids: map[*dsl.TypeDef]bytecode.TypeID{}}
	builtin := map[*dsl.TypeDef]bool{}
	for _, d := range dsl.BuiltinTypes() {
		builtin[d] = true
	}
	for _, d := range prog.Types {
		if d.Kind != dsl.DefRecord && d.Kind != dsl.DefEnum {
			continue
		}
		if builtin[d] {
			if id, ok := reserved[d.Name]; ok {
				t.ids[d] = id
				continue
			}
		}
		t.ids[d] = bytecode.FirstUserType + bytecode.TypeID(len(t.Module))
		t.Module = append(t.Module, d)
	}
	return t
}

// ID returns the bytecode type id of a record or enum definition.
func (t *Types) ID(d *dsl.TypeDef) (bytecode.TypeID, bool) {
	id, ok := t.ids[d]
	return id, ok
}

// IsOption reports whether d is the standard-library Option.
func IsOption(d *dsl.TypeDef) bool { return d != nil && d.Builtin && d.Name == "Option" }

// IsDenied reports whether t is the standard-library Denied record, the
// error type of approve.
func IsDenied(t dsl.Type) bool {
	n, ok := t.(*dsl.NamedType)
	return ok && n.Def.Builtin && n.Def.Name == "Denied"
}

// VariantIndex returns the bytecode variant index of v. The DSL declares
// Option as Some | None while the VM numbers None 0 and Some 1; every
// other enum keeps its declaration order.
func VariantIndex(v *dsl.Variant) uint8 {
	if IsOption(v.Def) {
		if v.Name == "Some" {
			return bytecode.VariantSome
		}
		return bytecode.VariantNone
	}
	return uint8(v.Index)
}

// OkVariant returns the bytecode index of the success variant of an
// Option (Some) or Result (Ok) operand of `?`.
func OkVariant(onOption bool) uint8 {
	if onOption {
		return bytecode.VariantSome
	}
	return bytecode.VariantOk
}

// TypeText renders t as the DSL type text stored in TYPES (the vm parses
// it to convert JSON).
func TypeText(t dsl.Type) string {
	if t == nil {
		return ""
	}
	if p, ok := t.(*dsl.PrimType); ok && p.Kind == dsl.PrimUnit {
		return "()"
	}
	return t.String()
}

// leaf maps pure DSL library functions to the native builtin that
// implements them with the same argument order.
var leaf = map[string]bytecode.BuiltinID{
	"len":           bytecode.BLen,
	"contains":      bytecode.BContains,
	"starts_with":   bytecode.BStartsWith,
	"ends_with":     bytecode.BEndsWith,
	"split":         bytecode.BSplit,
	"join":          bytecode.BJoin,
	"trim":          bytecode.BTrim,
	"lower":         bytecode.BLower,
	"upper":         bytecode.BUpper,
	"replace":       bytecode.BReplace,
	"parse_int":     bytecode.BParseInt,
	"parse_float":   bytecode.BParseFloat,
	"base64_decode": bytecode.BBase64Decode,
	"base64_encode": bytecode.BBase64Encode,
	"url_decode":    bytecode.BURLDecode,
	"url_encode":    bytecode.BURLEncode,
	"hex_decode":    bytecode.BHexDecode,
	"hex_encode":    bytecode.BHexEncode,
	"to_string":     bytecode.BToString,
	"take":          bytecode.BTake,
	"skip":          bytecode.BSkip,
	"dedupe":        bytecode.BDedupe,
	"zip":           bytecode.BZip,
	"enumerate":     bytecode.BEnumerate,
	"reverse":       bytecode.BReverse,
	"concat":        bytecode.BConcat,
	"get":           bytecode.BMapGet,
	"keys":          bytecode.BMapKeys,
	"values":        bytecode.BMapValues,
	"scope.allows":  bytecode.BScopeAllows,
}

// LeafBuiltin returns the native builtin implementing the pure library
// function name, when it maps one to one.
func LeafBuiltin(name string) (bytecode.BuiltinID, bool) {
	id, ok := leaf[name]
	return id, ok
}

// EffectKind returns the EFFECT kind of an effectful library function.
func EffectKind(b *dsl.Builtin) (bytecode.EffectKind, bool) {
	if b == nil || b.Op == dsl.OpNone {
		return 0, false
	}
	return bytecode.EffectKind(b.Op), true
}

// DomainParser returns the native parser of a domain scalar (the lowering
// of `s as Fqdn` and friends).
func DomainParser(d *dsl.TypeDef) (bytecode.BuiltinID, bool) {
	if d == nil || !d.Builtin {
		return 0, false
	}
	switch d.Name {
	case "Fqdn":
		return bytecode.BFqdnParse, true
	case "IpAddress":
		return bytecode.BIPParse, true
	case "Url":
		return bytecode.BURLParse, true
	case "Netblock":
		return bytecode.BCIDRParse, true
	case "Asn":
		return bytecode.BASNParse, true
	}
	return 0, false
}

// ParMapMode returns the bytecode on_error operand for the DSL literal.
func ParMapMode(s string) int64 {
	if s == "collect" {
		return bytecode.ParMapCollect
	}
	return bytecode.ParMapFail
}

// ApprovalDefaultTimeout is the timeout argument of an approval effect
// whose source omits it: zero means "the engine's default".
const ApprovalDefaultTimeout int64 = 0

// ClosureKind says what a closure's function implements.
type ClosureKind int

// Closure kinds.
const (
	// ClosureLambda is a lambda; captures are Lambda.Captures in order.
	ClosureLambda ClosureKind = iota
	// ClosureCompensation is a compensation: zero arguments, captures are
	// `it` followed by Lambda.Captures.
	ClosureCompensation
	// ClosureFn is a user fn used as a value (no captures).
	ClosureFn
	// ClosureTool, ClosureBuiltin and ClosureVariant wrap a tool, a library
	// function or a variant constructor used as a value (no captures).
	ClosureTool
	ClosureBuiltin
	ClosureVariant
)

// Closures maps the functions a compiled module uses as closure targets
// back to their source. The tree-walker uses it to build closure values
// identical to the VM's.
type Closures interface {
	LambdaFunc(l *dsl.Lambda) (uint16, bool)
	FnFunc(fn *dsl.FnDecl) (uint16, bool)
	ToolFunc(t *dsl.Tool) (uint16, bool)
	BuiltinFunc(b *dsl.Builtin) (uint16, bool)
	VariantFunc(v *dsl.Variant) (uint16, bool)
}
