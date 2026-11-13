package contentstore

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
)

// Envelope encryption (spec 34.4; V2 §6.4): each tenant has data keys (DEKs), 32
// random bytes each, stored only wrapped by a key-encryption key (KEK).
// The KEK is the KeyEncrypter port: a local AES-256-GCM key in
// development (LocalKEK), a KMS key in production (Cloud KMS
// Encrypt/Decrypt with the same additional data fits the interface).
// Findings and redaction seals are AES-256-GCM under the tenant's "enc"
// DEK; keyed hashes of secrets are HMAC-SHA256 under its "hash" DEK.

// Data key purposes.
const (
	PurposeEncrypt = "enc"
	PurposeHash    = "hash"
)

// KeyEncrypter wraps and unwraps data keys (the KEK port). Implementations
// must bind the wrapped key to the tenant (additional data), so a wrapped
// key copied to another tenant does not unwrap.
type KeyEncrypter interface {
	// ID names the KEK (recorded with every wrapped key).
	ID() string
	// Wrap encrypts dek for tenant.
	Wrap(ctx context.Context, tenant core.UUID, dek []byte) ([]byte, error)
	// Unwrap reverses Wrap; a key wrapped for another tenant or under
	// another KEK must fail (LocalKEK: ErrKeyUnwrap).
	Unwrap(ctx context.Context, tenant core.UUID, wrapped []byte) ([]byte, error)
}

// ErrKeyUnwrap: a wrapped key or ciphertext did not authenticate (wrong
// KEK, wrong tenant, tampered bytes).
var ErrKeyUnwrap = errors.New("contentstore: key or ciphertext failed authentication")

// LocalKEK is a KeyEncrypter over a local 32-byte key (the development
// key; tests). Wrapped form: version 0x01 || nonce(12) || GCM(ciphertext ||
// tag), additional data "ff-content-kek|" || tenant UUID.
type LocalKEK struct {
	KeyID string
	Key   [32]byte
	// Rng draws nonces (core.SystemRng in production); calls are
	// serialized, so it need not be safe for concurrent use.
	Rng core.Rng

	mu sync.Mutex
}

// ID implements KeyEncrypter.
func (k *LocalKEK) ID() string { return "local:" + k.KeyID }

// Wrap implements KeyEncrypter.
func (k *LocalKEK) Wrap(_ context.Context, tenant core.UUID, dek []byte) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return seal(k.Key[:], k.Rng, []byte("ff-content-kek|"+tenant.String()), dek)
}

// Unwrap implements KeyEncrypter.
func (k *LocalKEK) Unwrap(_ context.Context, tenant core.UUID, wrapped []byte) ([]byte, error) {
	return open(k.Key[:], []byte("ff-content-kek|"+tenant.String()), wrapped)
}

// sealVersion is the first byte of every ciphertext this package writes.
const sealVersion = 0x01

// seal encrypts plaintext with AES-256-GCM under key with a fresh 96-bit
// nonce: 0x01 || nonce || ciphertext || tag.
//
// PORT: the nonce is 12 bytes from two Rng draws (little-endian u64, then
// the low 4 bytes of the second); a random nonce is safe for well under
// 2^32 messages per key, far above one tenant's findings.
func seal(key []byte, rng core.Rng, aad, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 1+g.NonceSize(), 1+g.NonceSize()+len(plaintext)+g.Overhead())
	out[0] = sealVersion
	var n [16]byte
	binary.LittleEndian.PutUint64(n[0:], rng.Uint64())
	binary.LittleEndian.PutUint64(n[8:], rng.Uint64())
	copy(out[1:], n[:g.NonceSize()])
	return g.Seal(out, out[1:1+g.NonceSize()], plaintext, aad), nil
}

// open reverses seal.
func open(key, aad, ct []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(ct) < 1+g.NonceSize()+g.Overhead() || ct[0] != sealVersion {
		return nil, ErrKeyUnwrap
	}
	pt, err := g.Open(nil, ct[1:1+g.NonceSize()], ct[1+g.NonceSize():], aad)
	if err != nil {
		return nil, ErrKeyUnwrap
	}
	return pt, nil
}

// Keyring hands out a tenant's data keys: it creates them on first use
// (random, wrapped by the KEK, stored through the Repo), unwraps them and
// caches the plaintext keys in memory. Safe for concurrent use.
type Keyring struct {
	Repo Repo
	KEK  KeyEncrypter
	Rng  core.Rng
	// Clock stamps created_at of new keys.
	Clock core.Clock

	mu    sync.Mutex
	cache map[string][]byte // tenant|purpose/version -> key
	cur   map[string]string // tenant|purpose -> current key id
	rngMu sync.Mutex
}

// draw returns one Uint64 from Rng under a lock (core.Rng need not be
// safe for concurrent use).
func (kr *Keyring) draw() uint64 {
	kr.rngMu.Lock()
	defer kr.rngMu.Unlock()
	return kr.Rng.Uint64()
}

// lockedDraw adapts draw to core.Rng.
type lockedDraw struct{ kr *Keyring }

func (l lockedDraw) Uint64() uint64 { return l.kr.draw() }

// Key returns the tenant's current data key for purpose and its id
// ("<purpose>/<version>").
func (kr *Keyring) Key(ctx context.Context, tenant core.UUID, purpose string) ([]byte, string, error) {
	kr.mu.Lock()
	if kr.cache == nil {
		kr.cache, kr.cur = map[string][]byte{}, map[string]string{}
	}
	if id, ok := kr.cur[tenant.String()+"|"+purpose]; ok {
		k := kr.cache[tenant.String()+"|"+id]
		kr.mu.Unlock()
		return k, id, nil
	}
	kr.mu.Unlock()
	var dk DataKey
	err := kr.Repo.InTx(ctx, tenant, func(tx Tx) error {
		ks, err := tx.DataKeys(ctx)
		if err != nil {
			return err
		}
		var best *DataKey
		for i := range ks {
			if ks[i].Purpose == purpose && (best == nil || ks[i].Version > best.Version) {
				best = &ks[i]
			}
		}
		if best != nil {
			dk = *best
			return nil
		}
		raw := make([]byte, 32)
		for i := 0; i < 32; i += 8 {
			binary.LittleEndian.PutUint64(raw[i:], kr.draw())
		}
		w, err := kr.KEK.Wrap(ctx, tenant, raw)
		if err != nil {
			return err
		}
		dk = DataKey{Tenant: tenant, Purpose: purpose, Version: 1, KEKID: kr.KEK.ID(), Wrapped: w, CreatedAt: kr.Clock.Now()}
		ok, err := tx.InsertDataKey(ctx, dk)
		if err != nil {
			return err
		}
		if !ok {
			return conflict("data key %s created concurrently", dk.ID())
		}
		return nil
	})
	if errors.Is(err, ErrConflict) {
		return kr.Key(ctx, tenant, purpose)
	}
	if err != nil {
		return nil, "", internal("data key", err)
	}
	k, err := kr.unwrap(ctx, dk)
	if err != nil {
		return nil, "", err
	}
	kr.mu.Lock()
	kr.cur[tenant.String()+"|"+purpose] = dk.ID()
	kr.mu.Unlock()
	return k, dk.ID(), nil
}

// KeyByID returns the tenant's data key with the given id (decrypting an
// older ciphertext after a rotation).
func (kr *Keyring) KeyByID(ctx context.Context, tenant core.UUID, id string) ([]byte, error) {
	kr.mu.Lock()
	if kr.cache == nil {
		kr.cache, kr.cur = map[string][]byte{}, map[string]string{}
	}
	if k, ok := kr.cache[tenant.String()+"|"+id]; ok {
		kr.mu.Unlock()
		return k, nil
	}
	kr.mu.Unlock()
	purpose, v, ok := strings.Cut(id, "/")
	ver, err := strconv.Atoi(v)
	if !ok || err != nil {
		return nil, integrity("data key id %q", id)
	}
	var dk DataKey
	found := false
	err = kr.Repo.InTx(ctx, tenant, func(tx Tx) error {
		ks, err := tx.DataKeys(ctx)
		if err != nil {
			return err
		}
		i := slices.IndexFunc(ks, func(k DataKey) bool { return k.Purpose == purpose && k.Version == ver })
		if i >= 0 {
			dk, found = ks[i], true
		}
		return nil
	})
	if err != nil {
		return nil, internal("data key", err)
	}
	if !found {
		return nil, integrity("data key %s not found", id)
	}
	return kr.unwrap(ctx, dk)
}

func (kr *Keyring) unwrap(ctx context.Context, dk DataKey) ([]byte, error) {
	if dk.KEKID != kr.KEK.ID() {
		return nil, integrity("data key %s is wrapped by %s, not %s", dk.ID(), dk.KEKID, kr.KEK.ID())
	}
	k, err := kr.KEK.Unwrap(ctx, dk.Tenant, dk.Wrapped)
	if err != nil {
		return nil, integrity("unwrap data key %s: %v", dk.ID(), err)
	}
	kr.mu.Lock()
	kr.cache[dk.Tenant.String()+"|"+dk.ID()] = k
	kr.mu.Unlock()
	return k, nil
}

// encrypt seals plaintext under the tenant's current encryption key;
// aad binds the ciphertext to its row (tenant, record kind, id).
func (kr *Keyring) encrypt(ctx context.Context, tenant core.UUID, aad string, plaintext []byte) ([]byte, string, error) {
	k, id, err := kr.Key(ctx, tenant, PurposeEncrypt)
	if err != nil {
		return nil, "", err
	}
	ct, err := seal(k, lockedDraw{kr}, []byte(aad), plaintext)
	if err != nil {
		return nil, "", internal("encrypt", err)
	}
	return ct, id, nil
}

// decrypt opens a ciphertext written by encrypt.
func (kr *Keyring) decrypt(ctx context.Context, tenant core.UUID, keyID, aad string, ct []byte) ([]byte, error) {
	k, err := kr.KeyByID(ctx, tenant, keyID)
	if err != nil {
		return nil, err
	}
	pt, err := open(k, []byte(aad), ct)
	if err != nil {
		return nil, integrity("decrypt: %v", err)
	}
	return pt, nil
}

// hasher returns the tenant's keyed hasher for secrets
// (oam.SecretKeyedHash: HMAC-SHA256 under the "hash" data key, lower-case
// hex), so keyed hashes are stable across data-key rotations of "enc".
func (kr *Keyring) hasher(ctx context.Context, tenant core.UUID) (content.TenantKeyHasher, error) {
	k, _, err := kr.Key(ctx, tenant, PurposeHash)
	if err != nil {
		return content.TenantKeyHasher{}, err
	}
	return content.TenantKeyHasher{Key: k}, nil
}
