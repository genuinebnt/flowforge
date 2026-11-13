package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

// CanonicalJSON encodes v as canonical JSON for hashing (args hashes,
// request hashes, dedupe keys, stored payloads): object keys sorted by byte
// order at every depth, no insignificant whitespace, no HTML escaping, and
// numbers re-emitted with exactly the digits encoding/json produced for
// them (the shortest round-trip form for Go floats, verbatim for
// json.Number and json.RawMessage). Numbers are not normalized: a raw
// message holding 1.0 and one holding 1 encode differently. v is first
// marshalled with encoding/json, so struct tags, MarshalJSON methods and
// omitempty apply; the error is json.Marshal's or the re-decode's.
//
// PORT: this is a cross-language hashing contract (docs/PORTING_RUST.md
// 4.2). encoding/json escapes U+2028 and U+2029 as \u2028/\u2029 even with
// HTML escaping off, replaces invalid UTF-8 in strings with U+FFFD,
// encodes nil slices and maps as null (empty ones as [] and {}), []byte as
// standard padded base64, and float64 values with strconv 'g'-like
// formatting that switches to exponent form below 1e-6 and from 1e21 on
// ("1e-07" is printed as "1e-7", 1e21 as "1e+21"). serde_json
// differs on every one of these; reproduce them explicitly.
func CanonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// encoding/json sorts map keys, so re-encoding the generic form yields
	// sorted objects at every depth. UseNumber above keeps each number as
	// its original digits instead of rounding it through float64.
	// PORT: Go sorts map[string]any keys by Go string comparison, which is
	// byte order of the UTF-8 encoding (not UTF-16 code units as in JCS,
	// RFC 8785); BTreeMap<String, _> in Rust sorts the same way.
	if err := enc.Encode(generic); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// SHA256Hex returns the lowercase hex SHA-256 of b.
func SHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// HashJSON returns the SHA-256 of the canonical JSON encoding of v.
func HashJSON(v any) ([32]byte, error) {
	b, err := CanonicalJSON(v)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(b), nil
}

// DecodeStrict decodes one JSON value from r into v, rejecting unknown
// fields and any non-whitespace data after the value. Requests use it;
// responses are decoded tolerantly with plain json.Unmarshal (spec 1.2).
//
// PORT: encoding/json matches object keys to struct fields
// case-insensitively and lets a duplicate key overwrite the earlier one,
// and DisallowUnknownFields does not change either. A key "Name" therefore
// fills field `json:"name"` here, while serde with deny_unknown_fields
// rejects it. Packages that must reject case variants and duplicates (oam,
// auth) check the raw object themselves; match this lenient behaviour
// elsewhere or add conformance cases before tightening it.
func DecodeStrict(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	// More catches a following value that starts a new token; the second
	// Decode catches the rest (for example a stray "]"). Only io.EOF means
	// the input held exactly one value.
	if dec.More() {
		return fmt.Errorf("core: trailing data after JSON value")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return fmt.Errorf("core: trailing data after JSON value")
	}
	return nil
}

// DecodeStrictBytes is DecodeStrict over a byte slice.
func DecodeStrictBytes(b []byte, v any) error { return DecodeStrict(bytes.NewReader(b), v) }
