package gatewayd_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"regexp"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/gateway/gatewayd"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/wire"
)

// TestRegress_G_SVC_03_SharedEnvKeys: a gateway and a standalone internal
// service (the scope service, wired as cmd/scope wires it) share the
// Terraform key layout, FF_SIGNING_KID plus FF_SIGNING_KEY_<KID>. The
// service verifies the gateway's internal tokens through the gateway's
// JWKS (FF_JWKS_URL), the gateway verifies nothing it did not publish, the
// service's own tokens (signed with the same env key, read through
// platform.ConfigFromEnv) verify too, and a restarted gateway keeps the
// same kid instead of generating a new key (G-SVC-03, G-OPS-12).
func TestRegress_G_SVC_03_SharedEnvKeys(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a gateway and a service")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &logBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	seed := make([]byte, 32)
	if _, err := rand.Read(seed); err != nil {
		t.Fatal(err)
	}
	gwAddr := freeAddr(t)
	issuer := "http://" + gwAddr
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	scopeAddr := lis.Addr().String()
	env := map[string]string{
		"FF_ENV": "dev", "FF_JWT_ISSUER": issuer, "FF_GATEWAY_ADDR": gwAddr, "FF_COOKIE_INSECURE": "true",
		"FF_SIGNING_KID": "2026-11-a", "FF_SIGNING_KEY_2026_11_A": hex.EncodeToString(seed),
		"FF_JWKS_URL": issuer + "/.well-known/jwks.json",
	}
	for _, v := range gatewayd.BackendVars {
		env[v] = scopeAddr
	}
	getenv := func(k string) string { return env[k] }

	// The service process: configuration, keys and wiring as services/app.
	cfg, err := platform.ConfigFromEnv("scope", getenv)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SigningKID != "2026-11-a" || len(cfg.SigningSeed) != 32 {
		t.Fatalf("service did not read FF_SIGNING_KEY_<KID>: %+v", cfg)
	}
	clock := &core.SystemClock{}
	keys, err := platform.LoadKeys(ctx, cfg, clock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := keys.Verifier(clock)
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := keys.Tokens(clock)
	if err != nil {
		t.Fatal(err)
	}
	wenv := &wire.Env{Service: "scope", Logger: platform.DiscardLogger(), Metrics: platform.NewMetrics(nil), Clock: clock, Verifier: verifier, Tokens: tokens}
	comp, _ := wire.Scope(wenv, wire.ScopeOptions{})
	served := make(chan error, 1)
	go func() { served <- wire.Serve(ctx, wenv, lis, comp) }()

	// The gateway process.
	gcfg, err := gatewayd.LoadConfig(getenv)
	if err != nil {
		t.Fatal(err)
	}
	gcfg.ShutdownGrace = time.Second
	gw, err := gatewayd.New(ctx, gcfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	if gw.Keys().ActiveKID() != "2026-11-a" {
		t.Fatalf("gateway signs with %q, not the provisioned key", gw.Keys().ActiveKID())
	}
	gwDone := make(chan error, 1)
	go func() { gwDone <- gw.Serve(ctx, gcfg.Backends) }()
	defer func() {
		cancel()
		<-gwDone
		<-served
	}()

	c := &client{t: t, base: issuer + "/v1"}
	c.waitReady(issuer + "/readyz")
	signup := c.do("POST", "/auth/signup", map[string]any{"email": "keys@example.com", "password": "a long enough passphrase 42", "tenant_name": "Keys"}, 202)
	token := c.waitFor(func() (string, bool) {
		m := regexp.MustCompile(`token=([A-Za-z0-9_-]+)`).FindStringSubmatch(logs.String())
		if m == nil {
			return "", false
		}
		return m[1], true
	})
	c.do("POST", "/auth/verify-email", map[string]any{"token": token}, 200)
	login := c.do("POST", "/auth/login", map[string]any{"email": "keys@example.com", "password": "a long enough passphrase 42", "tenant_id": signup["tenant_id"]}, 200)
	c.bearer = login["access_token"].(string)
	// The scope service accepts the gateway's internal token: it fetched
	// the env key's public half from the gateway's JWKS.
	prog := c.do("POST", "/programs", map[string]any{"name": "Shared keys", "scope": map[string]any{
		"rules":    []any{map[string]any{"id": "r1", "effect": "allow", "kind": "domain", "value": "acme.test"}},
		"activity": map[string]any{"max_class": "passive", "windows": []any{}},
	}}, 201)

	// A token the service itself mints (service-to-service calls) carries
	// the same kid and verifies against the gateway's JWKS.
	tenant, err := core.ParsePublicID(core.PrefixTenant, signup["tenant_id"].(string))
	if err != nil {
		t.Fatal(err)
	}
	conn, err := platform.Dial(scopeAddr, platform.ClientOptions{Tokens: tokens})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	sctx := platform.WithIdentity(ctx, platform.Identity{Tenant: tenant, Actor: platform.ServiceActor("engine"), Role: auth.RoleAnalyst})
	if _, err := pb.NewScopeServiceClient(conn).GetProgram(sctx, &pb.GetProgramRequest{ProgramId: prog["id"].(string)}); err != nil {
		t.Fatalf("service-minted token rejected: %v", err)
	}

	// A restart (a second instance over its own store) converges on the
	// same kid and public key: nothing an instance signed becomes
	// unverifiable.
	gw2, err := gatewayd.New(ctx, gcfg, log)
	if err != nil {
		t.Fatal(err)
	}
	defer gw2.Close()
	k1, err := gw.Keys().ResolveKey(ctx, "2026-11-a")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := gw2.Keys().ResolveKey(ctx, "2026-11-a")
	if err != nil || !k1.Key.Equal(k2.Key) || gw2.Keys().ActiveKID() != "2026-11-a" {
		t.Fatalf("restart changed the signing key: %v %v", k2, err)
	}
}
