package auth_test

import (
	"crypto/sha256"
	"encoding/binary"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
)

// detReader is a deterministic io.Reader standing in for crypto/rand in
// tests. It is not safe for concurrent use.
type detReader struct{ rng *core.SplitMix64 }

func newReader(seed uint64) *detReader { return &detReader{core.NewSplitMix64(seed)} }

func (r *detReader) Read(p []byte) (int, error) {
	var b [8]byte
	for i := 0; i < len(p); i += 8 {
		binary.LittleEndian.PutUint64(b[:], r.rng.Uint64())
		copy(p[i:], b[:])
	}
	return len(p), nil
}

// fastParams are reduced Argon2id parameters for unit tests.
func fastParams() auth.Params {
	return auth.Params{Memory: 64, Time: 1, Threads: 1, SaltLen: 16, KeyLen: 32}
}

func fastHasher(t testing.TB, seed uint64) *auth.Hasher {
	t.Helper()
	h, err := auth.NewHasher(newReader(seed), fastParams(), auth.AllowWeakParams())
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// testKey derives a deterministic Ed25519 key from its kid.
func testKey(t testing.TB, kid string, state auth.KeyState) auth.SigningKey {
	t.Helper()
	seed := sha256.Sum256([]byte("flowforge conformance key " + kid))
	k, err := auth.NewSigningKey(kid, seed[:], state)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func mustSigner(t testing.TB, k auth.SigningKey) *auth.Signer {
	t.Helper()
	k.State = auth.KeyActive // tests sign with keys in any state
	s, err := auth.NewSigner(k)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustKeySet(t testing.TB, keys ...auth.PublicKey) *auth.KeySet {
	t.Helper()
	s, err := auth.NewKeySet(keys...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

const testIssuer = "https://api.flowforge.example"

// specNow is one minute and forty seconds after the spec 13.2 example iat.
var specNow = time.Unix(1794489700, 0).UTC()

func baseClaims() auth.Claims {
	return auth.Claims{
		Issuer: testIssuer, Audience: auth.AudienceAPI,
		Subject: "usr_01JD8K0A", TenantID: "ten_01JD8K0B", Role: auth.RoleOwner,
		SessionID: "ses_01JD8K0E", SessionVersion: 1, ID: "01JD8K0F9Z",
		IssuedAt: 1794489600, ExpiresAt: 1794490200,
	}
}
