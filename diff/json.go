package diff

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

// MaxJSONDepth is the deepest array/object nesting ParseJSON accepts.
const MaxJSONDepth = 1000

// Kind is the type of a JSON value.
type Kind uint8

// JSON value kinds. The zero Value is null.
const (
	KindNull Kind = iota
	KindBool
	KindNumber
	KindString
	KindArray
	KindObject
)

// String returns the kind's name ("null", "boolean", "number", "string",
// "array", "object").
func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindBool:
		return "boolean"
	case KindNumber:
		return "number"
	case KindString:
		return "string"
	case KindArray:
		return "array"
	case KindObject:
		return "object"
	default:
		return fmt.Sprintf("Kind(%d)", uint8(k))
	}
}

// Value is a JSON value as a tagged tree (a Rust enum in the port). Only
// the field matching Kind is meaningful. Numbers keep their source text;
// object members keep source order and have unique keys.
type Value struct {
	Kind    Kind
	Bool    bool
	Number  string
	Text    string
	Items   []Value
	Members []Member
}

// Member is one object member.
type Member struct {
	Key   string
	Value Value
}

// Null returns the null value.
func Null() Value { return Value{} }

// BoolValue returns a boolean value.
func BoolValue(b bool) Value { return Value{Kind: KindBool, Bool: b} }

// NumberValue returns a number value; text must be a JSON number.
func NumberValue(text string) Value { return Value{Kind: KindNumber, Number: text} }

// StringValue returns a string value.
func StringValue(s string) Value { return Value{Kind: KindString, Text: s} }

// ArrayValue returns an array value.
func ArrayValue(items ...Value) Value { return Value{Kind: KindArray, Items: items} }

// ObjectValue returns an object value; keys must be unique.
func ObjectValue(members ...Member) Value { return Value{Kind: KindObject, Members: members} }

// Get returns the member with key and whether it exists.
func (v Value) Get(key string) (Value, bool) {
	for _, m := range v.Members {
		if m.Key == key {
			return m.Value, true
		}
	}
	return Value{}, false
}

// ParseJSON decodes exactly one JSON value (RFC 8259) with
// encoding/json's UseNumber, rejecting duplicate object keys, trailing
// data and nesting deeper than MaxJSONDepth. Errors wrap ErrInvalidJSON.
func ParseJSON(b []byte) (Value, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	v, err := parseValue(dec, 0)
	if err != nil {
		return Value{}, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return Value{}, fmt.Errorf("%w: trailing data", ErrInvalidJSON)
	}
	return v, nil
}

func parseValue(dec *json.Decoder, depth int) (Value, error) {
	tok, err := dec.Token()
	if err != nil {
		return Value{}, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
	}
	return valueFromToken(dec, tok, depth)
}

func valueFromToken(dec *json.Decoder, tok json.Token, depth int) (Value, error) {
	switch t := tok.(type) {
	case nil:
		return Value{}, nil
	case bool:
		return BoolValue(t), nil
	case json.Number:
		return NumberValue(t.String()), nil
	case string:
		return StringValue(t), nil
	case json.Delim:
		if depth >= MaxJSONDepth {
			return Value{}, fmt.Errorf("%w: nesting deeper than %d", ErrInvalidJSON, MaxJSONDepth)
		}
		switch t {
		case '[':
			v := Value{Kind: KindArray, Items: []Value{}}
			for dec.More() {
				item, err := parseValue(dec, depth+1)
				if err != nil {
					return Value{}, err
				}
				v.Items = append(v.Items, item)
			}
			if _, err := dec.Token(); err != nil {
				return Value{}, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
			}
			return v, nil
		case '{':
			v := Value{Kind: KindObject, Members: []Member{}}
			seen := make(map[string]struct{})
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return Value{}, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
				}
				key, ok := kt.(string)
				if !ok {
					return Value{}, fmt.Errorf("%w: object key is not a string", ErrInvalidJSON)
				}
				if _, dup := seen[key]; dup {
					return Value{}, fmt.Errorf("%w: duplicate object key %q", ErrInvalidJSON, key)
				}
				seen[key] = struct{}{}
				mv, err := parseValue(dec, depth+1)
				if err != nil {
					return Value{}, err
				}
				v.Members = append(v.Members, Member{Key: key, Value: mv})
			}
			if _, err := dec.Token(); err != nil {
				return Value{}, fmt.Errorf("%w: %w", ErrInvalidJSON, err)
			}
			return v, nil
		}
	}
	return Value{}, fmt.Errorf("%w: unexpected token %v", ErrInvalidJSON, tok)
}

// EncodeJSON writes v as compact JSON: members in stored order, numbers as
// their text, strings escaped like encoding/json without HTML escaping.
func EncodeJSON(v Value) []byte {
	var buf bytes.Buffer
	encodeValue(&buf, v)
	return buf.Bytes()
}

func encodeValue(buf *bytes.Buffer, v Value) {
	switch v.Kind {
	case KindNull:
		buf.WriteString("null")
	case KindBool:
		buf.WriteString(strconv.FormatBool(v.Bool))
	case KindNumber:
		buf.WriteString(v.Number)
	case KindString:
		encodeString(buf, v.Text)
	case KindArray:
		buf.WriteByte('[')
		for i, it := range v.Items {
			if i > 0 {
				buf.WriteByte(',')
			}
			encodeValue(buf, it)
		}
		buf.WriteByte(']')
	case KindObject:
		buf.WriteByte('{')
		for i, m := range v.Members {
			if i > 0 {
				buf.WriteByte(',')
			}
			encodeString(buf, m.Key)
			buf.WriteByte(':')
			encodeValue(buf, m.Value)
		}
		buf.WriteByte('}')
	}
}

func encodeString(buf *bytes.Buffer, s string) {
	var tmp bytes.Buffer
	enc := json.NewEncoder(&tmp)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		buf.WriteString(`""`) // strings always encode; unreachable
		return
	}
	buf.Write(bytes.TrimRight(tmp.Bytes(), "\n"))
}

// Canonical returns a deep copy of v with object members sorted by key
// (bytewise) at every depth.
func Canonical(v Value) Value {
	out := cloneValue(v)
	sortMembers(&out)
	return out
}

func sortMembers(v *Value) {
	for i := range v.Items {
		sortMembers(&v.Items[i])
	}
	for i := range v.Members {
		sortMembers(&v.Members[i].Value)
	}
	slices.SortFunc(v.Members, func(a, b Member) int { return strings.Compare(a.Key, b.Key) })
}

func cloneValue(v Value) Value {
	out := v
	if v.Items != nil {
		out.Items = make([]Value, len(v.Items))
		for i, it := range v.Items {
			out.Items[i] = cloneValue(it)
		}
	}
	if v.Members != nil {
		out.Members = make([]Member, len(v.Members))
		for i, m := range v.Members {
			out.Members[i] = Member{Key: m.Key, Value: cloneValue(m.Value)}
		}
	}
	return out
}

// Equal reports structural equality: numbers compare by numeric value
// ("1", "1.0" and "10e-1" are equal), objects ignore member order, arrays
// compare element-wise. This is RFC 6902's "test" equality.
func Equal(a, b Value) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case KindNull:
		return true
	case KindBool:
		return a.Bool == b.Bool
	case KindNumber:
		return a.Number == b.Number || canonicalNumber(a.Number) == canonicalNumber(b.Number)
	case KindString:
		return a.Text == b.Text
	case KindArray:
		return slices.EqualFunc(a.Items, b.Items, Equal)
	case KindObject:
		if len(a.Members) != len(b.Members) {
			return false
		}
		for _, m := range a.Members {
			bv, ok := b.Get(m.Key)
			if !ok || !Equal(m.Value, bv) {
				return false
			}
		}
		return true
	}
	return false
}

// canonicalNumber returns an exact canonical form of a JSON number's value:
// "0", or [-]digits"e"exponent with no leading or trailing zeros in digits.
// Malformed text is returned unchanged (it then only equals itself).
func canonicalNumber(s string) string {
	neg := strings.HasPrefix(s, "-")
	body := strings.TrimPrefix(s, "-")
	mant, expText, hasExp := strings.Cut(strings.ToLower(body), "e")
	intPart, frac, _ := strings.Cut(mant, ".")
	digits := strings.TrimLeft(intPart+frac, "0")
	if digits == "" {
		return "0"
	}
	exp := new(big.Int)
	if hasExp {
		if _, ok := exp.SetString(strings.TrimPrefix(expText, "+"), 10); !ok {
			return s
		}
	}
	exp.Sub(exp, big.NewInt(int64(len(frac))))
	trimmed := strings.TrimRight(digits, "0")
	exp.Add(exp, big.NewInt(int64(len(digits)-len(trimmed))))
	sign := ""
	if neg {
		sign = "-"
	}
	return sign + trimmed + "e" + exp.String()
}

// hashCons assigns every distinct JSON value (under Equal) a dense uint32
// id bottom-up: a node's key is its kind plus its scalar payload or its
// children's ids, so equal subtrees get equal ids without hashing their
// full text, and different subtrees never collide.
type hashCons struct {
	ids map[string]uint32
	buf []byte
}

func newHashCons() *hashCons { return &hashCons{ids: make(map[string]uint32)} }

func (h *hashCons) id(v Value) uint32 {
	var key []byte
	switch v.Kind {
	case KindNull:
		key = []byte{'n'}
	case KindBool:
		key = []byte{'f'}
		if v.Bool {
			key[0] = 't'
		}
	case KindNumber:
		key = append([]byte{'#'}, canonicalNumber(v.Number)...)
	case KindString:
		key = append([]byte{'s'}, v.Text...)
	case KindArray:
		key = []byte{'['}
		for _, it := range v.Items {
			key = binary.LittleEndian.AppendUint32(key, h.id(it))
		}
	case KindObject:
		ms := slices.Clone(v.Members)
		slices.SortFunc(ms, func(a, b Member) int { return strings.Compare(a.Key, b.Key) })
		key = []byte{'{'}
		for _, m := range ms {
			key = binary.AppendUvarint(key, uint64(len(m.Key)))
			key = append(key, m.Key...)
			key = binary.LittleEndian.AppendUint32(key, h.id(m.Value))
		}
	}
	if id, ok := h.ids[string(key)]; ok {
		return id
	}
	id := uint32(len(h.ids))
	h.ids[string(key)] = id
	return id
}

