package oam

import (
	"bytes"
	"encoding"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
)

// DecodeMode selects how decoders treat keys a schema does not define
// (spec 25.3 oam_007).
type DecodeMode uint8

const (
	// Strict rejects unknown keys (matched exactly, not case-insensitively),
	// duplicate keys and trailing data. Use it for requests and tool output.
	Strict DecodeMode = iota
	// Lenient ignores unknown keys. Use it for data that is already stored,
	// so a newer writer's extra fields never make old readers fail.
	Lenient
)

// String returns "strict" or "lenient".
func (m DecodeMode) String() string {
	if m == Lenient {
		return "lenient"
	}
	return "strict"
}

// decodeJSON decodes one JSON value into v (a pointer). Numbers inside
// interface values decode as json.Number so they survive unchanged. In
// Strict mode the key structure is first checked against v's type; extra
// names top-level keys that are allowed although v has no field for them
// (the "type" discriminator).
func decodeJSON(data []byte, v any, mode DecodeMode, extra ...string) error {
	if mode == Strict {
		if err := checkStrict(data, reflect.TypeOf(v), extra); err != nil {
			return err
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data after JSON value", ErrMalformed)
	}
	return nil
}

// checkStrict walks data alongside the Go type t and reports the first key
// that t does not define, or any duplicated key.
func checkStrict(data []byte, t reflect.Type, extra []string) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := checkValue(dec, t, extra, "$"); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing data after JSON value", ErrMalformed)
	}
	return nil
}

func opaqueJSONType(t reflect.Type) bool {
	if t == reflect.TypeFor[json.RawMessage]() || t.Kind() == reflect.Interface {
		return true
	}
	pt := reflect.PointerTo(t)
	return pt.Implements(reflect.TypeFor[json.Unmarshaler]()) ||
		pt.Implements(reflect.TypeFor[encoding.TextUnmarshaler]())
}

func skipValue(dec *json.Decoder) error {
	var skip json.RawMessage
	if err := dec.Decode(&skip); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return nil
}

func checkValue(dec *json.Decoder, t reflect.Type, extra []string, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if opaqueJSONType(t) {
		return skipValue(dec)
	}
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		return checkObject(dec, t, extra, path)
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return skipValue(dec)
		}
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		if tok == nil {
			return nil
		}
		if d, ok := tok.(json.Delim); !ok || d != '[' {
			return fmt.Errorf("%w: %s: expected array", ErrMalformed, path)
		}
		for i := 0; dec.More(); i++ {
			if err := checkValue(dec, t.Elem(), nil, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		return nil
	default:
		return skipValue(dec)
	}
}

func checkObject(dec *json.Decoder, t reflect.Type, extra []string, path string) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if tok == nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("%w: %s: expected object", ErrMalformed, path)
	}
	var fields map[string]reflect.Type
	if t.Kind() == reflect.Struct {
		fields = jsonFields(t)
	}
	seen := make(map[string]bool)
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return fmt.Errorf("%w: %v", ErrMalformed, err)
		}
		key, ok := kt.(string)
		if !ok {
			return fmt.Errorf("%w: %s: object key is not a string", ErrMalformed, path)
		}
		if seen[key] {
			return fmt.Errorf("%w: %s.%s", ErrDuplicateField, path, key)
		}
		seen[key] = true
		var ft reflect.Type
		if t.Kind() == reflect.Map {
			ft = t.Elem()
		} else if ft, ok = fields[key]; !ok {
			if !slices.Contains(extra, key) {
				return fmt.Errorf("%w: %s.%s", ErrUnknownField, path, key)
			}
			if err := skipValue(dec); err != nil {
				return err
			}
			continue
		}
		if err := checkValue(dec, ft, nil, path+"."+key); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return nil
}

// jsonFields maps the exact JSON names of t's exported fields to their types.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

// normalizeJSON round-trips v through JSON so the result holds only
// JSON-native Go values (string, json.Number, bool, nil, []any,
// map[string]any). It also deep-copies v.
func normalizeJSON(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	var out any
	if err := decodeJSON(b, &out, Lenient); err != nil {
		return nil, err
	}
	return out, nil
}

// objectOf decodes a JSON object into a map with json.Number values.
func objectOf(b []byte) (map[string]any, error) {
	var m map[string]any
	if err := decodeJSON(b, &m, Lenient); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("%w: expected object", ErrMalformed)
	}
	return m, nil
}

// marshalTagged encodes v (a struct without JSON methods) as an object
// whose first key is "type": typ.
func marshalTagged(typ string, v any) ([]byte, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(body) < 2 || body[0] != '{' {
		return nil, fmt.Errorf("%w: %s does not encode as an object", ErrMalformed, typ)
	}
	head, err := json.Marshal(typ)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(body)+len(head)+10)
	out = append(out, `{"type":`...)
	out = append(out, head...)
	if len(body) > 2 {
		out = append(out, ',')
	}
	return append(out, body[1:]...), nil
}

// peekType reads the exact "type" key of a JSON object.
func peekType(data []byte, what error) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return "", fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	raw, ok := m["type"]
	if !ok {
		return "", fmt.Errorf("%w: missing type", what)
	}
	var typ string
	if err := json.Unmarshal(raw, &typ); err != nil {
		return "", fmt.Errorf("%w: type is not a string", what)
	}
	if !validTypeName(typ) {
		return "", fmt.Errorf("%w: bad type name %q", what, typ)
	}
	return typ, nil
}

// validTypeName accepts identifiers such as "FQDN" or "ProductRelease".
func validTypeName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z':
		case (c >= '0' && c <= '9' || c == '_') && i > 0:
		default:
			return false
		}
	}
	return true
}
