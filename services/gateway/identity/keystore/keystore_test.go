package keystore_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
	"github.com/genuinebnt/flowforge/services/gateway/identity/keystore"
	"github.com/genuinebnt/flowforge/testkit"
)

func seedHex(t *testing.T) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b)
}

func manager(t *testing.T, st identity.Store, ks identity.KeyStore, clock core.Clock) *identity.KeyManager {
	t.Helper()
	m, err := identity.NewKeyManager(context.Background(), identity.KeyManagerConfig{
		Store: st, Secrets: ks, Clock: clock, IDs: core.NewSplitMix64(uint64(clock.Now().UnixNano())), Random: rand.Reader, Issuer: "https://api.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestRegress_G_OPS_12_EnvKeyRotation: the env store reads the Terraform
// names; rotation is operator-driven: Rotate refuses, a new FF_SIGNING_KID
// becomes active on restart and the previous key retires but keeps
// verifying until its tokens expire; the single FF_SIGNING_KEY still works.
func TestRegress_G_OPS_12_EnvKeyRotation(t *testing.T) {
	ctx := context.Background()
	clock := testkit.NewFakeClock(time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC))
	st := identity.NewMemStore()
	env := map[string]string{"FF_SIGNING_KEY_2026_11_A": seedHex(t), "FF_SIGNING_KEY_2026_12_B": seedHex(t)}
	getenv := func(k string) string { return env[k] }

	a, err := keystore.FromEnv("2026-11-a", getenv)
	if err != nil {
		t.Fatal(err)
	}
	m := manager(t, st, a, clock)
	if m.ActiveKID() != "2026-11-a" {
		t.Fatalf("active %s", m.ActiveKID())
	}
	tok, _, err := m.Minter().Internal(auth.InternalGrant{TenantID: core.MustParseUUID("0190a1b2-0000-7000-8000-00000000a001"), Actor: "service:x", Role: auth.RoleAnalyst})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Rotate(ctx); !errors.Is(err, identity.ErrOperatorRotation) {
		t.Fatalf("rotate with env keys: %v", err)
	}
	// The operator provisions kid b and restarts.
	b, _ := keystore.FromEnv("2026-12-b", getenv)
	m2 := manager(t, st, b, clock)
	if m2.ActiveKID() != "2026-12-b" {
		t.Fatalf("active after kid switch %s", m2.ActiveKID())
	}
	v, _ := auth.NewVerifier(auth.InternalVerifierConfig(m2, clock, "https://api.test"))
	if _, err := v.Verify(ctx, tok); err != nil {
		t.Fatalf("token of the retiring key: %v", err)
	}
	var status = map[string]auth.KeyState{}
	recs, _ := st.ListSigningKeys(ctx)
	for _, r := range recs {
		status[r.KID] = r.Status
	}
	if status["2026-11-a"] != auth.KeyRetiring || status["2026-12-b"] != auth.KeyActive || len(recs) != 2 {
		t.Fatalf("statuses %v", status)
	}
	// Maintenance retires a, never rotates away from b.
	clock.Advance(2 * identity.RotationPeriod)
	if err := m2.Maintain(ctx); err != nil || m2.ActiveKID() != "2026-12-b" {
		t.Fatalf("maintain: %v %s", err, m2.ActiveKID())
	}
	// Switching back to a retired kid is refused.
	if _, err := identity.NewKeyManager(ctx, identity.KeyManagerConfig{Store: st, Secrets: a, Clock: clock, IDs: core.NewSplitMix64(1), Random: rand.Reader, Issuer: "x"}); err == nil {
		t.Fatal("re-activated a retired kid")
	}
	// Compatibility: FF_SIGNING_KEY alone.
	legacy, err := keystore.FromEnv("legacy", func(k string) string { return map[string]string{"FF_SIGNING_KEY": env["FF_SIGNING_KEY_2026_11_A"]}[k] })
	if err != nil || legacy.Ref != "env://FF_SIGNING_KEY" {
		t.Fatalf("legacy: %+v %v", legacy, err)
	}
	if _, err := keystore.FromEnv("2026-11-a", func(string) string { return "" }); err == nil {
		t.Fatal("missing key accepted")
	}
}

// TestRegress_G_SVC_03_DirKeyStore: two gateway instances sharing a
// database and a key directory: a rotation by one is picked up by the
// other on Reload, and both sign with keys the other can verify.
func TestRegress_G_SVC_03_DirKeyStore(t *testing.T) {
	ctx := context.Background()
	clock := testkit.NewFakeClock(time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC))
	st := identity.NewMemStore()
	dir := keystore.Dir{Path: t.TempDir()}
	m1 := manager(t, st, dir, clock)
	m2 := manager(t, st, dir, clock)
	if m1.ActiveKID() != m2.ActiveKID() {
		t.Fatalf("instances disagree: %s %s", m1.ActiveKID(), m2.ActiveKID())
	}
	old := m1.ActiveKID()
	if err := m1.Rotate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m2.Reload(ctx); err != nil {
		t.Fatal(err)
	}
	if m2.ActiveKID() == old || m2.ActiveKID() != m1.ActiveKID() {
		t.Fatalf("rotation not shared: %s %s (old %s)", m1.ActiveKID(), m2.ActiveKID(), old)
	}
	fi, err := os.Stat(filepath.Join(dir.Path, m1.ActiveKID()+".key"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	if _, err := dir.GetPrivateKey(ctx, "file:///etc/passwd"); err == nil {
		t.Fatal("read a file outside the key directory")
	}
}
