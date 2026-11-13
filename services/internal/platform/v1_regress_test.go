package platform_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// TestRegress_G_LIM_05_GroupsReachEngineCaller: role groups in an internal
// token become Identity.Groups and the engine Caller's roles, so
// engine.DecideApproval matches approval.role against them; the token
// cache keeps identities with different groups apart.
func TestRegress_G_LIM_05_GroupsReachEngineCaller(t *testing.T) {
	ctx := context.Background()
	keys, err := platform.NewDevKeys("iss")
	if err != nil {
		t.Fatal(err)
	}
	clock := &core.SystemClock{}
	tokens, _ := keys.Tokens(clock)
	v, _ := keys.Verifier(clock)
	tenant := core.MustParseUUID("0190a1b2-0000-7000-8000-00000000a001")
	with := platform.Identity{Tenant: tenant, Actor: "user:0190a1b2-0000-7000-8000-0000000000a1", Role: auth.RoleApprover, Groups: []string{"finance_ap"}}
	without := with
	without.Groups = nil
	t1, err := tokens.Token(ctx, with)
	if err != nil {
		t.Fatal(err)
	}
	t2, _ := tokens.Token(ctx, without)
	if t1 == t2 {
		t.Fatal("token cache ignores groups")
	}
	id, err := platform.IdentityFromBearer(ctx, v, "Bearer "+t1)
	if err != nil || !slices.Equal(id.Groups, []string{"finance_ap"}) {
		t.Fatalf("identity %+v %v", id, err)
	}
	c := id.EngineCaller()
	if !c.HasRole("finance_ap") || !c.HasRole("approver") || c.HasRole("lead") {
		t.Fatalf("caller roles %v", c.Roles)
	}
}

// TestRegress_G_SVC_04_LocalSecrets: env:// and file:// references resolve
// locally; files outside FileSecrets.Root and unknown schemes fail closed.
func TestRegress_G_SVC_04_LocalSecrets(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	f := filepath.Join(dir, "hook")
	if err := os.WriteFile(f, []byte("  s3cret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := platform.LocalSecrets(func(k string) string { return map[string]string{"HOOK": "from-env"}[k] }, nil)
	for ref, want := range map[string]string{"env://HOOK": "from-env", "file://" + f: "s3cret"} {
		if got, err := r.Resolve(ctx, ref); err != nil || string(got) != want {
			t.Errorf("%s = %q %v", ref, got, err)
		}
	}
	if _, err := r.Resolve(ctx, "env://MISSING"); !errors.Is(err, platform.ErrSecretNotFound) {
		t.Fatalf("missing env: %v", err)
	}
	if _, err := r.Resolve(ctx, "sm://projects/p/secrets/s/versions/1"); !errors.Is(err, platform.ErrSecretRef) {
		t.Fatalf("sm without adapter: %v", err)
	}
	if _, err := (platform.FileSecrets{Root: filepath.Join(dir, "sub")}).Resolve(ctx, "file://"+f); !errors.Is(err, platform.ErrSecretRef) {
		t.Fatalf("file outside root: %v", err)
	}
	if _, err := r.Resolve(ctx, "file://relative"); !errors.Is(err, platform.ErrSecretRef) {
		t.Fatalf("relative file: %v", err)
	}
}

// TestRegress_G_OPS_12_TerraformKeyNames: the service config reads the
// signing key under the name Terraform mounts it (FF_SIGNING_KEY_<KID>)
// and still accepts the single FF_SIGNING_KEY.
func TestRegress_G_OPS_12_TerraformKeyNames(t *testing.T) {
	hexSeed := "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	env := map[string]string{"FF_SIGNING_KID": "2026-11-a", "FF_SIGNING_KEY_2026_11_A": hexSeed, "FF_REDIS_CA": "pem", "FF_PUBSUB_PROJECT": "proj"}
	c, err := platform.ConfigFromEnv("engine", func(k string) string { return env[k] })
	if err != nil || len(c.SigningSeed) != 32 || c.SigningKID != "2026-11-a" || c.RedisCA != "pem" || c.SecretProject != "proj" {
		t.Fatalf("%+v %v", c, err)
	}
	if platform.SigningKeyVar("2026-11-a") != "FF_SIGNING_KEY_2026_11_A" {
		t.Fatal(platform.SigningKeyVar("2026-11-a"))
	}
	legacy := map[string]string{"FF_SIGNING_KID": "k", "FF_SIGNING_KEY": hexSeed}
	if c, err := platform.ConfigFromEnv("engine", func(k string) string { return legacy[k] }); err != nil || len(c.SigningSeed) != 32 {
		t.Fatalf("legacy: %+v %v", c, err)
	}
	bad := map[string]string{"FF_SIGNING_KID": "k", "FF_SIGNING_KEY_K": "short"}
	if _, err := platform.ConfigFromEnv("engine", func(k string) string { return bad[k] }); err == nil {
		t.Fatal("bad seed accepted")
	}
}
