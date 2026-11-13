package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// JWS constants (spec 13.2).
const (
	// Algorithm is the only accepted JWS alg.
	Algorithm = "EdDSA"
	// TokenType is the required typ header.
	TokenType = "JWT"
	// MaxTokenLength bounds the compact serialization accepted by Verify.
	MaxTokenLength = 8 << 10
)

// header is the protected JOSE header. Unknown parameters are ignored
// except crit, whose presence is rejected (RFC 7515 4.1.11).
type header struct {
	Alg  string          `json:"alg"`
	Typ  string          `json:"typ,omitempty"`
	Kid  string          `json:"kid,omitempty"`
	Crit json.RawMessage `json:"crit,omitempty"`
}

// compact is a split, base64-decoded JWS compact serialization.
type compact struct {
	header       header
	signingInput string // base64url(header) "." base64url(payload)
	payload      []byte
	signature    []byte
}

// parseCompact splits and decodes a token without trusting anything in it.
func parseCompact(token string) (compact, error) {
	var c compact
	if len(token) > MaxTokenLength {
		return c, fmt.Errorf("%w: longer than %d bytes", ErrMalformed, MaxTokenLength)
	}
	h, rest, ok1 := strings.Cut(token, ".")
	p, s, ok2 := strings.Cut(rest, ".")
	if !ok1 || !ok2 || strings.Contains(s, ".") {
		return c, fmt.Errorf("%w: want three dot-separated segments", ErrMalformed)
	}
	hb, err := b64url.DecodeString(h)
	if err != nil {
		return c, fmt.Errorf("%w: header encoding: %w", ErrMalformed, err)
	}
	if err := decodeObject(hb, &c.header, headerNames...); err != nil {
		return c, fmt.Errorf("%w: header: %w", ErrMalformed, err)
	}
	if c.payload, err = b64url.DecodeString(p); err != nil {
		return c, fmt.Errorf("%w: payload encoding: %w", ErrMalformed, err)
	}
	if c.signature, err = b64url.DecodeString(s); err != nil {
		return c, fmt.Errorf("%w: signature encoding: %w", ErrMalformed, err)
	}
	c.signingInput = token[:len(h)+1+len(p)]
	return c, nil
}

// decodeObject decodes one JSON object into v. encoding/json matches
// member names case-insensitively and lets the last duplicate win, which
// other JOSE parsers do not; to leave no room for disagreement it rejects
// member names that are equal ignoring case, a member that spells one of
// the known names with different case, and trailing data.
func decodeObject(b []byte, v any, known ...string) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("not a JSON object")
	}
	seen := make(map[string]struct{})
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		name, _ := tok.(string)
		folded := strings.ToLower(name)
		if _, dup := seen[folded]; dup {
			return fmt.Errorf("duplicate member %q", name)
		}
		seen[folded] = struct{}{}
		for _, k := range known {
			if name != k && strings.EqualFold(name, k) {
				return fmt.Errorf("member %q must be spelled %q", name, k)
			}
		}
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return err
		}
	}
	if _, err := dec.Token(); err != nil { // closing brace
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("trailing data after object")
	}
	return json.Unmarshal(b, v)
}

var (
	headerNames = []string{"alg", "typ", "kid", "crit"}
	claimNames  = []string{"iss", "aud", "sub", "tid", "role", "sid", "sv", "jti", "iat", "exp", "nbf", "groups"}
)
