package alerts

import (
	"math"
	"strconv"
	"time"
)

// kind is a static type of the alert language and the tag of a run-time
// value.
type kind uint8

const (
	kInvalid  kind = iota // error recovery: suppresses cascading diagnostics
	kNull                 // run time only: the field is absent from the event
	kBool                 // Bool
	kInt                  // Int (64-bit signed)
	kFloat                // Float (binary64)
	kString               // String
	kDuration             // Duration (nanoseconds)
	kTime                 // Time (Unix nanoseconds, UTC)
	kList                 // List<elem>
	kObject               // Object (opaque JSON object: patch, payload, attributes)
	kRange                // Range (half-open integer range, right of `in` only)
	kAny                  // element type of the empty list literal
)

func (k kind) String() string {
	switch k {
	case kBool:
		return "Bool"
	case kInt:
		return "Int"
	case kFloat:
		return "Float"
	case kString:
		return "String"
	case kDuration:
		return "Duration"
	case kTime:
		return "Time"
	case kList:
		return "List"
	case kObject:
		return "Object"
	case kRange:
		return "Range"
	case kNull:
		return "Null"
	case kAny:
		return "Any"
	}
	return "<invalid>"
}

// typ is a static type. elem is set for lists.
type typ struct {
	k    kind
	elem kind
}

func (t typ) String() string {
	if t.k == kList {
		return "List<" + t.elem.String() + ">"
	}
	return t.k.String()
}

func (t typ) isScalar() bool {
	switch t.k {
	case kBool, kInt, kFloat, kString, kDuration, kTime:
		return true
	}
	return false
}

func (t typ) isNumeric() bool { return t.k == kInt || t.k == kFloat }

var (
	tInvalidT = typ{k: kInvalid}
	tBoolT    = typ{k: kBool}
	tIntT     = typ{k: kInt}
	tFloatT   = typ{k: kFloat}
	tStringT  = typ{k: kString}
	tDurT     = typ{k: kDuration}
	tTimeT    = typ{k: kTime}
	tObjectT  = typ{k: kObject}
	tRangeT   = typ{k: kRange}
	tStrList  = typ{k: kList, elem: kString}
)

// value is a run-time value. Durations and times live in i.
type value struct {
	k    kind
	b    bool
	i    int64
	f    float64
	s    string
	list []value
	obj  map[string]any
}

var nullValue = value{k: kNull}

func boolVal(b bool) value      { return value{k: kBool, b: b} }
func intVal(i int64) value      { return value{k: kInt, i: i} }
func floatVal(f float64) value  { return value{k: kFloat, f: f} }
func stringVal(s string) value  { return value{k: kString, s: s} }
func durationVal(d int64) value { return value{k: kDuration, i: d} }
func timeVal(t time.Time) value { return value{k: kTime, i: t.UnixNano()} }
func objectVal(m map[string]any) value {
	if m == nil {
		return nullValue
	}
	return value{k: kObject, obj: m}
}

// num returns a numeric value as float64.
func (v value) num() float64 {
	if v.k == kInt {
		return float64(v.i)
	}
	return v.f
}

// export converts a scalar value into a JSON-friendly Go value: strings,
// int64, float64 (non-finite floats as strings), bools, RFC 3339 times and
// Go duration strings. Absent values export as nil. Exported values feed
// DedupeKey, so their text is part of the dedupe-key contract.
//
// PORT: three Go formats to reproduce exactly: time.Duration.String
// ("1h30m0s", "250ms", "1.5µs" with U+00B5, "0s"), RFC3339Nano in UTC with
// trailing fractional zeros trimmed, and FormatFloat(f, 'g', -1, 64) for
// non-finite floats ("+Inf", "-Inf", "NaN").
func (v value) export() any {
	switch v.k {
	case kBool:
		return v.b
	case kInt:
		return v.i
	case kFloat:
		if math.IsInf(v.f, 0) || math.IsNaN(v.f) {
			return strconv.FormatFloat(v.f, 'g', -1, 64)
		}
		return v.f
	case kString:
		return v.s
	case kDuration:
		return time.Duration(v.i).String()
	case kTime:
		return time.Unix(0, v.i).UTC().Format(time.RFC3339Nano)
	case kList:
		out := make([]any, len(v.list))
		for i, e := range v.list {
			out[i] = e.export()
		}
		return out
	case kObject:
		return v.obj
	}
	return nil
}
