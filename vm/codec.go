package vm

import (
	"crypto/sha256"
	"fmt"
	"math"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"

	"github.com/genuinebnt/flowforge/bytecode"
)

// Value encoding for effect arguments, recorded results and outputs that
// must be hashed or stored: deterministic CBOR (RFC 8949 section 4.2) of a
// purely structural tree, so equal values always encode to equal bytes no
// matter how they share memory.
//
//	Unit          [0]
//	Bool          [1, bool]
//	Int           [2, int]      Duration [3, int]   Time [4, int]
//	Float         [5, float]    (shortest exact float encoding)
//	String        [6, text]     Fqdn [12, text] IpAddress [13, text]
//	              Netblock [14, text] Asn [15, text] Url [16, text] EntityRef [18, text]
//	List/Stream   [7, stream 0|1, [elems...]]
//	Map           [8, [k1, v1, k2, v2, ...]]   (insertion order)
//	Record        [9, type_id, [fields...]]
//	Enum          [10, type_id, variant, [payload...]]
//	Closure       [11, fn, [captures...]]
//	ScopeProof    [17, target, decision_id]
//
// EncodeArgs encodes an argument list as a CBOR array of such trees.

var (
	encMode = mustEncMode()
	decMode = mustDecMode(1024)
)

func mustEncMode() cbor.EncMode {
	em, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic("vm: CBOR encoder options: " + err.Error()) // static options; programmer error
	}
	return em
}

func mustDecMode(levels int) cbor.DecMode {
	dm, err := cbor.DecOptions{
		DupMapKey:        cbor.DupMapKeyEnforcedAPF,
		MaxNestedLevels:  levels,
		MaxArrayElements: 1 << 26,
		MaxMapPairs:      16,
		IndefLength:      cbor.IndefLengthForbidden,
		TagsMd:           cbor.TagsForbidden,
		IntDec:           cbor.IntDecConvertNone,
		UTF8:             cbor.UTF8RejectInvalid,
		NaN:              cbor.NaNDecodeForbidden,
		Inf:              cbor.InfDecodeForbidden,
	}.DecMode()
	if err != nil {
		panic("vm: CBOR decoder options: " + err.Error()) // static options; programmer error
	}
	return dm
}

// maxEncodeNodes bounds the size of one structural encoding. Values can
// share subtrees, so the tree can be much larger than the heap.
const maxEncodeNodes = 1 << 23

type treeEncoder struct {
	nodes     int
	allowFunc bool
}

func (e *treeEncoder) tree(v Value) (any, error) {
	e.nodes++
	if e.nodes > maxEncodeNodes {
		return nil, fmt.Errorf("%w: value too large to encode", ErrEncode)
	}
	k := uint64(v.k)
	switch v.k {
	case KindUnit:
		return []any{k}, nil
	case KindBool:
		return []any{k, v.AsBool()}, nil
	case KindInt, KindDuration, KindTime:
		return []any{k, v.AsInt()}, nil
	case KindFloat:
		return []any{k, v.AsFloat()}, nil
	case KindList:
		elems, err := e.slice(v.Elems())
		if err != nil {
			return nil, err
		}
		return []any{k, uint64(v.f & flagStream), elems}, nil
	case KindMap:
		m := v.p.(*mapObj)
		flat := make([]any, 0, 2*len(m.keys))
		for i := range m.keys {
			kt, err := e.tree(m.keys[i])
			if err != nil {
				return nil, err
			}
			vt, err := e.tree(m.vals[i])
			if err != nil {
				return nil, err
			}
			flat = append(flat, kt, vt)
		}
		return []any{k, flat}, nil
	case KindRecord:
		fs, err := e.slice(v.Fields())
		if err != nil {
			return nil, err
		}
		return []any{k, uint64(v.TypeID()), fs}, nil
	case KindEnum:
		fs, err := e.slice(v.Fields())
		if err != nil {
			return nil, err
		}
		return []any{k, uint64(v.TypeID()), uint64(v.Variant()), fs}, nil
	case KindClosure:
		fs, err := e.slice(v.Fields())
		if err != nil {
			return nil, err
		}
		return []any{k, uint64(v.FuncIndex()), fs}, nil
	case KindScopeProof:
		t, err := e.tree(v.ProofTarget())
		if err != nil {
			return nil, err
		}
		return []any{k, t, v.ProofDecision()}, nil
	}
	if v.k.isStringy() {
		return []any{k, v.Str()}, nil
	}
	return nil, fmt.Errorf("%w: kind %d", ErrEncode, v.k)
}

func (e *treeEncoder) slice(vs []Value) ([]any, error) {
	out := make([]any, len(vs))
	for i, x := range vs {
		t, err := e.tree(x)
		if err != nil {
			return nil, err
		}
		out[i] = t
	}
	return out, nil
}

// EncodeValue returns the canonical encoding of v.
func EncodeValue(v Value) ([]byte, error) {
	e := treeEncoder{}
	t, err := e.tree(v)
	if err != nil {
		return nil, err
	}
	return encMode.Marshal(t)
}

// EncodeArgs returns the canonical encoding of an effect's arguments: a
// CBOR array of value trees. The engine records these bytes in
// effect_scheduled and hashes them (ArgsHash).
func EncodeArgs(args []Value) ([]byte, error) {
	e := treeEncoder{}
	t, err := e.slice(args)
	if err != nil {
		return nil, err
	}
	return encMode.Marshal(t)
}

// ArgsHash is SHA-256 of EncodeArgs(args); replay compares it to detect
// nondeterminism (spec 8.6).
func ArgsHash(args []Value) ([32]byte, error) {
	b, err := EncodeArgs(args)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// DecodeValue decodes an EncodeValue encoding and validates it against the
// module: known type ids with matching arity, closures of existing
// functions with the right capture count, canonical domain scalars,
// finite floats and valid UTF-8.
func DecodeValue(prog *bytecode.Verified, b []byte) (Value, error) {
	var t any
	if err := decMode.Unmarshal(b, &t); err != nil {
		return Value{}, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	d := treeDecoder{mod: prog.Module()}
	return d.value(t, 0)
}

// DecodeArgs decodes an EncodeArgs encoding.
func DecodeArgs(prog *bytecode.Verified, b []byte) ([]Value, error) {
	var t any
	if err := decMode.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDecode, err)
	}
	arr, ok := t.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: arguments are not an array", ErrDecode)
	}
	d := treeDecoder{mod: prog.Module()}
	return d.slice(arr, 0)
}

type treeDecoder struct {
	mod *bytecode.Module
}

func decErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrDecode, fmt.Sprintf(format, args...))
}

func asUint(x any, max uint64) (uint64, bool) {
	u, ok := x.(uint64)
	return u, ok && u <= max
}

func asInt(x any) (int64, bool) {
	switch n := x.(type) {
	case uint64:
		if n <= math.MaxInt64 {
			return int64(n), true
		}
	case int64:
		return n, true
	}
	return 0, false
}

func asText(x any) (string, bool) {
	s, ok := x.(string)
	return s, ok && utf8.ValidString(s)
}

func (d *treeDecoder) slice(xs []any, depth int) ([]Value, error) {
	if len(xs) == 0 {
		return nil, nil
	}
	out := make([]Value, len(xs))
	for i, x := range xs {
		v, err := d.value(x, depth+1)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

func (d *treeDecoder) value(x any, depth int) (Value, error) {
	if depth > maxValueDepth {
		return Value{}, decErr("nested too deeply")
	}
	arr, ok := x.([]any)
	if !ok || len(arr) == 0 {
		return Value{}, decErr("value is not a tagged array")
	}
	kn, ok := asUint(arr[0], numKinds-1)
	if !ok {
		return Value{}, decErr("bad kind")
	}
	k := Kind(kn)
	if len(arr) != treeLen(k) {
		return Value{}, decErr("%s with %d fields", k, len(arr))
	}
	switch k {
	case KindUnit:
		return Value{}, nil
	case KindBool:
		b, ok := arr[1].(bool)
		if !ok {
			return Value{}, decErr("bad Bool")
		}
		return Bool(b), nil
	case KindInt, KindDuration, KindTime:
		i, ok := asInt(arr[1])
		if !ok {
			return Value{}, decErr("bad integer")
		}
		return Value{k: k, n: uint64(i)}, nil
	case KindFloat:
		f, ok := arr[1].(float64)
		if !ok || math.IsNaN(f) || math.IsInf(f, 0) {
			return Value{}, decErr("bad Float")
		}
		return Float(f), nil
	case KindList:
		flag, ok := asUint(arr[1], 1)
		xs, ok2 := arr[2].([]any)
		if !ok || !ok2 || len(xs) > DefaultList {
			return Value{}, decErr("bad List")
		}
		elems, err := d.slice(xs, depth)
		if err != nil {
			return Value{}, err
		}
		v := mkList(elems)
		v.f = uint8(flag)
		return v, nil
	case KindMap:
		xs, ok := arr[1].([]any)
		if !ok || len(xs)%2 != 0 || len(xs) > 2*DefaultList {
			return Value{}, decErr("bad Map")
		}
		flat, err := d.slice(xs, depth)
		if err != nil {
			return Value{}, err
		}
		keys := make([]Value, len(flat)/2)
		vals := make([]Value, len(flat)/2)
		for i := range keys {
			keys[i], vals[i] = flat[2*i], flat[2*i+1]
		}
		v, err := Map(keys, vals)
		if err != nil {
			return Value{}, fmt.Errorf("%w: %v", ErrDecode, err)
		}
		return v, nil
	case KindRecord, KindEnum, KindClosure:
		id, ok := asUint(arr[1], math.MaxUint16)
		xs, ok2 := arr[len(arr)-1].([]any)
		if !ok || !ok2 {
			return Value{}, decErr("bad %s", k)
		}
		var variant uint64
		if k == KindEnum {
			variant, ok = asUint(arr[2], math.MaxUint8)
			if !ok {
				return Value{}, decErr("bad variant")
			}
		}
		if err := checkShape(d.mod, k, id, variant, len(xs)); err != nil {
			return Value{}, err
		}
		fs, err := d.slice(xs, depth)
		if err != nil {
			return Value{}, err
		}
		return buildCompound(k, id, variant, fs), nil
	case KindScopeProof:
		t, err := d.value(arr[1], depth+1)
		if err != nil {
			return Value{}, err
		}
		dec, ok := asText(arr[2])
		if !ok {
			return Value{}, decErr("bad decision id")
		}
		return ScopeProof(t, dec), nil
	}
	s, ok := asText(arr[1])
	if !ok || len(s) > DefaultString {
		return Value{}, decErr("bad %s text", k)
	}
	return textValue(k, s)
}

// treeLen is the array length of a tagged value tree of kind k.
func treeLen(k Kind) int {
	switch k {
	case KindUnit:
		return 1
	case KindList, KindRecord, KindClosure, KindScopeProof:
		return 3
	case KindEnum:
		return 4
	}
	return 2
}

// checkShape validates a record, enum or closure header against the module.
func checkShape(m *bytecode.Module, k Kind, id, variant uint64, n int) error {
	switch k {
	case KindClosure:
		if id >= uint64(len(m.Funcs)) || int(m.Funcs[id].NCaps) != n {
			return decErr("closure does not match the module")
		}
		return nil
	}
	l, ok := m.Layout(bytecode.TypeID(id))
	if !ok {
		return decErr("unknown type id %d", id)
	}
	if k == KindRecord {
		if l.Kind != bytecode.KindRecord || (!l.Variadic && len(l.Fields) != n) {
			return decErr("record does not match type %s", l.Name)
		}
		return nil
	}
	if l.Kind != bytecode.KindEnum || l.Arity(int(variant)) != n {
		return decErr("enum does not match type %s", l.Name)
	}
	return nil
}

func buildCompound(k Kind, id, variant uint64, fs []Value) Value {
	switch k {
	case KindRecord:
		return Value{k: KindRecord, n: id, p: &tupObj{vals: fs}}
	case KindEnum:
		return mkEnum(bytecode.TypeID(id), uint8(variant), fs)
	}
	v := Value{k: KindClosure, n: id}
	if len(fs) > 0 {
		v.p = &tupObj{vals: fs}
	}
	return v
}

// textValue builds a String, EntityRef or domain scalar; domain text must
// already be canonical.
func textValue(k Kind, s string) (Value, error) {
	switch {
	case k == KindString || k == KindEntityRef:
		return Value{k: k, p: &strObj{s: s}}, nil
	case k.IsDomain():
		c, err := canonicalDomain(k, s)
		if err != nil || c != s {
			return Value{}, decErr("%s %q is not canonical", k, s)
		}
		return domain(k, s), nil
	}
	return Value{}, decErr("kind %d has no text", k)
}
