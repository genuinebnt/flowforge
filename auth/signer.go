package auth

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
)

// Signer signs claims as JWS compact EdDSA tokens with one active key. It
// is safe for concurrent use.
type Signer struct {
	key    SigningKey
	header string // base64url of the fixed protected header
}

// NewSigner returns a Signer for key. Only an active key may sign
// (ErrKeyNotActive).
func NewSigner(key SigningKey) (*Signer, error) {
	if err := checkKID(key.KID); err != nil {
		return nil, err
	}
	if len(key.Private) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: private key is not %d bytes", ErrInvalidKey, ed25519.PrivateKeySize)
	}
	if key.State != KeyActive {
		return nil, fmt.Errorf("%w: key %q is %s", ErrKeyNotActive, key.KID, key.State)
	}
	h, err := json.Marshal(header{Alg: Algorithm, Typ: TokenType, Kid: key.KID})
	if err != nil {
		return nil, fmt.Errorf("auth: encode header: %w", err)
	}
	return &Signer{key: key, header: b64url.EncodeToString(h)}, nil
}

// KID returns the kid placed in every header.
func (s *Signer) KID() string { return s.key.KID }

// Sign returns the compact serialization of c, with the protected header
// {"alg":"EdDSA","typ":"JWT","kid":<kid>}.
func (s *Signer) Sign(c Claims) (string, error) {
	p, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("auth: encode claims: %w", err)
	}
	input := s.header + "." + b64url.EncodeToString(p)
	sig := ed25519.Sign(s.key.Private, []byte(input))
	return input + "." + b64url.EncodeToString(sig), nil
}
