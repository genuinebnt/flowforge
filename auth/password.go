package auth

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Params are Argon2id cost parameters.
type Params struct {
	Memory  uint32 // KiB
	Time    uint32 // iterations
	Threads uint8  // parallelism
	SaltLen uint32 // bytes
	KeyLen  uint32 // bytes
}

// PolicyParams returns the spec 13.1 parameters: 19 MiB, 2 iterations,
// parallelism 1, 16-byte salt, 32-byte output.
func PolicyParams() Params {
	return Params{Memory: 19 * 1024, Time: 2, Threads: 1, SaltLen: 16, KeyLen: 32}
}

// MeetsPolicy reports whether p is at least as strong as PolicyParams in
// every dimension.
func (p Params) MeetsPolicy() bool {
	f := PolicyParams()
	return p.Memory >= f.Memory && p.Time >= f.Time && p.Threads >= f.Threads &&
		p.SaltLen >= f.SaltLen && p.KeyLen >= f.KeyLen
}

// Bounds accepted when parsing stored hashes, so a corrupt or hostile row
// cannot make verification allocate gigabytes or spin for minutes.
const (
	maxMemoryKiB = 1 << 20 // 1 GiB
	maxTime      = 64
	maxSaltLen   = 64
	minSaltLen   = 8
	maxKeyLen    = 64
	minKeyLen    = 16
)

func (p Params) validate() error {
	switch {
	case p.Threads < 1:
		return fmt.Errorf("%w: parallelism must be at least 1", ErrInvalidHash)
	case p.Time < 1 || p.Time > maxTime:
		return fmt.Errorf("%w: iterations %d out of range", ErrInvalidHash, p.Time)
	case p.Memory < 8*uint32(p.Threads) || p.Memory > maxMemoryKiB:
		return fmt.Errorf("%w: memory %d KiB out of range", ErrInvalidHash, p.Memory)
	case p.SaltLen < minSaltLen || p.SaltLen > maxSaltLen:
		return fmt.Errorf("%w: salt length %d out of range", ErrInvalidHash, p.SaltLen)
	case p.KeyLen < minKeyLen || p.KeyLen > maxKeyLen:
		return fmt.Errorf("%w: key length %d out of range", ErrInvalidHash, p.KeyLen)
	}
	return nil
}

// HasherOption configures NewHasher.
type HasherOption func(*hasherOptions)

type hasherOptions struct{ allowWeak bool }

// AllowWeakParams lets NewHasher accept parameters below the policy floor.
// It exists for tests, which would otherwise spend most of their time in
// Argon2; never use it in production wiring.
func AllowWeakParams() HasherOption { return func(o *hasherOptions) { o.allowWeak = true } }

// Hasher hashes and verifies passwords with Argon2id. It is safe for
// concurrent use if its random reader is (crypto/rand.Reader is).
type Hasher struct {
	params Params
	rand   io.Reader
}

// NewHasher returns a Hasher that draws salts from rand and hashes with p.
// It returns ErrWeakParams when p is below PolicyParams, unless
// AllowWeakParams is given.
func NewHasher(rand io.Reader, p Params, opts ...HasherOption) (*Hasher, error) {
	var o hasherOptions
	for _, opt := range opts {
		opt(&o)
	}
	if rand == nil {
		return nil, fmt.Errorf("%w: hasher needs a random reader", ErrConfig)
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	if !o.allowWeak && !p.MeetsPolicy() {
		return nil, fmt.Errorf("%w: %+v", ErrWeakParams, p)
	}
	return &Hasher{params: p, rand: rand}, nil
}

// Params returns the parameters new hashes are made with.
func (h *Hasher) Params() Params { return h.params }

// Hash returns the PHC string of password with a fresh salt:
// $argon2id$v=19$m=<KiB>,t=<iterations>,p=<parallelism>$<salt>$<hash>, with
// salt and hash in unpadded standard base64.
//
// PORT: the argon2 crate's PasswordHash reads and writes this format; the
// password is hashed as its raw UTF-8 bytes with no normalization
// (docs/PORTING_RUST.md 4.4).
func (h *Hasher) Hash(password string) (string, error) {
	salt := make([]byte, h.params.SaltLen)
	if _, err := io.ReadFull(h.rand, salt); err != nil {
		return "", fmt.Errorf("auth: read salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, h.params.Time, h.params.Memory, h.params.Threads, h.params.KeyLen)
	return encodePHC(h.params, salt, key), nil
}

// Verify reports whether password matches encoded, using the parameters
// stored in encoded. The comparison is constant-time. The error is non-nil
// only when encoded is not a valid PHC string (ErrInvalidHash).
func (h *Hasher) Verify(password, encoded string) (bool, error) {
	p, salt, want, err := parsePHC(encoded)
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.Memory, p.Threads, p.KeyLen)
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// NeedsRehash reports whether encoded was made with parameters other than
// the Hasher's (or cannot be parsed), so a successful login should store a
// fresh hash (spec 13.1).
func (h *Hasher) NeedsRehash(encoded string) bool {
	p, _, _, err := parsePHC(encoded)
	return err != nil || p != h.params
}

const phcVersion = argon2.Version // 0x13 = 19

var b64 = base64.RawStdEncoding.Strict()

func encodePHC(p Params, salt, key []byte) string {
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		phcVersion, p.Memory, p.Time, p.Threads, b64.EncodeToString(salt), b64.EncodeToString(key))
}

// parsePHC parses the canonical form written by encodePHC and nothing
// else: fields in order m, t, p, canonical decimal numbers, unpadded
// base64.
func parsePHC(s string) (Params, []byte, []byte, error) {
	var p Params
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" {
		return p, nil, nil, fmt.Errorf("%w: not an argon2id PHC string", ErrInvalidHash)
	}
	if parts[2] != "v="+strconv.Itoa(phcVersion) {
		return p, nil, nil, fmt.Errorf("%w: unsupported version %q", ErrInvalidHash, parts[2])
	}
	fields := strings.Split(parts[3], ",")
	if len(fields) != 3 {
		return p, nil, nil, fmt.Errorf("%w: want m, t and p parameters", ErrInvalidHash)
	}
	var vals [3]uint64
	for i, name := range []string{"m", "t", "p"} {
		v, ok := strings.CutPrefix(fields[i], name+"=")
		if !ok {
			return p, nil, nil, fmt.Errorf("%w: parameter %d is not %s", ErrInvalidHash, i, name)
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil || strconv.FormatUint(n, 10) != v {
			return p, nil, nil, fmt.Errorf("%w: bad %s value %q", ErrInvalidHash, name, v)
		}
		vals[i] = n
	}
	if vals[2] > 255 {
		return p, nil, nil, fmt.Errorf("%w: parallelism %d out of range", ErrInvalidHash, vals[2])
	}
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return p, nil, nil, fmt.Errorf("%w: salt: %w", ErrInvalidHash, err)
	}
	key, err := b64.DecodeString(parts[5])
	if err != nil {
		return p, nil, nil, fmt.Errorf("%w: hash: %w", ErrInvalidHash, err)
	}
	p = Params{
		Memory: uint32(vals[0]), Time: uint32(vals[1]), Threads: uint8(vals[2]),
		SaltLen: uint32(len(salt)), KeyLen: uint32(len(key)),
	}
	if err := p.validate(); err != nil {
		return Params{}, nil, nil, err
	}
	return p, salt, key, nil
}
