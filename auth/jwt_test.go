package auth_test

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

func newVerifier(t testing.TB, keys auth.KeyResolver, clk core.Clock, rev auth.RevocationChecker) *auth.Verifier {
	t.Helper()
	if rev == nil {
		rev = auth.NewInMemoryRevocation()
	}
	v, err := auth.NewVerifier(auth.VerifierConfig{Keys: keys, Clock: clk, Issuer: testIssuer, Audience: auth.AudienceAPI, Revocation: rev})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSignerRequiresActiveKey(t *testing.T) {
	for _, st := range []auth.KeyState{auth.KeyRetiring, auth.KeyRetired} {
		if _, err := auth.NewSigner(testKey(t, "k", st)); !errors.Is(err, auth.ErrKeyNotActive) {
			t.Errorf("NewSigner(%s key) error = %v, want ErrKeyNotActive", st, err)
		}
	}
	if _, err := auth.NewSigner(auth.SigningKey{KID: "k", State: auth.KeyActive}); !errors.Is(err, auth.ErrInvalidKey) {
		t.Errorf("NewSigner(empty key) error = %v, want ErrInvalidKey", err)
	}
}

func TestVerifierTimeBoundaries(t *testing.T) {
	k := testKey(t, "2026-11", auth.KeyActive)
	s := mustSigner(t, k)
	clk := testkit.NewFakeClock(specNow)
	v := newVerifier(t, mustKeySet(t, k.Public()), clk, nil)
	now := specNow.Unix()
	tests := []struct {
		name   string
		mutate func(*auth.Claims)
		want   error
	}{
		{"exp 29s ago", func(c *auth.Claims) { c.ExpiresAt, c.IssuedAt = now-29, now-600 }, nil},
		{"exp 30s ago", func(c *auth.Claims) { c.ExpiresAt, c.IssuedAt = now-30, now-600 }, auth.ErrExpired},
		{"iat 30s ahead", func(c *auth.Claims) { c.IssuedAt, c.ExpiresAt = now+30, now+600 }, nil},
		{"iat 31s ahead", func(c *auth.Claims) { c.IssuedAt, c.ExpiresAt = now+31, now+600 }, auth.ErrIssuedInFuture},
		{"nbf 30s ahead", func(c *auth.Claims) { c.NotBefore = now + 30 }, nil},
		{"nbf 31s ahead", func(c *auth.Claims) { c.NotBefore = now + 31 }, auth.ErrNotYetValid},
		{"no exp", func(c *auth.Claims) { c.ExpiresAt = 0 }, auth.ErrClaimMissing},
		{"no iat", func(c *auth.Claims) { c.IssuedAt = 0 }, auth.ErrClaimMissing},
		{"one-minute lifetime", func(c *auth.Claims) { c.IssuedAt, c.ExpiresAt = now, now+60 }, nil},
		{"exp equals iat", func(c *auth.Claims) { c.IssuedAt, c.ExpiresAt = now, now }, auth.ErrClaimInvalid},
		{"no sub", func(c *auth.Claims) { c.Subject = "" }, auth.ErrClaimMissing},
		{"no tid", func(c *auth.Claims) { c.TenantID = "" }, auth.ErrClaimMissing},
		{"no role", func(c *auth.Claims) { c.Role = "" }, auth.ErrClaimMissing},
		{"negative sv", func(c *auth.Claims) { c.SessionVersion = -1 }, auth.ErrClaimInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := baseClaims()
			tt.mutate(&c)
			tok, err := s.Sign(c)
			if err != nil {
				t.Fatal(err)
			}
			_, err = v.Verify(context.Background(), tok)
			if tt.want == nil && err != nil || tt.want != nil && !errors.Is(err, tt.want) {
				t.Fatalf("Verify = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestVerifierRejectsMalformed(t *testing.T) {
	k := testKey(t, "2026-11", auth.KeyActive)
	v := newVerifier(t, mustKeySet(t, k.Public()), testkit.NewFakeClock(specNow), nil)
	good, err := mustSigner(t, k).Sign(baseClaims())
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	signed := func(header, payload string) string {
		in := enc([]byte(header)) + "." + enc([]byte(payload))
		return in + "." + enc(ed25519.Sign(k.Private, []byte(in)))
	}
	hdr := `{"alg":"EdDSA","typ":"JWT","kid":"2026-11"}`
	payload, _ := json.Marshal(baseClaims())
	tests := map[string]string{
		"empty":                   "",
		"one segment":             "abc",
		"four segments":           good + ".x",
		"space":                   " " + good,
		"standard base64":         strings.ReplaceAll(strings.ReplaceAll(good, "-", "+"), "_", "/") + "+",
		"header not json":         enc([]byte("nope")) + "." + enc(payload) + ".AA",
		"header is array":         enc([]byte(`["EdDSA"]`)) + "." + enc(payload) + ".AA",
		"header trailing data":    signed(hdr+`{}`, string(payload)),
		"payload not object":      signed(hdr, `[1,2]`),
		"payload duplicate claim": signed(hdr, strings.Replace(string(payload), `"role":"owner"`, `"role":"viewer","role":"owner"`, 1)),
		"payload case variant":    signed(hdr, strings.Replace(string(payload), `"role":"owner"`, `"ROLE":"viewer","role":"owner"`, 1)),
		"payload float iat":       signed(hdr, strings.Replace(string(payload), `"iat":1794489600`, `"iat":1794489600.5`, 1)),
		"too long":                good + strings.Repeat("A", auth.MaxTokenLength),
	}
	for name, tok := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), tok)
			if !errors.Is(err, auth.ErrMalformed) {
				t.Fatalf("Verify = %v, want ErrMalformed", err)
			}
		})
	}
}

type brokenRevocation struct{}

func (brokenRevocation) IsRevoked(context.Context, string) (bool, error) {
	return false, errors.New("redis down")
}
func (brokenRevocation) SessionVersion(context.Context, string) (int64, bool, error) {
	return 0, false, nil
}

func TestVerifierRevocationFailsClosed(t *testing.T) {
	k := testKey(t, "2026-11", auth.KeyActive)
	tok, _ := mustSigner(t, k).Sign(baseClaims())
	v := newVerifier(t, mustKeySet(t, k.Public()), testkit.NewFakeClock(specNow), brokenRevocation{})
	_, err := v.Verify(context.Background(), tok)
	if !errors.Is(err, auth.ErrRevocationUnavailable) || errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("Verify = %v, want ErrRevocationUnavailable (not ErrInvalidToken)", err)
	}
}

func TestNewVerifierConfig(t *testing.T) {
	ks := mustKeySet(t)
	clk := testkit.NewFakeClock(specNow)
	bad := []auth.VerifierConfig{
		{Clock: clk, Issuer: "i", Audience: "a", Revocation: auth.NoRevocation{}},
		{Keys: ks, Clock: clk, Issuer: "i", Audience: "a"},
		{Keys: ks, Clock: clk, Audience: "a", Revocation: auth.NoRevocation{}},
		{Keys: ks, Clock: clk, Issuer: "i", Audience: "a", Revocation: auth.NoRevocation{}, Skew: -time.Second},
	}
	for i, c := range bad {
		if _, err := auth.NewVerifier(c); !errors.Is(err, auth.ErrConfig) {
			t.Errorf("config %d: error = %v, want ErrConfig", i, err)
		}
	}
}

func TestKeyCacheRefreshOnUnknownKid(t *testing.T) {
	a := testKey(t, "a", auth.KeyActive)
	b := testKey(t, "b", auth.KeyActive)
	src := auth.NewMemoryKeySource(mustKeySet(t, a.Public()))
	clk := testkit.NewFakeClock(specNow)
	cache, err := auth.NewKeyCache(auth.KeyCacheConfig{Source: src, Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	resolve := func(kid string, wantErr error, wantFetches int) {
		t.Helper()
		_, err := cache.ResolveKey(ctx, kid)
		if wantErr == nil && err != nil || wantErr != nil && !errors.Is(err, wantErr) {
			t.Fatalf("ResolveKey(%q) = %v, want %v", kid, err, wantErr)
		}
		if got := src.Fetches(); got != wantFetches {
			t.Fatalf("after ResolveKey(%q): %d fetches, want %d", kid, got, wantFetches)
		}
	}
	resolve("a", nil, 1)                // first use loads the set
	resolve("b", auth.ErrUnknownKid, 1) // the load counts as this minute's refresh
	clk.Advance(time.Minute)
	resolve("b", auth.ErrUnknownKid, 2) // unknown kid triggers one refresh
	resolve("b", auth.ErrUnknownKid, 2) // ...but not another within a minute
	src.Put(mustKeySet(t, a.Public(), b.Public()))
	clk.Advance(59 * time.Second)
	resolve("b", auth.ErrUnknownKid, 2)
	clk.Advance(time.Second)
	resolve("b", nil, 3) // a minute later the new key is picked up
	resolve("a", nil, 3)

	// MaxAge: the set is refetched after five minutes, so retirement
	// propagates even without unknown kids.
	src.Put(mustKeySet(t, auth.PublicKey{KID: "a", Key: a.Public().Key, State: auth.KeyRetired}, b.Public()))
	clk.Advance(auth.KeySetMaxAge)
	resolve("a", auth.ErrKeyRetired, 4)

	// A failing source keeps the cached set and reports the cause for an
	// unknown kid.
	src.Fail(errors.New("jwks endpoint down"))
	clk.Advance(time.Minute)
	resolve("b", nil, 4)
	_, err = cache.ResolveKey(ctx, "c")
	if !errors.Is(err, auth.ErrUnknownKid) || !strings.Contains(err.Error(), "jwks endpoint down") {
		t.Fatalf("ResolveKey with failing source = %v", err)
	}
}

func TestJWKS(t *testing.T) {
	active := testKey(t, "2026-11", auth.KeyActive).Public()
	retiring := testKey(t, "2026-08", auth.KeyRetiring).Public()
	retired := testKey(t, "2026-05", auth.KeyRetired).Public()
	set := mustKeySet(t, active, retiring, retired)
	b, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Keys []map[string]string `json:"keys"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Keys) != 2 || doc.Keys[0]["kid"] != "2026-08" || doc.Keys[1]["kid"] != "2026-11" {
		t.Fatalf("JWKS = %s; want the retiring and active keys sorted by kid", b)
	}
	k := doc.Keys[1]
	if k["kty"] != "OKP" || k["crv"] != "Ed25519" || k["alg"] != "EdDSA" || k["use"] != "sig" ||
		k["x"] != base64.RawURLEncoding.EncodeToString(active.Key) || k["d"] != "" {
		t.Fatalf("JWK = %v", k)
	}
	parsed, err := auth.ParseJWKS(b)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := parsed.Lookup("2026-11"); !ok || !got.Key.Equal(active.Key) {
		t.Fatal("round trip lost the active key")
	}
	if _, ok := parsed.Lookup("2026-05"); ok {
		t.Fatal("retired key was published")
	}

	x := base64.RawURLEncoding.EncodeToString(active.Key)
	bad := []string{
		`not json`,
		`{"keys":[{"kty":"OKP","crv":"X25519","kid":"k","x":"` + x + `"}]}`,
		`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"k","x":"` + x + `","d":"secret"}]}`,
		`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"k","x":"AAAA"}]}`,
		`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"k","x":"` + x + `","use":"enc"}]}`,
		`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"k","x":"` + x + `","alg":"ES256"}]}`,
		`{"keys":[{"kty":"OKP","crv":"Ed25519","x":"` + x + `"}]}`,
		`{"keys":[{"kty":"OKP","crv":"Ed25519","kid":"k","x":"` + x + `"},{"kty":"OKP","crv":"Ed25519","kid":"k","x":"` + x + `"}]}`,
	}
	for _, s := range bad {
		if _, err := auth.ParseJWKS([]byte(s)); !errors.Is(err, auth.ErrInvalidJWKS) {
			t.Errorf("ParseJWKS(%s) = %v, want ErrInvalidJWKS", s, err)
		}
	}
	other, err := auth.ParseJWKS([]byte(`{"keys":[{"kty":"RSA","kid":"r","n":"AQAB","e":"AQAB"}]}`))
	if err != nil || len(other.Keys()) != 0 {
		t.Fatalf("ParseJWKS skipping RSA = %v, %v", other, err)
	}
}

func TestMinterAccessAndInternal(t *testing.T) {
	k := testKey(t, "2026-11", auth.KeyActive)
	clk := testkit.NewFakeClock(specNow)
	m, err := auth.NewMinter(auth.MinterConfig{Signer: mustSigner(t, k), Clock: clk, IDs: core.NewSplitMix64(1), Issuer: testIssuer})
	if err != nil {
		t.Fatal(err)
	}
	ids := core.IDGen{Clock: clk, Rng: core.NewSplitMix64(2)}
	user, tenant, session := ids.New(), ids.New(), ids.New()
	keys := mustKeySet(t, k.Public())
	api := newVerifier(t, keys, clk, nil)
	internal, err := auth.NewVerifier(auth.InternalVerifierConfig(keys, clk, testIssuer))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	tok, claims, err := m.Access(auth.AccessGrant{UserID: user, TenantID: tenant, Role: auth.RoleAnalyst, SessionID: session, SessionVersion: 3})
	if err != nil {
		t.Fatal(err)
	}
	got, err := api.Verify(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if *got != claims || got.Subject != core.PublicID(core.PrefixUser, user) || got.TenantID != core.PublicID(core.PrefixTenant, tenant) ||
		got.SessionID != core.PublicID(core.PrefixSession, session) || got.SessionVersion != 3 || got.Role != auth.RoleAnalyst ||
		got.Expiry().Sub(got.Issued()) != auth.AccessTokenTTL || got.ID == "" {
		t.Fatalf("access claims = %+v", got)
	}
	if _, err := internal.Verify(ctx, tok); !errors.Is(err, auth.ErrAudience) {
		t.Fatalf("access token on the internal verifier: %v, want ErrAudience", err)
	}
	_, second, _ := m.Access(auth.AccessGrant{UserID: user, TenantID: tenant, Role: auth.RoleAnalyst, SessionID: session})
	if second.ID == claims.ID {
		t.Fatal("two access tokens share a jti")
	}

	itok, ic, err := m.Internal(auth.InternalGrant{TenantID: tenant, Actor: "svc_scheduler", Role: auth.RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := internal.Verify(ctx, itok); err != nil || got.Subject != "svc_scheduler" || got.Audience != auth.AudienceInternal ||
		got.Expiry().Sub(got.Issued()) != auth.InternalTokenTTL {
		t.Fatalf("internal Verify = %+v, %v (minted %+v)", got, err, ic)
	}
	if _, err := api.Verify(ctx, itok); !errors.Is(err, auth.ErrAudience) {
		t.Fatalf("internal token on the API verifier: %v, want ErrAudience", err)
	}
	long := ic
	long.ExpiresAt = long.IssuedAt + 600
	ltok, _ := mustSigner(t, k).Sign(long)
	if _, err := internal.Verify(ctx, ltok); !errors.Is(err, auth.ErrLifetimeTooLong) {
		t.Fatalf("10-minute internal token: %v, want ErrLifetimeTooLong", err)
	}
	clk.Advance(auth.InternalTokenTTL + auth.DefaultSkew)
	if _, err := internal.Verify(ctx, itok); !errors.Is(err, auth.ErrExpired) {
		t.Fatalf("internal token after 90 s: %v, want ErrExpired", err)
	}
	if _, _, err := m.Access(auth.AccessGrant{Role: "root"}); err == nil {
		t.Fatal("minted a token for an unknown role")
	}
}

// FuzzJWT is fuzz_jwt: arbitrary token strings are never accepted without a
// valid signature by a non-retired key over exactly the first two
// segments, with the header alg EdDSA.
func FuzzJWT(f *testing.F) {
	active := testKey(f, "2026-11", auth.KeyActive)
	retired := testKey(f, "2026-05", auth.KeyRetired)
	keys := mustKeySet(f, active.Public(), retired.Public())
	good, err := mustSigner(f, active).Sign(baseClaims())
	if err != nil {
		f.Fatal(err)
	}
	byRetired, _ := mustSigner(f, retired).Sign(baseClaims())
	f.Add(good)
	f.Add(byRetired)
	f.Add("")
	f.Add("..")
	f.Add("eyJhbGciOiJub25lIn0.e30.")
	f.Add(good[:len(good)-2])
	f.Add(strings.Replace(good, ".", "..", 1))
	for _, name := range []string{"alg_none.json", "tampered_payload.json", "alg_hs256_key_confusion.json"} {
		var c jwtCase
		if err := json.Unmarshal(testkit.ReadFile(f, "auth/jwt", name), &c); err != nil {
			f.Fatal(err)
		}
		f.Add(c.Token)
	}
	v := newVerifier(f, keys, testkit.NewFakeClock(specNow), auth.NoRevocation{})
	f.Fuzz(func(t *testing.T, tok string) {
		claims, err := v.Verify(context.Background(), tok)
		if err != nil {
			if claims != nil {
				t.Fatal("rejection returned claims")
			}
			return
		}
		parts := strings.Split(tok, ".")
		if len(parts) != 3 {
			t.Fatalf("accepted a token with %d segments", len(parts))
		}
		hb, err1 := base64.RawURLEncoding.DecodeString(parts[0])
		sig, err2 := base64.RawURLEncoding.DecodeString(parts[2])
		var h struct {
			Alg string `json:"alg"`
		}
		if err1 != nil || err2 != nil || json.Unmarshal(hb, &h) != nil || h.Alg != "EdDSA" {
			t.Fatalf("accepted a token whose header or signature does not decode: %q", tok)
		}
		if !ed25519.Verify(active.Public().Key, []byte(parts[0]+"."+parts[1]), sig) {
			t.Fatalf("accepted a token without a valid signature by the active key: %q", tok)
		}
	})
}

// BenchmarkAuth01_Verify is b_auth_01 verification: one full Verify (EdDSA
// signature plus claim and revocation checks) per op.
func BenchmarkAuth01_Verify(b *testing.B) {
	k := testKey(b, "2026-11", auth.KeyActive)
	tok, err := mustSigner(b, k).Sign(baseClaims())
	if err != nil {
		b.Fatal(err)
	}
	v := newVerifier(b, mustKeySet(b, k.Public()), testkit.NewFakeClock(specNow), nil)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := v.Verify(ctx, tok); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAuth01_Sign is b_auth_01 signing: one EdDSA access token per op.
func BenchmarkAuth01_Sign(b *testing.B) {
	s := mustSigner(b, testKey(b, "2026-11", auth.KeyActive))
	c := baseClaims()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := s.Sign(c); err != nil {
			b.Fatal(err)
		}
	}
}
