package identity_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
	"github.com/genuinebnt/flowforge/services/gateway/identity/identitytest"
	"github.com/genuinebnt/flowforge/testkit"
)

var t0 = time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC)

func reason(err error) string {
	var ce *core.Error
	if errors.As(err, &ce) {
		return ce.Reason
	}
	return ""
}

func TestIdentityMemStoreContract(t *testing.T) {
	identitytest.RunStoreContract(t, func(*testing.T) identity.Store { return identity.NewMemStore() })
}

func newFixture(t *testing.T) (*identitytest.Fixture, *testkit.FakeClock) {
	clock := testkit.NewFakeClock(t0)
	return identitytest.New(t, clock, identitytest.Options{}), clock
}

func TestIdentitySignupVerifyLogin(t *testing.T) {
	ctx := context.Background()
	f, _ := newFixture(t)
	ci := identity.ClientInfo{IP: "203.0.113.1"}
	pw := "correct horse battery staple"
	res, err := f.Service.Signup(ctx, identity.SignupInput{Email: "Dev@Example.com", Password: pw, TenantName: "Example Security"}, ci)
	if err != nil {
		t.Fatal(err)
	}
	// Pending users cannot log in, even with the right password.
	if _, err := f.Service.Login(ctx, identity.LoginInput{Email: "dev@example.com", Password: pw}, ci); reason(err) != core.ReasonTokenInvalid {
		t.Fatalf("pending login: %v", err)
	}
	m, ok := f.Email.Last("dev@example.com", identity.EmailVerify)
	if !ok || m.Token == "" {
		t.Fatal("no verification e-mail")
	}
	u, err := f.Service.VerifyEmail(ctx, m.Token, ci)
	if err != nil || u.ID != res.UserID || u.Status != auth.UserActive {
		t.Fatalf("verify: %+v %v", u, err)
	}
	if _, err := f.Service.VerifyEmail(ctx, m.Token, ci); reason(err) != core.ReasonValidationFailed {
		t.Fatalf("token reuse: %v", err)
	}
	s, err := f.Service.Login(ctx, identity.LoginInput{Email: "dev@example.com", Password: pw}, ci)
	if err != nil {
		t.Fatal(err)
	}
	if s.Role != auth.RoleOwner || s.Tenant.ID != res.TenantID || !strings.HasPrefix(s.RefreshToken, "ffr_") {
		t.Fatalf("session: %+v", s)
	}
	if s.Claims.TenantID != core.PublicID(core.PrefixTenant, res.TenantID) || s.Claims.Audience != auth.AudienceAPI {
		t.Fatalf("claims: %+v", s.Claims)
	}
	var events []string
	for _, a := range f.Mem.AuditLog() {
		events = append(events, a.Event)
	}
	if got := strings.Join(events, ","); !strings.Contains(got, "signup") || !strings.Contains(got, "email_verified") || !strings.Contains(got, "login") {
		t.Fatalf("audit: %s", got)
	}
	if len(f.Mem.Outbox()) < 3 {
		t.Fatalf("outbox: %d rows", len(f.Mem.Outbox()))
	}
}

func TestIdentitySignupExistingEmailIsUniform(t *testing.T) {
	ctx := context.Background()
	f, _ := newFixture(t)
	_, accts := f.SeedTenant(t, "acme")
	in := identity.SignupInput{Email: accts[auth.RoleOwner].Email, Password: "another long password", TenantName: "Again"}
	res, err := f.Service.Signup(ctx, in, identity.ClientInfo{IP: "203.0.113.2"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Store.GetUser(ctx, res.UserID); !errors.Is(err, identity.ErrNotFound) {
		t.Fatal("existing-email signup returned a real user id")
	}
	if _, ok := f.Email.Last(in.Email, identity.EmailSignupExists); !ok {
		t.Fatal("no 'someone tried to sign up' notice")
	}
}

func TestIdentitySignupThrottlePerIP(t *testing.T) {
	ctx := context.Background()
	f, _ := newFixture(t)
	ci := identity.ClientInfo{IP: "198.51.100.9"}
	for i := 0; i < 5; i++ {
		_, err := f.Service.Signup(ctx, identity.SignupInput{Email: "u" + string(rune('a'+i)) + "@example.com", Password: "long enough password", TenantName: "T"}, ci)
		if err != nil {
			t.Fatalf("signup %d: %v", i, err)
		}
	}
	_, err := f.Service.Signup(ctx, identity.SignupInput{Email: "z@example.com", Password: "long enough password", TenantName: "T"}, ci)
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Reason != core.ReasonRateLimited || ce.RetryAfter <= 0 {
		t.Fatalf("sixth signup: %v", err)
	}
}

func TestIdentityRefreshRotationAndReuse(t *testing.T) {
	ctx := context.Background()
	f, _ := newFixture(t)
	_, accts := f.SeedTenant(t, "acme")
	a := accts[auth.RoleAnalyst]
	s1, err := f.Service.Login(ctx, identity.LoginInput{Email: a.Email, Password: a.Password}, identity.ClientInfo{IP: "x"})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := f.Service.Refresh(ctx, s1.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if s2.SessionID != s1.SessionID || s2.RefreshToken == s1.RefreshToken {
		t.Fatal("rotation did not keep the family / issue a new token")
	}
	// Reuse of the rotated token: whole family revoked, sv bumped.
	if _, err := f.Service.Refresh(ctx, s1.RefreshToken); reason(err) != core.ReasonTokenInvalid {
		t.Fatalf("reuse: %v", err)
	}
	if _, err := f.Service.Refresh(ctx, s2.RefreshToken); reason(err) != core.ReasonTokenInvalid {
		t.Fatalf("family member after reuse: %v", err)
	}
	v, ok, _ := f.Revocations.SessionVersion(ctx, core.PublicID(core.PrefixUser, a.UserID))
	if !ok || v != 2 {
		t.Fatalf("session version cached: %d %v", v, ok)
	}
	found := false
	for _, e := range f.Mem.Outbox() {
		found = found || strings.Contains(string(e.Payload), `"auth.refresh_reuse"`)
	}
	if !found {
		t.Fatal("no auth.refresh_reuse outbox event")
	}
}

func TestIdentityLoginUniformAndLockout(t *testing.T) {
	ctx := context.Background()
	f, clock := newFixture(t)
	_, accts := f.SeedTenant(t, "acme")
	known := accts[auth.RoleViewer].Email
	for _, email := range []string{known, "ghost@acme.test"} {
		f.Hasher.Verifies()
		_, err := f.Service.Login(ctx, identity.LoginInput{Email: email, Password: "wrong password!!"}, identity.ClientInfo{IP: email})
		var ce *core.Error
		if !errors.As(err, &ce) || ce.Reason != core.ReasonTokenInvalid || ce.Message != "invalid email or password" {
			t.Fatalf("%s: %v", email, err)
		}
		if n := f.Hasher.Verifies(); n != 1 {
			t.Fatalf("%s: %d password verifications, want 1", email, n)
		}
	}
	// 10 failures (spread over IPs to stay under the IP limit) lock the
	// account for 15 minutes, identically for unknown e-mails.
	for _, email := range []string{known, "ghost@acme.test"} {
		var last error
		for i := 0; i < 11; i++ {
			_, last = f.Service.Login(ctx, identity.LoginInput{Email: email, Password: "wrong password!!"}, identity.ClientInfo{IP: "ip" + string(rune('a'+i))})
			clock.Advance(time.Minute)
		}
		if reason(last) != core.ReasonRateLimited {
			t.Fatalf("%s: after 11 failures: %v", email, last)
		}
	}
}

func TestIdentityPasswordReset(t *testing.T) {
	ctx := context.Background()
	f, _ := newFixture(t)
	_, accts := f.SeedTenant(t, "acme")
	a := accts[auth.RoleAdmin]
	s, _ := f.Service.Login(ctx, identity.LoginInput{Email: a.Email, Password: a.Password}, identity.ClientInfo{IP: "x"})
	if err := f.Service.ForgotPassword(ctx, "nobody@acme.test", identity.ClientInfo{IP: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := f.Service.ForgotPassword(ctx, a.Email, identity.ClientInfo{IP: "x"}); err != nil {
		t.Fatal(err)
	}
	m, ok := f.Email.Last(a.Email, identity.EmailPasswordReset)
	if !ok {
		t.Fatal("no reset mail")
	}
	// A reset token cannot verify e-mail (domain separation).
	if _, err := f.Service.VerifyEmail(ctx, m.Token, identity.ClientInfo{}); reason(err) != core.ReasonValidationFailed {
		t.Fatalf("reset token as verification: %v", err)
	}
	if err := f.Service.ResetPassword(ctx, m.Token, "brand new password 2", identity.ClientInfo{}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Service.Refresh(ctx, s.RefreshToken); reason(err) != core.ReasonTokenInvalid {
		t.Fatalf("old session survived reset: %v", err)
	}
	if _, err := f.Service.Login(ctx, identity.LoginInput{Email: a.Email, Password: "brand new password 2"}, identity.ClientInfo{IP: "y"}); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityMembersInvitesAPIKeys(t *testing.T) {
	ctx := context.Background()
	f, _ := newFixture(t)
	tid, accts := f.SeedTenant(t, "acme")
	admin := identity.Actor{TenantID: tid, UserID: accts[auth.RoleAdmin].UserID, Role: auth.RoleAdmin}
	owner := identity.Actor{TenantID: tid, UserID: accts[auth.RoleOwner].UserID, Role: auth.RoleOwner}

	if _, err := f.Service.SetMemberRole(ctx, admin, accts[auth.RoleViewer].UserID, auth.RoleOwner); reason(err) != core.ReasonForbiddenRole {
		t.Fatalf("admin grants owner: %v", err)
	}
	if _, err := f.Service.SetMemberRole(ctx, owner, accts[auth.RoleOwner].UserID, auth.RoleAdmin); reason(err) != core.ReasonValidationFailed {
		t.Fatalf("demote last owner: %v", err)
	}
	m, err := f.Service.SetMemberRole(ctx, admin, accts[auth.RoleViewer].UserID, auth.RoleAnalyst)
	if err != nil || m.Role != auth.RoleAnalyst {
		t.Fatalf("set role: %+v %v", m, err)
	}
	if v, ok, _ := f.Revocations.SessionVersion(ctx, core.PublicID(core.PrefixUser, accts[auth.RoleViewer].UserID)); !ok || v != 2 {
		t.Fatal("role change did not bump session version")
	}
	ms, next, err := f.Service.ListMembers(ctx, tid, "", 2)
	if err != nil || len(ms) != 2 || next == "" {
		t.Fatalf("page 1: %d %q %v", len(ms), next, err)
	}
	rest, next2, _ := f.Service.ListMembers(ctx, tid, next, 10)
	if len(rest) != 3 || next2 != "" {
		t.Fatalf("page 2: %d %q", len(rest), next2)
	}

	inv, err := f.Service.CreateInvite(ctx, admin, "new@acme.test", auth.RoleViewer)
	if err != nil || inv.Role != auth.RoleViewer {
		t.Fatal(err)
	}
	mail, _ := f.Email.Last("new@acme.test", identity.EmailInvite)
	acc, err := f.Service.AcceptInvite(ctx, mail.Token, "invited user password", identity.ClientInfo{})
	if err != nil || !acc.Created || acc.TenantID != tid {
		t.Fatalf("accept: %+v %v", acc, err)
	}
	if _, err := f.Service.AcceptInvite(ctx, mail.Token, "invited user password", identity.ClientInfo{}); reason(err) != core.ReasonValidationFailed {
		t.Fatalf("accept twice: %v", err)
	}

	k, err := f.Service.CreateAPIKey(ctx, admin, "ci", auth.RoleAnalyst, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.Service.AuthenticateAPIKey(ctx, k.Token)
	if err != nil || got.ID != k.Record.ID || got.Role != auth.RoleAnalyst {
		t.Fatalf("authenticate: %v", err)
	}
	// Flip a character inside the secret (not the last one: its low bits
	// are base64 padding and may decode to the same bytes).
	tampered := []byte(k.Token)
	i := len(tampered) - 5
	if tampered[i] == 'A' {
		tampered[i] = 'B'
	} else {
		tampered[i] = 'A'
	}
	if _, err := f.Service.AuthenticateAPIKey(ctx, string(tampered)); reason(err) != core.ReasonTokenInvalid {
		t.Fatalf("tampered key: %v", err)
	}
	if err := f.Service.RevokeAPIKey(ctx, identity.Actor{TenantID: core.NewUUIDv7(f.Clock, core.NewSplitMix64(1))}, k.Record.ID); reason(err) != core.ReasonNotFound {
		t.Fatalf("revoke via other tenant: %v", err)
	}
	if err := f.Service.RevokeAPIKey(ctx, admin, k.Record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Service.AuthenticateAPIKey(ctx, k.Token); reason(err) != core.ReasonTokenInvalid {
		t.Fatalf("revoked key: %v", err)
	}
	if _, err := f.Service.CreateAPIKey(ctx, owner, "own", auth.RoleOwner, time.Time{}); reason(err) != core.ReasonValidationFailed {
		t.Fatalf("owner key: %v", err)
	}
}

func TestIdentityKeyRotation(t *testing.T) {
	ctx := context.Background()
	f, clock := newFixture(t)
	first := f.Keys.ActiveKID()
	_, accts := f.SeedTenant(t, "acme")
	tok := f.AccessToken(t, accts[auth.RoleViewer])
	clock.Advance(identity.RotationPeriod)
	if err := f.Keys.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if f.Keys.ActiveKID() == first {
		t.Fatal("not rotated after 90 days")
	}
	if k, _ := f.Keys.KeySet().Lookup(first); k.State != auth.KeyRetiring {
		t.Fatalf("old key state %s", k.State)
	}
	clock.Advance(auth.AccessTokenTTL + time.Minute)
	if err := f.Keys.Maintain(ctx); err != nil {
		t.Fatal(err)
	}
	if k, _ := f.Keys.KeySet().Lookup(first); k.State != auth.KeyRetired {
		t.Fatalf("old key state %s after retire_at", k.State)
	}
	if b, _ := f.Keys.JWKS(); strings.Contains(string(b), first) {
		t.Fatal("retired key published")
	}
	_ = tok
	// Private keys are never in the store, only references.
	recs, _ := f.Store.ListSigningKeys(ctx)
	for _, r := range recs {
		if !strings.HasPrefix(r.PrivateKeyRef, "mem://") {
			t.Fatalf("private key ref %q", r.PrivateKeyRef)
		}
	}
	// A restart with a key store that lost the private key rotates.
	km, err := identity.NewKeyManager(ctx, identity.KeyManagerConfig{
		Store: f.Store, Secrets: identity.NewMemKeyStore(), Clock: clock, IDs: core.NewSplitMix64(9),
		Random: strings.NewReader(strings.Repeat("k", 64)), Issuer: identitytest.Issuer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if km.ActiveKID() == f.Keys.ActiveKID() {
		t.Fatal("restart without private key reused the unreadable key")
	}
}
