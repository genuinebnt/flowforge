package identitytest

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
)

// RunStoreContract checks behaviour every identity.Store must share.
// newStore returns an empty store.
func RunStoreContract(t *testing.T, newStore func(t *testing.T) identity.Store) {
	ctx := context.Background()
	now := time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC)
	gen := core.IDGen{Clock: fixed{now}, Rng: core.NewSplitMix64(42)}

	seed := func(t *testing.T, st identity.Store) (identity.Tenant, identity.User) {
		t.Helper()
		tn := identity.Tenant{ID: gen.New(), Name: "Acme", Slug: "acme-" + gen.New().String()[:8], Plan: "free", CreatedAt: now}
		u := identity.User{ID: gen.New(), Email: "Dev" + gen.New().String()[:6] + "@example.com", PasswordHash: "h", Status: auth.UserPending, CreatedAt: now, SessionVersion: 1}
		if err := st.CreateTenant(ctx, tn); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
		if err := st.CreateMembership(ctx, identity.Membership{TenantID: tn.ID, UserID: u.ID, Role: auth.RoleOwner, CreatedAt: now}); err != nil {
			t.Fatal(err)
		}
		return tn, u
	}

	t.Run("role_groups", func(t *testing.T) {
		st := newStore(t)
		tn, u := seed(t, st)
		other := identity.Tenant{ID: gen.New(), Name: "Other", Slug: "other-groups", Plan: "free", CreatedAt: now}
		if err := st.CreateTenant(ctx, other); err != nil {
			t.Fatal(err)
		}
		v := gen.New() // a user id that is no member of tn
		if err := st.AddRoleGroupMember(ctx, tn.ID, "finance_ap", u.ID, now); err != nil {
			t.Fatal(err)
		}
		if err := st.AddRoleGroupMember(ctx, tn.ID, "finance_ap", u.ID, now); err != nil {
			t.Fatalf("re-adding is a no-op: %v", err)
		}
		if err := st.AddRoleGroupMember(ctx, tn.ID, "lead", u.ID, now); err != nil {
			t.Fatal(err)
		}
		// Only members of the tenant can join its groups.
		if err := st.AddRoleGroupMember(ctx, tn.ID, "lead", v, now); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("non-member added: %v", err)
		}
		if gs, err := st.UserRoleGroups(ctx, tn.ID, u.ID); err != nil || len(gs) != 2 || gs[0] != "finance_ap" || gs[1] != "lead" {
			t.Fatalf("user groups %v %v", gs, err)
		}
		if gs, _ := st.UserRoleGroups(ctx, other.ID, u.ID); len(gs) != 0 {
			t.Fatalf("groups leak across tenants: %v", gs)
		}
		if l, err := st.ListRoleGroups(ctx, tn.ID); err != nil || len(l) != 2 || l[0].Name != "finance_ap" || len(l[0].Members) != 1 || l[0].Members[0] != u.ID {
			t.Fatalf("list %+v %v", l, err)
		}
		if ok, err := st.RemoveRoleGroupMember(ctx, tn.ID, "lead", u.ID); !ok || err != nil {
			t.Fatalf("remove: %v %v", ok, err)
		}
		if ok, _ := st.RemoveRoleGroupMember(ctx, tn.ID, "lead", u.ID); ok {
			t.Fatal("removed twice")
		}
		if l, _ := st.ListRoleGroups(ctx, tn.ID); len(l) != 1 {
			t.Fatalf("empty group listed: %+v", l)
		}
		if l, _ := st.ListRoleGroups(ctx, other.ID); len(l) != 0 {
			t.Fatalf("other tenant sees groups: %+v", l)
		}
	})

	t.Run("users", func(t *testing.T) {
		st := newStore(t)
		tn, u := seed(t, st)
		got, err := st.GetUserByEmail(ctx, u.Email)
		if err != nil || got.ID != u.ID {
			t.Fatalf("by email (case-insensitive): %v %v", got, err)
		}
		if _, err := st.GetUserByEmail(ctx, "nobody@example.com"); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("missing user: %v", err)
		}
		dup := u
		dup.ID = gen.New()
		if err := st.CreateUser(ctx, dup); !errors.Is(err, identity.ErrConflict) {
			t.Fatalf("duplicate email: %v", err)
		}
		if err := st.ActivateUser(ctx, u.ID, now); err != nil {
			t.Fatal(err)
		}
		v, err := st.BumpSessionVersion(ctx, u.ID)
		if err != nil || v != 2 {
			t.Fatalf("bump: %d %v", v, err)
		}
		got, _ = st.GetUser(ctx, u.ID)
		if got.Status != auth.UserActive || got.EmailVerifiedAt.IsZero() || got.SessionVersion != 2 {
			t.Fatalf("after activate+bump: %+v", got)
		}
		if _, err := st.GetTenant(ctx, tn.ID); err != nil {
			t.Fatal(err)
		}
		ms, err := st.UserMemberships(ctx, u.ID)
		if err != nil || len(ms) != 1 || ms[0].Role != auth.RoleOwner {
			t.Fatalf("memberships: %v %v", ms, err)
		}
		n, err := st.CountOwners(ctx, tn.ID)
		if err != nil || n != 1 {
			t.Fatalf("owners: %d %v", n, err)
		}
		if err := st.SetMemberRole(ctx, tn.ID, u.ID, auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		mem, err := st.ListMembers(ctx, tn.ID, core.Nil, 10)
		if err != nil || len(mem) != 1 || mem[0].Role != auth.RoleAdmin || mem[0].Email == "" {
			t.Fatalf("members: %+v %v", mem, err)
		}
	})

	t.Run("tx_rollback_or_isolated", func(t *testing.T) {
		st := newStore(t)
		_, u := seed(t, st)
		err := st.WithTx(ctx, func(tx identity.Store) error {
			if _, err := tx.GetUser(ctx, u.ID); err != nil {
				return err
			}
			return errors.New("boom")
		})
		if err == nil || err.Error() != "boom" {
			t.Fatalf("tx error not returned: %v", err)
		}
	})

	t.Run("verifications", func(t *testing.T) {
		st := newStore(t)
		_, u := seed(t, st)
		h := auth.HashToken("tok")
		if err := st.InsertVerification(ctx, auth.Verification{TokenHash: h, UserID: u.ID, ExpiresAt: now.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		v, err := st.ConsumeVerification(ctx, h, now)
		if err != nil || v.UserID != u.ID {
			t.Fatalf("consume: %v %v", v, err)
		}
		if _, err := st.ConsumeVerification(ctx, h, now); !errors.Is(err, auth.ErrVerificationInvalid) {
			t.Fatalf("second consume: %v", err)
		}
		h2 := auth.HashToken("tok2")
		_ = st.InsertVerification(ctx, auth.Verification{TokenHash: h2, UserID: u.ID, ExpiresAt: now})
		if _, err := st.ConsumeVerification(ctx, h2, now); !errors.Is(err, auth.ErrVerificationInvalid) {
			t.Fatalf("expired consume: %v", err)
		}
	})

	t.Run("refresh_tokens", func(t *testing.T) {
		st := newStore(t)
		tn, u := seed(t, st)
		fam := gen.New()
		r1 := auth.RefreshToken{ID: gen.New(), FamilyID: fam, UserID: u.ID, TenantID: tn.ID, TokenHash: auth.HashToken("r1"), CreatedAt: now, FamilyStartedAt: now, ExpiresAt: now.Add(time.Hour)}
		if err := st.InsertRefreshToken(ctx, r1); err != nil {
			t.Fatal(err)
		}
		r2 := r1
		r2.ID, r2.ParentID, r2.TokenHash = gen.New(), r1.ID, auth.HashToken("r2")
		if err := st.InsertRefreshToken(ctx, r2); err != nil {
			t.Fatal(err)
		}
		got, err := st.FindRefreshToken(ctx, r1.TokenHash)
		if err != nil || got.ID != r1.ID || got.FamilyID != fam || !got.FamilyStartedAt.Equal(now) {
			t.Fatalf("find: %+v %v", got, err)
		}
		ok, err := st.MarkRefreshUsed(ctx, r1.ID, now)
		if err != nil || !ok {
			t.Fatalf("mark: %v %v", ok, err)
		}
		ok, _ = st.MarkRefreshUsed(ctx, r1.ID, now)
		if ok {
			t.Fatal("second mark succeeded")
		}
		if err := st.RevokeFamily(ctx, fam, now); err != nil {
			t.Fatal(err)
		}
		got, _ = st.FindRefreshToken(ctx, r2.TokenHash)
		if got.RevokedAt.IsZero() || got.ParentID != r1.ID {
			t.Fatalf("revoked family: %+v", got)
		}
		if _, err := st.FindRefreshToken(ctx, auth.HashToken("none")); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("missing: %v", err)
		}
	})

	t.Run("api_keys", func(t *testing.T) {
		st := newStore(t)
		tn, u := seed(t, st)
		k := identity.APIKeyRecord{ID: gen.New(), TenantID: tn.ID, Name: "ci", SecretHash: auth.HashToken("s"), Role: auth.RoleAnalyst, CreatedBy: u.ID, CreatedAt: now}
		k.Prefix = "ffk_" + k.KeyID()
		if err := st.CreateAPIKey(ctx, k); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetAPIKeyByPrefix(ctx, k.Prefix)
		if err != nil || got.ID != k.ID || got.SecretHash != k.SecretHash || got.Role != auth.RoleAnalyst {
			t.Fatalf("by prefix: %+v %v", got, err)
		}
		if _, err := st.GetAPIKey(ctx, gen.New(), k.ID); !errors.Is(err, identity.ErrNotFound) {
			t.Fatalf("other tenant: %v", err)
		}
		ks, err := st.ListAPIKeys(ctx, tn.ID, core.Nil, 10)
		if err != nil || len(ks) != 1 {
			t.Fatalf("list: %v %v", ks, err)
		}
		if ok, _ := st.RevokeAPIKey(ctx, gen.New(), k.ID, now); ok {
			t.Fatal("revoked through another tenant")
		}
		if ok, err := st.RevokeAPIKey(ctx, tn.ID, k.ID, now); !ok || err != nil {
			t.Fatalf("revoke: %v %v", ok, err)
		}
		if ok, _ := st.RevokeAPIKey(ctx, tn.ID, k.ID, now); ok {
			t.Fatal("revoked twice")
		}
	})

	t.Run("invites", func(t *testing.T) {
		st := newStore(t)
		tn, u := seed(t, st)
		inv := identity.Invite{ID: gen.New(), TenantID: tn.ID, Email: "new@example.com", Role: auth.RoleViewer, TokenHash: auth.HashToken("i"), ExpiresAt: now.Add(time.Hour), CreatedBy: u.ID, CreatedAt: now}
		if err := st.CreateInvite(ctx, inv); err != nil {
			t.Fatal(err)
		}
		got, err := st.GetInviteByHash(ctx, inv.TokenHash)
		if err != nil || got.ID != inv.ID || got.Role != auth.RoleViewer {
			t.Fatalf("invite: %+v %v", got, err)
		}
		if ok, _ := st.AcceptInvite(ctx, inv.ID, now); !ok {
			t.Fatal("accept")
		}
		if ok, _ := st.AcceptInvite(ctx, inv.ID, now); ok {
			t.Fatal("accepted twice")
		}
	})

	t.Run("signing_keys_audit_outbox", func(t *testing.T) {
		st := newStore(t)
		pub := ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)
		k := identity.SigningKeyRecord{KID: "k1", Alg: "EdDSA", PublicKey: pub, PrivateKeyRef: "mem://k1", Status: auth.KeyActive, CreatedAt: now}
		if err := st.PutSigningKey(ctx, k); err != nil {
			t.Fatal(err)
		}
		if err := st.SetSigningKeyStatus(ctx, "k1", auth.KeyRetiring, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
		ks, err := st.ListSigningKeys(ctx)
		if err != nil || len(ks) != 1 || ks[0].Status != auth.KeyRetiring || !ks[0].PublicKey.Equal(pub) || ks[0].PrivateKeyRef != "mem://k1" {
			t.Fatalf("keys: %+v %v", ks, err)
		}
		if err := st.AppendAudit(ctx, identity.AuditRecord{At: now, Event: "login_failed", IP: "203.0.113.7", Detail: map[string]string{"a": "b"}}); err != nil {
			t.Fatal(err)
		}
		ev := identity.OutboxEvent{EventID: gen.New(), Topic: identity.Topic, OrderingKey: "usr_x", Payload: []byte(`{"type":"auth.login"}`), CreatedAt: now}
		if err := st.AppendOutbox(ctx, ev); err != nil {
			t.Fatal(err)
		}
		if err := st.AppendOutbox(ctx, ev); !errors.Is(err, identity.ErrConflict) {
			t.Fatalf("duplicate outbox event: %v", err)
		}
	})

	t.Run("idempotency", func(t *testing.T) {
		st := newStore(t)
		tid := gen.New()
		rec := identity.IdemRecord{TenantID: tid, Key: "k", Method: "POST", Path: "/v1/runs", RequestHash: auth.HashToken("body"), CreatedAt: now, ExpiresAt: now.Add(7 * 24 * time.Hour)}
		got, fresh, err := st.IdemBegin(ctx, rec, time.Minute)
		if err != nil || !fresh {
			t.Fatalf("begin: %v %v", fresh, err)
		}
		got, fresh, _ = st.IdemBegin(ctx, rec, time.Minute)
		if fresh || !got.CompletedAt.IsZero() {
			t.Fatalf("in-flight duplicate: fresh=%v %+v", fresh, got)
		}
		if err := st.IdemComplete(ctx, tid, "k", 202, []byte(`{"id":"run_1"}`), "application/json", now); err != nil {
			t.Fatal(err)
		}
		got, fresh, _ = st.IdemBegin(ctx, rec, time.Minute)
		if fresh || got.Status != 202 || string(got.Body) != `{"id":"run_1"}` || got.RequestHash != rec.RequestHash {
			t.Fatalf("replay: fresh=%v %+v", fresh, got)
		}
		other := rec
		other.TenantID = gen.New()
		if _, fresh, _ := st.IdemBegin(ctx, other, time.Minute); !fresh {
			t.Fatal("keys are not per tenant")
		}
		stale := rec
		stale.Key, stale.CreatedAt = "stale", now
		_, _, _ = st.IdemBegin(ctx, stale, time.Minute)
		stale.CreatedAt = now.Add(2 * time.Minute)
		if _, fresh, _ := st.IdemBegin(ctx, stale, time.Minute); !fresh {
			t.Fatal("stale in-flight row not replaced")
		}
		if err := st.IdemAbandon(ctx, tid, "stale"); err != nil {
			t.Fatal(err)
		}
		if _, fresh, _ := st.IdemBegin(ctx, stale, time.Minute); !fresh {
			t.Fatal("abandoned in-flight row not removed")
		}
		// Abandon keeps a completed row (G-SVC-22): "k" was completed above.
		if err := st.IdemAbandon(ctx, tid, "k"); err != nil {
			t.Fatal(err)
		}
		if got, fresh, _ := st.IdemBegin(ctx, rec, time.Minute); fresh || got.Status != 202 {
			t.Fatalf("abandon removed a completed row: fresh=%v %+v", fresh, got)
		}
		n, err := st.IdemPurge(ctx, now.Add(8*24*time.Hour))
		if err != nil || n < 2 {
			t.Fatalf("purge: %d %v", n, err)
		}
	})
}

type fixed struct{ t time.Time }

func (f fixed) Now() time.Time           { return f.t }
func (f fixed) Monotonic() time.Duration { return 0 }
