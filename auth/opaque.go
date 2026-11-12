package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"io"
	"strings"
)

// Opaque token formats (spec 13.2).
const (
	RefreshTokenPrefix = "ffr_"
	APIKeyPrefix       = "ffk_"
	// SecretBytes is the entropy of every opaque secret.
	SecretBytes = 32
	// secretChars is the unpadded base64url length of SecretBytes.
	secretChars = 43
	maxKeyIDLen = 64
)

// newSecret returns SecretBytes from rand as unpadded base64url.
func newSecret(rand io.Reader) (string, error) {
	if rand == nil {
		return "", fmt.Errorf("%w: no random reader", ErrConfig)
	}
	b := make([]byte, SecretBytes)
	if _, err := io.ReadFull(rand, b); err != nil {
		return "", fmt.Errorf("auth: read random secret: %w", err)
	}
	return b64url.EncodeToString(b), nil
}

// isSecret reports whether s is the canonical encoding of SecretBytes.
func isSecret(s string) bool {
	if len(s) != secretChars {
		return false
	}
	b, err := b64url.DecodeString(s)
	return err == nil && len(b) == SecretBytes
}

// HashToken returns the SHA-256 stored in place of an opaque token
// (refresh token, email verification token, API key secret).
func HashToken(token string) [32]byte { return sha256.Sum256([]byte(token)) }

// NewRefreshToken returns "ffr_" + base64url(32 random bytes).
func NewRefreshToken(rand io.Reader) (string, error) {
	s, err := newSecret(rand)
	if err != nil {
		return "", err
	}
	return RefreshTokenPrefix + s, nil
}

// IsRefreshTokenFormat reports whether s has the refresh token shape, so
// garbage can be rejected without a store lookup.
func IsRefreshTokenFormat(s string) bool {
	rest, ok := strings.CutPrefix(s, RefreshTokenPrefix)
	return ok && isSecret(rest)
}

// APIKey is a freshly minted API key. Token is shown to the user once;
// Prefix and SecretHash are stored (identity.api_keys.prefix and
// secret_hash).
type APIKey struct {
	ID         string   // key id, the lookup handle
	Prefix     string   // "ffk_<id>", stored in clear
	Token      string   // "ffk_<id>_<secret>", never stored
	SecretHash [32]byte // SHA-256 of the secret part
}

// validKeyID accepts 1-64 ASCII letters and digits, so the first
// underscore after the prefix unambiguously ends the id even though the
// base64url secret may itself contain underscores.
func validKeyID(id string) bool {
	if id == "" || len(id) > maxKeyIDLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9') {
			return false
		}
	}
	return true
}

// NewAPIKey mints an API key with the given key id (letters and digits
// only, for example the Crockford body of the key's UUIDv7) and a secret
// of 32 bytes from rand.
func NewAPIKey(rand io.Reader, keyID string) (APIKey, error) {
	if !validKeyID(keyID) {
		return APIKey{}, fmt.Errorf("%w: key id %q must be 1-%d letters or digits", ErrInvalidAPIKey, keyID, maxKeyIDLen)
	}
	secret, err := newSecret(rand)
	if err != nil {
		return APIKey{}, err
	}
	prefix := APIKeyPrefix + keyID
	return APIKey{ID: keyID, Prefix: prefix, Token: prefix + "_" + secret, SecretHash: HashToken(secret)}, nil
}

// ParsedAPIKey is a presented API key split into its parts.
type ParsedAPIKey struct {
	ID     string
	Prefix string // lookup key for identity.api_keys.prefix
	secret string
}

// ParseAPIKey splits "ffk_<key_id>_<secret>" and checks its shape. It does
// not authenticate the key; call Matches with the stored hash.
func ParseAPIKey(token string) (ParsedAPIKey, error) {
	rest, ok := strings.CutPrefix(token, APIKeyPrefix)
	if !ok {
		return ParsedAPIKey{}, fmt.Errorf("%w: missing %s prefix", ErrInvalidAPIKey, APIKeyPrefix)
	}
	id, secret, ok := strings.Cut(rest, "_")
	if !ok || !validKeyID(id) || !isSecret(secret) {
		return ParsedAPIKey{}, fmt.Errorf("%w: malformed key", ErrInvalidAPIKey)
	}
	return ParsedAPIKey{ID: id, Prefix: APIKeyPrefix + id, secret: secret}, nil
}

// Matches reports, in constant time, whether the presented secret hashes
// to storedHash.
func (k ParsedAPIKey) Matches(storedHash []byte) bool {
	h := HashToken(k.secret)
	return k.secret != "" && subtle.ConstantTimeCompare(h[:], storedHash) == 1
}
