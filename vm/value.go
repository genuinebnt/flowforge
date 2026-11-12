package vm

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core/netnorm"
)

// Kind is the run-time tag of a Value (spec 8.2).
type Kind uint8

// Value kinds. The numbering is used by the snapshot and argument
// encodings and never changes.
const (
	KindUnit       Kind = 0
	KindBool       Kind = 1
	KindInt        Kind = 2
	KindDuration   Kind = 3
	KindTime       Kind = 4
	KindFloat      Kind = 5
	KindString     Kind = 6
	KindList       Kind = 7 // also Stream (flag)
	KindMap        Kind = 8
	KindRecord     Kind = 9
	KindEnum       Kind = 10
	KindClosure    Kind = 11
	KindFqdn       Kind = 12
	KindIPAddress  Kind = 13
	KindNetblock   Kind = 14
	KindAsn        Kind = 15
	KindURL        Kind = 16
	KindScopeProof Kind = 17
	KindEntityRef  Kind = 18

	numKinds = 19
)

var kindNames = [numKinds]string{
	"Unit", "Bool", "Int", "Duration", "Time", "Float", "String", "List", "Map",
	"Record", "Enum", "Closure", "Fqdn", "IpAddress", "Netblock", "Asn", "Url",
	"ScopeProof", "EntityRef",
}

func (k Kind) String() string {
	if k < numKinds {
		return kindNames[k]
	}
	return fmt.Sprintf("kind(%d)", uint8(k))
}

// isStringy reports kinds whose payload is a *strObj.
func (k Kind) isStringy() bool {
	return k == KindString || (k >= KindFqdn && k <= KindURL) || k == KindEntityRef
}

// IsDomain reports whether k is one of the domain scalar kinds.
func (k Kind) IsDomain() bool { return k >= KindFqdn && k <= KindURL }

const flagStream uint8 = 1

// Value is an immutable VM value. Scalars live inline (no allocation);
// strings, lists, maps, records, enum payloads, closure captures and scope
// proofs point at shared immutable heap objects. The zero Value is Unit.
type Value struct {
	k Kind
	f uint8
	n uint64 // bool, int bits, float bits, list length, type id / variant, function index
	p any    // *strObj, *listObj, *mapObj, *tupObj, *proofObj or nil
}

type strObj struct {
	s     string
	konst int32 // constant index + 1 for constant-pool strings, else 0
}

// listObj is the backing store of one or more lists. A list value is a
// prefix of the backing (Value.n elements). Appending to a list whose
// length equals the backing length extends the backing in place, which is
// safe because no other value can observe elements past its own length;
// appending to a shorter prefix copies.
type listObj struct{ elems []Value }

type mapObj struct {
	keys  []Value
	vals  []Value
	index map[string]int
}

type tupObj struct{ vals []Value }

type proofObj struct {
	target   Value
	decision string
}

// Constructors.

// Unit returns the unit value.
func Unit() Value { return Value{} }

// Bool returns a Bool.
func Bool(b bool) Value {
	if b {
		return Value{k: KindBool, n: 1}
	}
	return Value{k: KindBool}
}

// Int returns an Int.
func Int(i int64) Value { return Value{k: KindInt, n: uint64(i)} }

// Duration returns a Duration of ns nanoseconds.
func Duration(ns int64) Value { return Value{k: KindDuration, n: uint64(ns)} }

// Time returns a Time of ns Unix nanoseconds.
func Time(ns int64) Value { return Value{k: KindTime, n: uint64(ns)} }

// Float returns a Float. NaN and infinities are not VM values: a result
// carrying one is rejected when it enters the VM (Resume, Start).
func Float(f float64) Value { return Value{k: KindFloat, n: math.Float64bits(f)} }

// Str returns a String.
func Str(s string) Value { return Value{k: KindString, p: &strObj{s: s}} }

// List returns a list of copies of elems.
func List(elems ...Value) Value {
	return mkList(append([]Value(nil), elems...))
}

// Stream returns a list flagged as a stream.
func Stream(elems ...Value) Value {
	v := List(elems...)
	v.f = flagStream
	return v
}

func mkList(elems []Value) Value {
	return Value{k: KindList, n: uint64(len(elems)), p: &listObj{elems: elems}}
}

// Map builds a map from parallel key and value slices, in order. Duplicate
// keys are an error.
func Map(keys, vals []Value) (Value, error) {
	if len(keys) != len(vals) {
		return Value{}, fmt.Errorf("%w: %d keys, %d values", ErrBadValue, len(keys), len(vals))
	}
	m := &mapObj{keys: append([]Value(nil), keys...), vals: append([]Value(nil), vals...), index: make(map[string]int, len(keys))}
	for i, k := range m.keys {
		ks := keyString(k)
		if _, dup := m.index[ks]; dup {
			return Value{}, fmt.Errorf("%w: duplicate map key %s", ErrBadValue, Display(k))
		}
		m.index[ks] = i
	}
	return Value{k: KindMap, p: m}, nil
}

// Record returns a record of type t.
func Record(t bytecode.TypeID, fields ...Value) Value {
	return Value{k: KindRecord, n: uint64(t), p: &tupObj{vals: append([]Value(nil), fields...)}}
}

// Tuple returns a tuple (a record of the built-in tuple type).
func Tuple(vals ...Value) Value { return Record(bytecode.TypeTuple, vals...) }

// Enum returns variant v of enum type t with the given payload.
func Enum(t bytecode.TypeID, variant uint8, payload ...Value) Value {
	return mkEnum(t, variant, append([]Value(nil), payload...))
}

func mkEnum(t bytecode.TypeID, variant uint8, payload []Value) Value {
	v := Value{k: KindEnum, n: uint64(t)<<8 | uint64(variant)}
	if len(payload) > 0 {
		v.p = &tupObj{vals: payload}
	}
	return v
}

// Some returns Option::Some(v).
func Some(v Value) Value { return mkEnum(bytecode.TypeOption, bytecode.VariantSome, []Value{v}) }

// None returns Option::None.
func None() Value { return mkEnum(bytecode.TypeOption, bytecode.VariantNone, nil) }

// Ok returns Result::Ok(v).
func Ok(v Value) Value { return mkEnum(bytecode.TypeResult, bytecode.VariantOk, []Value{v}) }

// Err returns Result::Err(v).
func Err(v Value) Value { return mkEnum(bytecode.TypeResult, bytecode.VariantErr, []Value{v}) }

// EntityRef returns a reference to a graph entity.
func EntityRef(id string) Value { return Value{k: KindEntityRef, p: &strObj{s: id}} }

// ScopeProof returns the run-time form of InScope<T>: the checked target
// and the id of the recorded allow decision. Only the engine creates
// proofs, as the result of a scope_check effect.
func ScopeProof(target Value, decisionID string) Value {
	return Value{k: KindScopeProof, p: &proofObj{target: target, decision: decisionID}}
}

// Domain scalar constructors validate and canonicalize (spec 4.2).

// Fqdn validates a DNS name.
func Fqdn(s string) (Value, error) {
	c, err := netnorm.Hostname(s)
	if err != nil {
		return Value{}, err
	}
	return domain(KindFqdn, c), nil
}

// IPAddress validates an IP address.
func IPAddress(s string) (Value, error) {
	a, err := netnorm.IP(s)
	if err != nil {
		return Value{}, err
	}
	return domain(KindIPAddress, a.String()), nil
}

// Netblock validates a CIDR prefix (host bits are zeroed).
func Netblock(s string) (Value, error) {
	p, err := netnorm.CIDR(s)
	if err != nil {
		return Value{}, err
	}
	return domain(KindNetblock, p.String()), nil
}

// URL validates an http(s) URL.
func URL(s string) (Value, error) {
	u, err := netnorm.ParseURL(s)
	if err != nil {
		return Value{}, err
	}
	return domain(KindURL, u.String()), nil
}

// Asn validates an autonomous system number, "AS64500" or "64500"; the
// canonical form is "AS64500".
func Asn(s string) (Value, error) {
	t := strings.TrimSpace(s)
	if len(t) > 2 && (t[:2] == "AS" || t[:2] == "as" || t[:2] == "As" || t[:2] == "aS") {
		t = t[2:]
	}
	if t == "" || t[0] == '+' || t[0] == '-' || (len(t) > 1 && t[0] == '0') {
		return Value{}, fmt.Errorf("%w: invalid ASN %q", ErrBadValue, s)
	}
	n, err := strconv.ParseUint(t, 10, 32)
	if err != nil {
		return Value{}, fmt.Errorf("%w: invalid ASN %q", ErrBadValue, s)
	}
	return domain(KindAsn, "AS"+strconv.FormatUint(n, 10)), nil
}

// Host parses a host name or IP literal into the built-in Host enum.
func Host(s string) (Value, error) {
	h, err := netnorm.ParseHost(s)
	if err != nil {
		return Value{}, err
	}
	if h.IsIP() {
		return Enum(bytecode.TypeHost, bytecode.VariantIP, domain(KindIPAddress, h.IP.String())), nil
	}
	return Enum(bytecode.TypeHost, bytecode.VariantName, domain(KindFqdn, h.Name)), nil
}

func domain(k Kind, canonical string) Value { return Value{k: k, p: &strObj{s: canonical}} }

// canonicalDomain re-validates a domain scalar's text and returns its
// canonical form; values decoded from bytes must already be canonical.
func canonicalDomain(k Kind, s string) (string, error) {
	var v Value
	var err error
	switch k {
	case KindFqdn:
		v, err = Fqdn(s)
	case KindIPAddress:
		v, err = IPAddress(s)
	case KindNetblock:
		v, err = Netblock(s)
	case KindURL:
		v, err = URL(s)
	case KindAsn:
		v, err = Asn(s)
	default:
		return "", fmt.Errorf("%w: %s is not a domain kind", ErrBadValue, k)
	}
	if err != nil {
		return "", err
	}
	return v.Str(), nil
}

// Accessors. They return zero values for the wrong kind.

// Kind returns the value's kind.
func (v Value) Kind() Kind { return v.k }

// IsStream reports whether a list carries the stream flag.
func (v Value) IsStream() bool { return v.k == KindList && v.f&flagStream != 0 }

// AsBool returns a Bool's payload.
func (v Value) AsBool() bool { return v.k == KindBool && v.n != 0 }

// AsInt returns the payload of an Int, Duration or Time.
func (v Value) AsInt() int64 { return int64(v.n) }

// AsFloat returns a Float's payload.
func (v Value) AsFloat() float64 {
	if v.k != KindFloat {
		return 0
	}
	return math.Float64frombits(v.n)
}

// Str returns the text of a String, domain scalar or EntityRef.
func (v Value) Str() string {
	if s, ok := v.p.(*strObj); ok {
		return s.s
	}
	return ""
}

// Len returns the element count of a list or map, or the byte length of a
// string.
func (v Value) Len() int {
	switch v.k {
	case KindList:
		return int(v.n)
	case KindMap:
		return len(v.p.(*mapObj).keys)
	}
	if s, ok := v.p.(*strObj); ok {
		return len(s.s)
	}
	return 0
}

// Elems returns a list's elements. The slice is shared: callers must not
// modify it.
func (v Value) Elems() []Value {
	if v.k != KindList {
		return nil
	}
	l := v.p.(*listObj)
	return l.elems[:v.n:v.n]
}

// MapKeys returns a map's keys in insertion order (shared, read-only).
func (v Value) MapKeys() []Value {
	if v.k != KindMap {
		return nil
	}
	return v.p.(*mapObj).keys
}

// MapValues returns a map's values in insertion order (shared, read-only).
func (v Value) MapValues() []Value {
	if v.k != KindMap {
		return nil
	}
	return v.p.(*mapObj).vals
}

// MapGet looks a key up structurally.
func (v Value) MapGet(key Value) (Value, bool) {
	if v.k != KindMap {
		return Value{}, false
	}
	m := v.p.(*mapObj)
	i, ok := m.index[keyString(key)]
	if !ok {
		return Value{}, false
	}
	return m.vals[i], true
}

// TypeID returns the type id of a record or enum.
func (v Value) TypeID() bytecode.TypeID {
	switch v.k {
	case KindRecord:
		return bytecode.TypeID(v.n)
	case KindEnum:
		return bytecode.TypeID(v.n >> 8)
	}
	return 0
}

// Variant returns an enum's variant index.
func (v Value) Variant() uint8 {
	if v.k != KindEnum {
		return 0
	}
	return uint8(v.n)
}

// Fields returns a record's fields, an enum's payload or a closure's
// captures (shared, read-only).
func (v Value) Fields() []Value {
	if t, ok := v.p.(*tupObj); ok && t != nil {
		return t.vals
	}
	return nil
}

// FuncIndex returns a closure's function index.
func (v Value) FuncIndex() uint16 {
	if v.k != KindClosure {
		return 0
	}
	return uint16(v.n)
}

// ProofTarget returns the checked target of a ScopeProof.
func (v Value) ProofTarget() Value {
	if p, ok := v.p.(*proofObj); ok {
		return p.target
	}
	return Value{}
}

// ProofDecision returns the decision id of a ScopeProof.
func (v Value) ProofDecision() string {
	if p, ok := v.p.(*proofObj); ok {
		return p.decision
	}
	return ""
}

// IsSome reports whether v is Option::Some.
func (v Value) IsSome() bool {
	return v.k == KindEnum && v.TypeID() == bytecode.TypeOption && v.Variant() == bytecode.VariantSome
}

// IsOk reports whether v is Result::Ok.
func (v Value) IsOk() bool {
	return v.k == KindEnum && v.TypeID() == bytecode.TypeResult && v.Variant() == bytecode.VariantOk
}

// String renders the display form (see Display).
func (v Value) String() string { return Display(v) }
