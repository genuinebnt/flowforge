package bytecode

// TypeID identifies a record or enum layout. Ids below FirstUserType are
// reserved for the built-in layouts below; TYPES entry i of a module has id
// FirstUserType + i.
type TypeID uint16

// Reserved type ids. Option and Result are the enums of spec 8.2 with fixed
// ids; the others are standard-library types the engine constructs as
// effect results (spec 7.4, 7.6). Ids 13-31 are reserved for future
// built-ins and are rejected today.
const (
	TypeOption      TypeID = 0  // enum None | Some(T)
	TypeResult      TypeID = 1  // enum Ok(T) | Err(E)
	TypeHost        TypeID = 2  // enum Name(Fqdn) | Ip(IpAddress)
	TypeSeverity    TypeID = 3  // enum Info | Low | Medium | High | Critical
	TypeTuple       TypeID = 4  // record with any number of positional fields
	TypeScopeDenied TypeID = 5  // record { target, reason, rule_id }
	TypeApproval    TypeID = 6  // record { approval_id, actor, comment }
	TypeDenied      TypeID = 7  // record { approval_id, actor, reason }
	TypePlanError   TypeID = 8  // record { code, message }
	TypeTaskError   TypeID = 9  // record { code, message } (par_map collect)
	TypeLogLevel    TypeID = 10 // enum Debug | Info | Warn | Error
	TypeBudget      TypeID = 11 // record { tokens, tool_calls, iterations }
	TypePlan        TypeID = 12 // record { plan_id, module_sha256 }

	FirstUserType TypeID = 32
)

// Variant indices of the fixed enums.
const (
	VariantNone uint8 = 0
	VariantSome uint8 = 1
	VariantOk   uint8 = 0
	VariantErr  uint8 = 1
	VariantName uint8 = 0 // Host::Name
	VariantIP   uint8 = 1 // Host::Ip
)

// TypeKind distinguishes the layouts in the TYPES section.
type TypeKind uint8

// Layout kinds.
const (
	KindRecord TypeKind = 0
	KindEnum   TypeKind = 1
	// KindSignature describes a function's parameters and result type; the
	// compiler emits one for the entry function so that the engine can
	// decode workflow inputs and encode outputs. It is not a value type and
	// MAKE_REC / MAKE_ENUM reject it.
	KindSignature TypeKind = 2
)

// FieldLayout is a resolved record field or signature parameter.
type FieldLayout struct {
	Name string
	Type string // DSL type text, "" when unknown
}

// VariantLayout is a resolved enum variant.
type VariantLayout struct {
	Name    string
	Payload []string // DSL type text of each payload value; len = arity
}

// TypeLayout is a record, enum or signature layout with names resolved to
// strings. Built-in layouts and module TYPES entries share this form.
type TypeLayout struct {
	ID       TypeID
	Kind     TypeKind
	Name     string
	Fields   []FieldLayout   // records and signatures
	Variants []VariantLayout // enums
	// Variadic is set for the tuple type: MAKE_REC accepts any field count.
	Variadic bool
	// Func and Result are set for signatures.
	Func   uint16
	Result string
}

func rec(id TypeID, name string, fields ...string) *TypeLayout {
	l := &TypeLayout{ID: id, Kind: KindRecord, Name: name}
	for i := 0; i+1 < len(fields); i += 2 {
		l.Fields = append(l.Fields, FieldLayout{Name: fields[i], Type: fields[i+1]})
	}
	return l
}

func enum(id TypeID, name string, variants ...VariantLayout) *TypeLayout {
	return &TypeLayout{ID: id, Kind: KindEnum, Name: name, Variants: variants}
}

func v(name string, payload ...string) VariantLayout {
	return VariantLayout{Name: name, Payload: payload}
}

var builtinTypes = [FirstUserType]*TypeLayout{
	TypeOption:      enum(TypeOption, "Option", v("None"), v("Some", "T")),
	TypeResult:      enum(TypeResult, "Result", v("Ok", "T"), v("Err", "E")),
	TypeHost:        enum(TypeHost, "Host", v("Name", "Fqdn"), v("Ip", "IpAddress")),
	TypeSeverity:    enum(TypeSeverity, "Severity", v("Info"), v("Low"), v("Medium"), v("High"), v("Critical")),
	TypeTuple:       {ID: TypeTuple, Kind: KindRecord, Name: "Tuple", Variadic: true},
	TypeScopeDenied: rec(TypeScopeDenied, "ScopeDenied", "target", "String", "reason", "String", "rule_id", "Option<String>"),
	TypeApproval:    rec(TypeApproval, "Approval", "approval_id", "String", "actor", "String", "comment", "String"),
	TypeDenied:      rec(TypeDenied, "Denied", "approval_id", "String", "actor", "String", "reason", "String"),
	TypePlanError:   rec(TypePlanError, "PlanError", "code", "String", "message", "String"),
	TypeTaskError:   rec(TypeTaskError, "TaskError", "code", "String", "message", "String"),
	TypeLogLevel:    enum(TypeLogLevel, "LogLevel", v("Debug"), v("Info"), v("Warn"), v("Error")),
	TypeBudget:      rec(TypeBudget, "Budget", "tokens", "Int", "tool_calls", "Int", "iterations", "Int"),
	TypePlan:        rec(TypePlan, "Plan", "plan_id", "String", "module_sha256", "String"),
}

// BuiltinType returns the layout of a reserved type id.
func BuiltinType(id TypeID) (*TypeLayout, bool) {
	if id < FirstUserType && builtinTypes[id] != nil {
		return builtinTypes[id], true
	}
	return nil, false
}

// BuiltinTypeByName looks a reserved layout up by name.
func BuiltinTypeByName(name string) (*TypeLayout, bool) {
	for _, l := range builtinTypes {
		if l != nil && l.Name == name {
			return l, true
		}
	}
	return nil, false
}

// Arity returns the payload arity of variant v, or -1 if v does not exist.
func (l *TypeLayout) Arity(variant int) int {
	if l.Kind != KindEnum || variant < 0 || variant >= len(l.Variants) {
		return -1
	}
	return len(l.Variants[variant].Payload)
}
