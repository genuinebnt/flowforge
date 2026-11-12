package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

// CanonicalJSON encodes v as canonical JSON: object keys sorted, no
// insignificant whitespace, no HTML escaping, numbers kept in their
// shortest textual form. Two values with the same JSON data model encode
// to the same bytes, which makes the output suitable for hashing.
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
	// sorted objects at every depth.
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
// fields and trailing data. Requests use it; responses are decoded
// tolerantly with plain json.Unmarshal (spec 1.2).
func DecodeStrict(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
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
