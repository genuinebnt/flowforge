package identity

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
)

// RotationPeriod is how long a signing key stays active (spec 13.7).
const RotationPeriod = 90 * 24 * time.Hour

// KeyManagerConfig configures a KeyManager.
type KeyManagerConfig struct {
	Store   Store
	Secrets KeyStore
	Clock   core.Clock
	IDs     core.Rng
	Random  io.Reader // key seeds
	Issuer  string    // iss of every token minted
	// AccessTTL overrides the access-token lifetime (default 10 minutes).
	AccessTTL time.Duration
	// RotationPeriod overrides RotationPeriod.
	RotationPeriod time.Duration
}

// KeyManager owns the gateway's signing keys (spec 13.7): it loads
// identity.signing_keys, makes sure exactly one active key whose private
// half it can read exists, rotates keys (active -> retiring -> retired)
// and exposes the current Minter, the verifying key set and the JWKS.
// It implements auth.KeyResolver.
type KeyManager struct {
	cfg  KeyManagerConfig
	mu   sync.Mutex // serializes Reload/Rotate/Maintain
	cur  atomic.Pointer[keyState]
	idMu sync.Mutex
}

type keyState struct {
	set     *auth.KeySet
	minter  *auth.Minter
	active  SigningKeyRecord
	records []SigningKeyRecord
}

var _ auth.KeyResolver = (*KeyManager)(nil)

// NewKeyManager loads the key set, creating or rotating to a usable active
// key when needed.
func NewKeyManager(ctx context.Context, cfg KeyManagerConfig) (*KeyManager, error) {
	if cfg.Store == nil || cfg.Secrets == nil || cfg.Clock == nil || cfg.IDs == nil || cfg.Issuer == "" {
		return nil, fmt.Errorf("identity: key manager needs a store, key store, clock, ids and issuer")
	}
	if err := randomReader(cfg.Random); err != nil {
		return nil, err
	}
	if cfg.AccessTTL <= 0 {
		cfg.AccessTTL = auth.AccessTokenTTL
	}
	if cfg.RotationPeriod <= 0 {
		cfg.RotationPeriod = RotationPeriod
	}
	m := &KeyManager{cfg: cfg}
	if err := m.Reload(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

// Reload re-reads identity.signing_keys (other gateway instances may have
// rotated). If no active key with a readable private half exists, it
// rotates to a new one.
func (m *KeyManager) Reload(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.reloadLocked(ctx)
}

func (m *KeyManager) reloadLocked(ctx context.Context) error {
	recs, err := m.cfg.Store.ListSigningKeys(ctx)
	if err != nil {
		return fmt.Errorf("identity: list signing keys: %w", err)
	}
	var active *SigningKeyRecord
	for i := range recs {
		if recs[i].Status == auth.KeyActive && (active == nil || recs[i].CreatedAt.After(active.CreatedAt)) {
			active = &recs[i]
		}
	}
	if active == nil {
		return m.rotateLocked(ctx, recs)
	}
	seed, err := m.cfg.Secrets.GetPrivateKey(ctx, active.PrivateKeyRef)
	if errors.Is(err, ErrNotFound) {
		// The private half is gone (development key store after a
		// restart): keep verifying with it, sign with a new key.
		return m.rotateLocked(ctx, recs)
	}
	if err != nil {
		return fmt.Errorf("identity: read private key %s: %w", active.KID, err)
	}
	return m.install(recs, *active, seed)
}

func (m *KeyManager) install(recs []SigningKeyRecord, active SigningKeyRecord, seed []byte) error {
	sk, err := auth.NewSigningKey(active.KID, seed, auth.KeyActive)
	if err != nil {
		return fmt.Errorf("identity: signing key %s: %w", active.KID, err)
	}
	if !sk.Public().Key.Equal(active.PublicKey) {
		return fmt.Errorf("identity: private key of %s does not match its public key", active.KID)
	}
	pubs := make([]auth.PublicKey, 0, len(recs))
	for _, r := range recs {
		pubs = append(pubs, auth.PublicKey{KID: r.KID, Key: r.PublicKey, State: r.Status})
	}
	set, err := auth.NewKeySet(pubs...)
	if err != nil {
		return fmt.Errorf("identity: key set: %w", err)
	}
	signer, err := auth.NewSigner(sk)
	if err != nil {
		return err
	}
	minter, err := auth.NewMinter(auth.MinterConfig{
		Signer: signer, Clock: m.cfg.Clock, IDs: lockedRng{mu: &m.idMu, r: m.cfg.IDs}, Issuer: m.cfg.Issuer, AccessTTL: m.cfg.AccessTTL,
	})
	if err != nil {
		return err
	}
	m.cur.Store(&keyState{set: set, minter: minter, active: active, records: recs})
	return nil
}

// Rotate makes a new active key; the previous active key becomes retiring
// until every token it signed has expired.
func (m *KeyManager) Rotate(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	recs, err := m.cfg.Store.ListSigningKeys(ctx)
	if err != nil {
		return fmt.Errorf("identity: list signing keys: %w", err)
	}
	return m.rotateLocked(ctx, recs)
}

func (m *KeyManager) rotateLocked(ctx context.Context, recs []SigningKeyRecord) error {
	now := m.cfg.Clock.Now()
	retireAt := now.Add(m.cfg.AccessTTL + auth.DefaultSkew)
	for i, r := range recs {
		if r.Status == auth.KeyActive {
			if err := m.cfg.Store.SetSigningKeyStatus(ctx, r.KID, auth.KeyRetiring, retireAt); err != nil {
				return fmt.Errorf("identity: retire key %s: %w", r.KID, err)
			}
			recs[i].Status, recs[i].RetireAt = auth.KeyRetiring, retireAt
		}
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := io.ReadFull(m.cfg.Random, seed); err != nil {
		return fmt.Errorf("identity: read key seed: %w", err)
	}
	m.idMu.Lock()
	suffix := m.cfg.IDs.Uint64() & 0xffffff
	m.idMu.Unlock()
	kid := fmt.Sprintf("%s-%06x", now.UTC().Format("2006-01-02"), suffix)
	ref, err := m.cfg.Secrets.PutPrivateKey(ctx, kid, seed)
	if err != nil {
		return fmt.Errorf("identity: store private key: %w", err)
	}
	rec := SigningKeyRecord{
		KID: kid, Alg: auth.Algorithm, PublicKey: ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey),
		PrivateKeyRef: ref, Status: auth.KeyActive, CreatedAt: now,
	}
	if err := m.cfg.Store.PutSigningKey(ctx, rec); err != nil {
		return fmt.Errorf("identity: insert signing key: %w", err)
	}
	return m.install(append(recs, rec), rec, seed)
}

// Maintain rotates when the active key is older than the rotation period
// and retires retiring keys whose retire_at has passed. Call it
// periodically.
func (m *KeyManager) Maintain(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.reloadLocked(ctx); err != nil {
		return err
	}
	st := m.cur.Load()
	now := m.cfg.Clock.Now()
	changed := false
	for _, r := range st.records {
		if r.Status == auth.KeyRetiring && !r.RetireAt.IsZero() && !now.Before(r.RetireAt) {
			if err := m.cfg.Store.SetSigningKeyStatus(ctx, r.KID, auth.KeyRetired, r.RetireAt); err != nil {
				return fmt.Errorf("identity: retire key %s: %w", r.KID, err)
			}
			changed = true
		}
	}
	if now.Sub(st.active.CreatedAt) >= m.cfg.RotationPeriod {
		recs, err := m.cfg.Store.ListSigningKeys(ctx)
		if err != nil {
			return err
		}
		return m.rotateLocked(ctx, recs)
	}
	if changed {
		return m.reloadLocked(ctx)
	}
	return nil
}

// Minter returns the minter for the current active key.
func (m *KeyManager) Minter() *auth.Minter { return m.cur.Load().minter }

// KeySet returns the current verifying key set (active, retiring and
// retired keys; retired ones are rejected and not published).
func (m *KeyManager) KeySet() *auth.KeySet { return m.cur.Load().set }

// ActiveKID returns the kid of the active key.
func (m *KeyManager) ActiveKID() string { return m.cur.Load().active.KID }

// ResolveKey implements auth.KeyResolver.
func (m *KeyManager) ResolveKey(ctx context.Context, kid string) (auth.PublicKey, error) {
	return m.cur.Load().set.ResolveKey(ctx, kid)
}

// JWKS returns the public JWKS document (spec 13.7).
func (m *KeyManager) JWKS() ([]byte, error) { return m.cur.Load().set.MarshalJSON() }

// lockedRng serializes a core.Rng shared by several users.
type lockedRng struct {
	mu *sync.Mutex
	r  core.Rng
}

func (l lockedRng) Uint64() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.r.Uint64()
}
