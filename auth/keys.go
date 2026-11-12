package auth

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
)

// KeyState is the lifecycle state of a signing key (spec 13.7).
type KeyState string

// Key states.
const (
	// KeyActive keys sign and verify.
	KeyActive KeyState = "active"
	// KeyRetiring keys verify only, until the longest-lived token they
	// signed has expired.
	KeyRetiring KeyState = "retiring"
	// KeyRetired keys neither sign nor verify.
	KeyRetired KeyState = "retired"
)

func (s KeyState) valid() bool { return s == KeyActive || s == KeyRetiring || s == KeyRetired }

// PublicKey is a verification key.
type PublicKey struct {
	KID   string
	Key   ed25519.PublicKey
	State KeyState
}

// SigningKey is an Ed25519 private key with its kid and state.
type SigningKey struct {
	KID     string
	Private ed25519.PrivateKey
	State   KeyState
}

// NewSigningKey derives a key from a 32-byte Ed25519 seed.
func NewSigningKey(kid string, seed []byte, state KeyState) (SigningKey, error) {
	if err := checkKID(kid); err != nil {
		return SigningKey{}, err
	}
	if len(seed) != ed25519.SeedSize {
		return SigningKey{}, fmt.Errorf("%w: seed must be %d bytes", ErrInvalidKey, ed25519.SeedSize)
	}
	if !state.valid() {
		return SigningKey{}, fmt.Errorf("%w: unknown state %q", ErrInvalidKey, state)
	}
	return SigningKey{KID: kid, Private: ed25519.NewKeyFromSeed(seed), State: state}, nil
}

// GenerateSigningKey creates an active key from 32 bytes of rand.
func GenerateSigningKey(rand io.Reader, kid string) (SigningKey, error) {
	seed := make([]byte, ed25519.SeedSize)
	if _, err := io.ReadFull(rand, seed); err != nil {
		return SigningKey{}, fmt.Errorf("auth: read key seed: %w", err)
	}
	return NewSigningKey(kid, seed, KeyActive)
}

// Public returns the verification half of k.
func (k SigningKey) Public() PublicKey {
	pub, _ := k.Private.Public().(ed25519.PublicKey)
	return PublicKey{KID: k.KID, Key: pub, State: k.State}
}

func checkKID(kid string) error {
	if kid == "" || len(kid) > 128 || strings.ContainsFunc(kid, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
		return fmt.Errorf("%w: kid must be 1-128 printable ASCII characters", ErrInvalidKey)
	}
	return nil
}

// KeyResolver finds the verification key for a kid. It returns
// ErrUnknownKid for a kid it does not know and ErrKeyRetired for a retired
// key.
type KeyResolver interface {
	ResolveKey(ctx context.Context, kid string) (PublicKey, error)
}

// KeySet is an immutable set of verification keys indexed by kid. It is
// safe for concurrent use.
type KeySet struct {
	keys map[string]PublicKey
}

var _ KeyResolver = (*KeySet)(nil)

// NewKeySet validates keys (non-empty unique kids, 32-byte keys, known
// states) and returns the set.
func NewKeySet(keys ...PublicKey) (*KeySet, error) {
	s := &KeySet{keys: make(map[string]PublicKey, len(keys))}
	for _, k := range keys {
		if err := checkKID(k.KID); err != nil {
			return nil, err
		}
		if len(k.Key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: key %q is not %d bytes", ErrInvalidKey, k.KID, ed25519.PublicKeySize)
		}
		if !k.State.valid() {
			return nil, fmt.Errorf("%w: key %q has unknown state %q", ErrInvalidKey, k.KID, k.State)
		}
		if _, dup := s.keys[k.KID]; dup {
			return nil, fmt.Errorf("%w: duplicate kid %q", ErrInvalidKey, k.KID)
		}
		s.keys[k.KID] = PublicKey{KID: k.KID, Key: bytes.Clone(k.Key), State: k.State}
	}
	return s, nil
}

// Lookup returns the key for kid, in any state.
func (s *KeySet) Lookup(kid string) (PublicKey, bool) {
	k, ok := s.keys[kid]
	return k, ok
}

// ResolveKey implements KeyResolver.
func (s *KeySet) ResolveKey(_ context.Context, kid string) (PublicKey, error) {
	k, ok := s.keys[kid]
	switch {
	case !ok:
		return PublicKey{}, ErrUnknownKid
	case k.State == KeyRetired:
		return PublicKey{}, ErrKeyRetired
	}
	return k, nil
}

// Keys returns every key sorted by kid.
func (s *KeySet) Keys() []PublicKey {
	out := make([]PublicKey, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b PublicKey) int { return strings.Compare(a.KID, b.KID) })
	return out
}

// jwk is an OKP/Ed25519 JSON Web Key (RFC 8037).
type jwk struct {
	Kty string `json:"kty"`
	Crv string `json:"crv,omitempty"`
	Kid string `json:"kid,omitempty"`
	Use string `json:"use,omitempty"`
	Alg string `json:"alg,omitempty"`
	X   string `json:"x,omitempty"`
	D   string `json:"d,omitempty"`
}

type jwks struct {
	Keys []jwk `json:"keys"`
}

var b64url = base64.RawURLEncoding.Strict()

// MarshalJSON renders the JWKS served at /.well-known/jwks.json: active and
// retiring public keys only, sorted by kid. Retired keys are not published.
func (s *KeySet) MarshalJSON() ([]byte, error) {
	out := jwks{Keys: []jwk{}}
	for _, k := range s.Keys() {
		if k.State == KeyRetired {
			continue
		}
		out.Keys = append(out.Keys, jwk{
			Kty: "OKP", Crv: "Ed25519", Kid: k.KID, Use: "sig", Alg: Algorithm,
			X: b64url.EncodeToString(k.Key),
		})
	}
	return json.Marshal(out)
}

// ParseJWKS parses a published JWKS. Keys whose kty is not OKP are
// skipped (a set may carry other key types during a migration); an OKP key
// must be Ed25519, usable for signatures with EdDSA, carry a kid and a
// 32-byte x, and must not carry a private part. Published keys verify, so
// they are returned as KeyActive.
func ParseJWKS(b []byte) (*KeySet, error) {
	var doc jwks
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidJWKS, err)
	}
	var keys []PublicKey
	for i, k := range doc.Keys {
		if k.Kty != "OKP" {
			continue
		}
		switch {
		case k.Crv != "Ed25519":
			return nil, fmt.Errorf("%w: key %d: crv %q", ErrInvalidJWKS, i, k.Crv)
		case k.D != "":
			return nil, fmt.Errorf("%w: key %d carries a private key", ErrInvalidJWKS, i)
		case k.Use != "" && k.Use != "sig":
			return nil, fmt.Errorf("%w: key %d: use %q", ErrInvalidJWKS, i, k.Use)
		case k.Alg != "" && k.Alg != Algorithm:
			return nil, fmt.Errorf("%w: key %d: alg %q", ErrInvalidJWKS, i, k.Alg)
		}
		x, err := b64url.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: key %d: x is not a base64url %d-byte key", ErrInvalidJWKS, i, ed25519.PublicKeySize)
		}
		keys = append(keys, PublicKey{KID: k.Kid, Key: x, State: KeyActive})
	}
	s, err := NewKeySet(keys...)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidJWKS, err)
	}
	return s, nil
}
