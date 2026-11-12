package vm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
)

// TypeKind classifies a parsed DSL type.
type TypeKind uint8

// Type kinds.
const (
	TUnit TypeKind = iota
	TBool
	TInt
	TFloat
	TString
	TDuration
	TTime
	TList
	TStream
	TMap
	TOption
	TResult
	TTuple
	TNamed // record or enum, by type id
	TFqdn
	TIPAddress
	TNetblock
	TAsn
	TURL
	TEntityRef
	TInScope
)

// Type is a parsed DSL type text such as "Option<List<Fqdn>>", resolved
// against a module's TYPES. It guides the JSON conversion of workflow
// inputs, outputs and tool results.
type Type struct {
	Kind TypeKind
	Args []Type
	ID   bytecode.TypeID // TNamed
}

var simpleTypes = map[string]TypeKind{
	"Bool": TBool, "Int": TInt, "Float": TFloat, "String": TString,
	"Duration": TDuration, "Time": TTime, "Fqdn": TFqdn, "IpAddress": TIPAddress,
	"Netblock": TNetblock, "Asn": TAsn, "Url": TURL, "EntityRef": TEntityRef, "Entity": TEntityRef,
	"Unit": TUnit,
}

var genericTypes = map[string]struct {
	kind  TypeKind
	arity int
}{
	"List": {TList, 1}, "Stream": {TStream, 1}, "Map": {TMap, 2},
	"Option": {TOption, 1}, "Result": {TResult, 2}, "InScope": {TInScope, 1},
}

// ParseType parses DSL type text against module m.
func ParseType(m *bytecode.Module, text string) (Type, error) {
	p := typeParser{m: m, s: text}
	t, err := p.parse(0)
	if err != nil {
		return Type{}, err
	}
	p.space()
	if p.i != len(p.s) {
		return Type{}, fmt.Errorf("%w: trailing text in %q", ErrType, text)
	}
	return t, nil
}

type typeParser struct {
	m *bytecode.Module
	s string
	i int
}

func (p *typeParser) space() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

func (p *typeParser) eat(c byte) bool {
	p.space()
	if p.i < len(p.s) && p.s[p.i] == c {
		p.i++
		return true
	}
	return false
}

func (p *typeParser) parse(depth int) (Type, error) {
	if depth > 64 {
		return Type{}, fmt.Errorf("%w: nested too deeply", ErrType)
	}
	if p.eat('(') {
		if p.eat(')') {
			return Type{Kind: TUnit}, nil
		}
		var elems []Type
		for {
			t, err := p.parse(depth + 1)
			if err != nil {
				return Type{}, err
			}
			elems = append(elems, t)
			if p.eat(')') {
				break
			}
			if !p.eat(',') {
				return Type{}, fmt.Errorf("%w: expected , or ) in %q", ErrType, p.s)
			}
		}
		if p.eat('-') {
			return Type{}, fmt.Errorf("%w: function types are not data: %q", ErrType, p.s)
		}
		if len(elems) == 1 {
			return elems[0], nil
		}
		return Type{Kind: TTuple, Args: elems}, nil
	}
	p.space()
	start := p.i
	for p.i < len(p.s) && (isIdent(p.s[p.i]) || p.s[p.i] == ':') {
		p.i++
	}
	name := p.s[start:p.i]
	if name == "" {
		return Type{}, fmt.Errorf("%w: expected a type name in %q", ErrType, p.s)
	}
	if k, ok := simpleTypes[name]; ok {
		return Type{Kind: k}, nil
	}
	if g, ok := genericTypes[name]; ok {
		if !p.eat('<') {
			return Type{}, fmt.Errorf("%w: %s needs type arguments", ErrType, name)
		}
		var args []Type
		for {
			t, err := p.parse(depth + 1)
			if err != nil {
				return Type{}, err
			}
			args = append(args, t)
			if p.eat('>') {
				break
			}
			if !p.eat(',') {
				return Type{}, fmt.Errorf("%w: expected , or > in %q", ErrType, p.s)
			}
		}
		if len(args) != g.arity {
			return Type{}, fmt.Errorf("%w: %s takes %d type arguments", ErrType, name, g.arity)
		}
		return Type{Kind: g.kind, Args: args}, nil
	}
	l, ok := p.m.LayoutByName(name)
	if !ok || l.Variadic {
		return Type{}, fmt.Errorf("%w: unknown type %q", ErrType, name)
	}
	return Type{Kind: TNamed, ID: l.ID}, nil
}

func isIdent(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// JSONOptions adjusts ToJSON.
type JSONOptions struct {
	// UnwrapProofs encodes a ScopeProof as its target. The engine uses it to
	// hand InScope<T> arguments to tools; workflow outputs never set it, so
	// a proof in an output is an error (spec 8.2).
	UnwrapProofs bool
}

// ToJSON encodes v as canonical JSON (core.CanonicalJSON):
//
//	Unit -> null; Bool, Int, Float -> JSON literals; Duration -> Go duration
//	text ("1h30m0s"); Time -> RFC 3339 UTC with nanoseconds; String, domain
//	scalars, EntityRef -> strings; List, Stream -> arrays; Map with String or
//	domain keys -> object (keys sorted), other maps -> array of [key, value];
//	record -> object by field name; tuple -> array; Option -> null or the
//	payload; Result -> {"ok": v} or {"err": e}; other enums ->
//	{"variant": name} plus "values": [payload...] when it has a payload.
//
// Closures cannot be encoded; ScopeProofs only with UnwrapProofs.
func ToJSON(m *bytecode.Module, v Value, opts JSONOptions) ([]byte, error) {
	j := jsonEncoder{m: m, opts: opts}
	t, err := j.value(v, 0)
	if err != nil {
		return nil, err
	}
	return core.CanonicalJSON(t)
}

type jsonEncoder struct {
	m    *bytecode.Module
	opts JSONOptions
}

func (j *jsonEncoder) value(v Value, depth int) (any, error) {
	if depth > maxValueDepth {
		return nil, fmt.Errorf("%w: nested too deeply", ErrEncode)
	}
	switch v.k {
	case KindUnit:
		return nil, nil
	case KindBool:
		return v.AsBool(), nil
	case KindInt:
		return v.AsInt(), nil
	case KindFloat:
		return v.AsFloat(), nil
	case KindDuration:
		return time.Duration(v.AsInt()).String(), nil
	case KindTime:
		return FormatTime(v.AsInt()), nil
	case KindList:
		return j.slice(v.Elems(), depth)
	case KindMap:
		m := v.p.(*mapObj)
		objKeys := true
		for _, k := range m.keys {
			if !k.k.isStringy() {
				objKeys = false
				break
			}
		}
		if objKeys {
			out := make(map[string]any, len(m.keys))
			for i, k := range m.keys {
				x, err := j.value(m.vals[i], depth+1)
				if err != nil {
					return nil, err
				}
				out[k.Str()] = x
			}
			return out, nil
		}
		out := make([]any, len(m.keys))
		for i := range m.keys {
			kx, err := j.value(m.keys[i], depth+1)
			if err != nil {
				return nil, err
			}
			vx, err := j.value(m.vals[i], depth+1)
			if err != nil {
				return nil, err
			}
			out[i] = []any{kx, vx}
		}
		return out, nil
	case KindRecord:
		if v.TypeID() == bytecode.TypeTuple {
			return j.slice(v.Fields(), depth)
		}
		l, ok := j.m.Layout(v.TypeID())
		if !ok || len(l.Fields) != len(v.Fields()) {
			return nil, fmt.Errorf("%w: record of unknown type %d", ErrEncode, v.TypeID())
		}
		out := make(map[string]any, len(l.Fields))
		for i, f := range v.Fields() {
			x, err := j.value(f, depth+1)
			if err != nil {
				return nil, err
			}
			out[l.Fields[i].Name] = x
		}
		return out, nil
	case KindEnum:
		fs := v.Fields()
		switch v.TypeID() {
		case bytecode.TypeOption:
			if v.Variant() == bytecode.VariantNone {
				return nil, nil
			}
			return j.value(fs[0], depth+1)
		case bytecode.TypeResult:
			x, err := j.value(fs[0], depth+1)
			if err != nil {
				return nil, err
			}
			if v.Variant() == bytecode.VariantOk {
				return map[string]any{"ok": x}, nil
			}
			return map[string]any{"err": x}, nil
		}
		l, ok := j.m.Layout(v.TypeID())
		if !ok || int(v.Variant()) >= len(l.Variants) {
			return nil, fmt.Errorf("%w: enum of unknown type %d", ErrEncode, v.TypeID())
		}
		out := map[string]any{"variant": l.Variants[v.Variant()].Name}
		if len(fs) > 0 {
			xs, err := j.slice(fs, depth)
			if err != nil {
				return nil, err
			}
			out["values"] = xs
		}
		return out, nil
	case KindClosure:
		return nil, fmt.Errorf("%w: closures are not data", ErrEncode)
	case KindScopeProof:
		if !j.opts.UnwrapProofs {
			return nil, ErrProofInOutput
		}
		return j.value(v.ProofTarget(), depth+1)
	}
	return v.Str(), nil
}

func (j *jsonEncoder) slice(vs []Value, depth int) ([]any, error) {
	out := make([]any, len(vs))
	for i, e := range vs {
		x, err := j.value(e, depth+1)
		if err != nil {
			return nil, err
		}
		out[i] = x
	}
	return out, nil
}

// FromJSON decodes JSON into a value of type t (the inverse of ToJSON).
// Records must have exactly their declared fields, except that missing
// Option fields decode as None. Domain scalars are validated and
// normalized. Durations also accept integer nanoseconds, Times integer
// Unix nanoseconds, Host a bare name or address, and payload-free enum
// variants a bare variant name. InScope<T> never decodes: proofs only come
// from scope_check results.
func FromJSON(m *bytecode.Module, t Type, data []byte) (Value, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return Value{}, fmt.Errorf("%w: %v", ErrJSON, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Value{}, fmt.Errorf("%w: trailing data", ErrJSON)
	}
	d := jsonDecoder{m: m, types: map[string]Type{}}
	return d.value(t, x, 0)
}

type jsonDecoder struct {
	m     *bytecode.Module
	types map[string]Type
}

func jsonErr(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrJSON, fmt.Sprintf(format, args...))
}

func (d *jsonDecoder) typeText(s string) (Type, error) {
	if t, ok := d.types[s]; ok {
		return t, nil
	}
	if s == "" {
		return Type{}, fmt.Errorf("%w: field without a type", ErrType)
	}
	t, err := ParseType(d.m, s)
	if err != nil {
		return Type{}, err
	}
	d.types[s] = t
	return t, nil
}

func (d *jsonDecoder) value(t Type, x any, depth int) (Value, error) {
	if depth > maxValueDepth {
		return Value{}, jsonErr("nested too deeply")
	}
	switch t.Kind {
	case TUnit:
		if x != nil {
			return Value{}, jsonErr("expected null")
		}
		return Unit(), nil
	case TBool:
		b, ok := x.(bool)
		if !ok {
			return Value{}, jsonErr("expected a boolean")
		}
		return Bool(b), nil
	case TInt:
		n, ok := x.(json.Number)
		if !ok {
			return Value{}, jsonErr("expected an integer")
		}
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil {
			return Value{}, jsonErr("expected a 64-bit integer, got %s", n)
		}
		return Int(i), nil
	case TFloat:
		n, ok := x.(json.Number)
		if !ok {
			return Value{}, jsonErr("expected a number")
		}
		f, err := strconv.ParseFloat(n.String(), 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return Value{}, jsonErr("expected a finite number, got %s", n)
		}
		return Float(f), nil
	case TDuration, TTime:
		if n, ok := x.(json.Number); ok {
			i, err := strconv.ParseInt(n.String(), 10, 64)
			if err != nil {
				return Value{}, jsonErr("expected integer nanoseconds")
			}
			if t.Kind == TDuration {
				return Duration(i), nil
			}
			return Time(i), nil
		}
		s, ok := x.(string)
		if !ok {
			return Value{}, jsonErr("expected a string")
		}
		if t.Kind == TDuration {
			dd, err := time.ParseDuration(s)
			if err != nil {
				return Value{}, jsonErr("bad duration %q", s)
			}
			return Duration(int64(dd)), nil
		}
		tm, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return Value{}, jsonErr("bad time %q", s)
		}
		return Time(tm.UnixNano()), nil
	case TString, TEntityRef, TFqdn, TIPAddress, TNetblock, TAsn, TURL:
		s, ok := x.(string)
		if !ok {
			return Value{}, jsonErr("expected a string")
		}
		if len(s) > DefaultString {
			return Value{}, jsonErr("string over 1 MiB")
		}
		switch t.Kind {
		case TString:
			return Str(s), nil
		case TEntityRef:
			return EntityRef(s), nil
		case TFqdn:
			return wrapDomain(Fqdn(s))
		case TIPAddress:
			return wrapDomain(IPAddress(s))
		case TNetblock:
			return wrapDomain(Netblock(s))
		case TAsn:
			return wrapDomain(Asn(s))
		}
		return wrapDomain(URL(s))
	case TList, TStream:
		xs, ok := x.([]any)
		if !ok {
			return Value{}, jsonErr("expected an array")
		}
		if len(xs) > DefaultList {
			return Value{}, jsonErr("list over the element limit")
		}
		elems := make([]Value, len(xs))
		for i, e := range xs {
			v, err := d.value(t.Args[0], e, depth+1)
			if err != nil {
				return Value{}, err
			}
			elems[i] = v
		}
		v := mkList(elems)
		if t.Kind == TStream {
			v.f = flagStream
		}
		return v, nil
	case TMap:
		var keys, vals []Value
		switch obj := x.(type) {
		case map[string]any:
			names := make([]string, 0, len(obj))
			for k := range obj {
				names = append(names, k)
			}
			sort.Strings(names)
			for _, name := range names {
				k, err := d.value(t.Args[0], name, depth+1)
				if err != nil {
					return Value{}, err
				}
				v, err := d.value(t.Args[1], obj[name], depth+1)
				if err != nil {
					return Value{}, err
				}
				keys, vals = append(keys, k), append(vals, v)
			}
		case []any:
			for _, e := range obj {
				pair, ok := e.([]any)
				if !ok || len(pair) != 2 {
					return Value{}, jsonErr("expected [key, value] pairs")
				}
				k, err := d.value(t.Args[0], pair[0], depth+1)
				if err != nil {
					return Value{}, err
				}
				v, err := d.value(t.Args[1], pair[1], depth+1)
				if err != nil {
					return Value{}, err
				}
				keys, vals = append(keys, k), append(vals, v)
			}
		default:
			return Value{}, jsonErr("expected an object or an array of pairs")
		}
		v, err := Map(keys, vals)
		if err != nil {
			return Value{}, fmt.Errorf("%w: %v", ErrJSON, err)
		}
		return v, nil
	case TOption:
		if x == nil {
			return None(), nil
		}
		v, err := d.value(t.Args[0], x, depth+1)
		if err != nil {
			return Value{}, err
		}
		return Some(v), nil
	case TResult:
		obj, ok := x.(map[string]any)
		if !ok || len(obj) != 1 {
			return Value{}, jsonErr(`expected {"ok": ...} or {"err": ...}`)
		}
		if ok, has := obj["ok"]; has {
			v, err := d.value(t.Args[0], ok, depth+1)
			if err != nil {
				return Value{}, err
			}
			return Ok(v), nil
		}
		e, has := obj["err"]
		if !has {
			return Value{}, jsonErr(`expected {"ok": ...} or {"err": ...}`)
		}
		v, err := d.value(t.Args[1], e, depth+1)
		if err != nil {
			return Value{}, err
		}
		return Err(v), nil
	case TTuple:
		xs, ok := x.([]any)
		if !ok || len(xs) != len(t.Args) {
			return Value{}, jsonErr("expected an array of %d elements", len(t.Args))
		}
		fs := make([]Value, len(xs))
		for i, e := range xs {
			v, err := d.value(t.Args[i], e, depth+1)
			if err != nil {
				return Value{}, err
			}
			fs[i] = v
		}
		return Tuple(fs...), nil
	case TNamed:
		return d.named(t.ID, x, depth)
	case TInScope:
		return Value{}, jsonErr("InScope values cannot be decoded from JSON")
	}
	return Value{}, jsonErr("unsupported type")
}

func wrapDomain(v Value, err error) (Value, error) {
	if err != nil {
		return Value{}, fmt.Errorf("%w: %v", ErrJSON, err)
	}
	return v, nil
}

func (d *jsonDecoder) named(id bytecode.TypeID, x any, depth int) (Value, error) {
	l, ok := d.m.Layout(id)
	if !ok {
		return Value{}, jsonErr("unknown type id %d", id)
	}
	if l.Kind == bytecode.KindRecord {
		obj, ok := x.(map[string]any)
		if !ok {
			return Value{}, jsonErr("expected an object for %s", l.Name)
		}
		fs := make([]Value, len(l.Fields))
		seen := 0
		for i, f := range l.Fields {
			ft, err := d.typeText(f.Type)
			if err != nil {
				return Value{}, err
			}
			fx, has := obj[f.Name]
			if !has {
				if ft.Kind != TOption {
					return Value{}, jsonErr("%s: missing field %q", l.Name, f.Name)
				}
				fs[i] = None()
				continue
			}
			seen++
			v, err := d.value(ft, fx, depth+1)
			if err != nil {
				return Value{}, fmt.Errorf("%s.%s: %w", l.Name, f.Name, err)
			}
			fs[i] = v
		}
		if seen != len(obj) {
			return Value{}, jsonErr("%s: unknown fields", l.Name)
		}
		return Record(id, fs...), nil
	}
	if l.Kind != bytecode.KindEnum {
		return Value{}, jsonErr("%s is not a value type", l.Name)
	}
	if id == bytecode.TypeHost {
		if s, ok := x.(string); ok {
			return wrapDomain(Host(s))
		}
	}
	name, vals := "", []any(nil)
	switch e := x.(type) {
	case string:
		name = e
	case map[string]any:
		n, ok := e["variant"].(string)
		if !ok {
			return Value{}, jsonErr(`%s: expected {"variant": name}`, l.Name)
		}
		name = n
		if raw, has := e["values"]; has {
			if vals, ok = raw.([]any); !ok {
				return Value{}, jsonErr("%s: values must be an array", l.Name)
			}
		}
		allowed := 1
		if _, has := e["values"]; has {
			allowed = 2
		}
		if len(e) != allowed {
			return Value{}, jsonErr("%s: unknown keys", l.Name)
		}
	default:
		return Value{}, jsonErr("expected a variant of %s", l.Name)
	}
	for vi, variant := range l.Variants {
		if variant.Name != name {
			continue
		}
		if len(vals) != len(variant.Payload) {
			return Value{}, jsonErr("%s::%s takes %d values", l.Name, name, len(variant.Payload))
		}
		payload := make([]Value, len(vals))
		for i, px := range vals {
			pt, err := d.typeText(variant.Payload[i])
			if err != nil {
				return Value{}, err
			}
			v, err := d.value(pt, px, depth+1)
			if err != nil {
				return Value{}, err
			}
			payload[i] = v
		}
		return Enum(id, uint8(vi), payload...), nil
	}
	return Value{}, jsonErr("%s has no variant %q", l.Name, name)
}

// DecodeInputs decodes a workflow's JSON input object (parameter name to
// value) using the entry function's signature in TYPES.
func DecodeInputs(prog *bytecode.Verified, data []byte) ([]Value, error) {
	m := prog.Module()
	sig, ok := m.Signature(m.Entry)
	if !ok {
		return nil, fmt.Errorf("%w: module has no entry signature", ErrType)
	}
	params := make([]string, 0, len(sig.Fields))
	fields := make([]string, 0, len(sig.Fields))
	for _, f := range sig.Fields {
		params = append(params, f.Name)
		fields = append(fields, fmt.Sprintf("%s: %s", f.Name, f.Type))
	}
	// Decode the object as an anonymous record with the parameter types.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var x any
	if err := dec.Decode(&x); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrJSON, err)
	}
	obj, ok := x.(map[string]any)
	if !ok {
		return nil, jsonErr("inputs must be an object (%s)", strings.Join(fields, ", "))
	}
	d := jsonDecoder{m: m, types: map[string]Type{}}
	out := make([]Value, len(sig.Fields))
	seen := 0
	for i, f := range sig.Fields {
		t, err := d.typeText(f.Type)
		if err != nil {
			return nil, err
		}
		px, has := obj[f.Name]
		if !has {
			if t.Kind != TOption {
				return nil, jsonErr("missing input %q", f.Name)
			}
			out[i] = None()
			continue
		}
		seen++
		if out[i], err = d.value(t, px, 0); err != nil {
			return nil, fmt.Errorf("input %s: %w", f.Name, err)
		}
	}
	if seen != len(obj) {
		return nil, jsonErr("unknown inputs (expected %s)", strings.Join(params, ", "))
	}
	return out, nil
}

// EncodeOutput encodes a workflow result as canonical JSON. ScopeProofs
// and closures are rejected.
func EncodeOutput(prog *bytecode.Verified, v Value) ([]byte, error) {
	return ToJSON(prog.Module(), v, JSONOptions{})
}
