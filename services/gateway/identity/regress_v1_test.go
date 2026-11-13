package identity_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
	"github.com/genuinebnt/flowforge/services/gateway/identity/identitytest"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/notifier"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestRegress_G_LIM_07_SignupThrottledEvent: a throttled signup writes
// auth.signup_throttled (and a throttled login auth.login_throttled) to
// identity.outbox, and the default signup-abuse rule matches both events
// once the notifier has turned them into rule input.
func TestRegress_G_LIM_07_SignupThrottledEvent(t *testing.T) {
	ctx := context.Background()
	f, _ := newFixture(t)
	ci := identity.ClientInfo{IP: "198.51.100.7"}
	lim := identity.DefaultLimits().Signup.Count
	for i := 0; i <= lim; i++ {
		_, err := f.Service.Signup(ctx, identity.SignupInput{Email: "u" + string(rune('a'+i)) + "@example.com",
			Password: "correct horse battery staple!", TenantName: "x"}, ci)
		if i < lim && err != nil {
			t.Fatalf("signup %d: %v", i, err)
		}
		if i == lim && reason(err) != "RATE_LIMITED" {
			t.Fatalf("signup %d not throttled: %v", i, err)
		}
	}
	for range auth.DefaultLoginIPLimit().Count + 1 {
		_, _ = f.Service.Login(ctx, identity.LoginInput{Email: "nobody@example.com", Password: "wrong password!!"}, ci)
	}

	rule, ds := alerts.Compile(alerts.DefaultRuleSignupAbuse)
	if rule == nil {
		t.Fatalf("default rule: %v", ds)
	}
	rs := alerts.NewRuleSet(alerts.RenderOptions{})
	if err := rs.Add("ops_signup_abuse", rule); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, o := range f.Mem.Outbox() {
		if o.Topic != identity.Topic || identity.Topic != convert.TopicAuthEvents {
			t.Fatalf("topic %q", o.Topic)
		}
		var p struct {
			EventID string `json:"event_id"`
			Type    string `json:"type"`
		}
		if err := json.Unmarshal(o.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Type != "auth."+identity.EventSignupThrottled && p.Type != "auth."+identity.EventLoginThrottled {
			continue
		}
		seen[p.Type] = true
		env := &pb.EventEnvelope{EventId: p.EventID, Type: p.Type, SchemaVersion: 1}
		if ms := rs.Evaluate(notifier.EventFromEnvelope(env)).Matches; len(ms) != 1 {
			t.Fatalf("%s: default rule matches %+v", p.Type, ms)
		}
	}
	if !seen["auth.signup_throttled"] || !seen["auth.login_throttled"] {
		t.Fatalf("throttle events in outbox: %v", seen)
	}
}

// disablingStore reports the tenant disabled once disabled is set, as an
// operator's UPDATE of identity.tenants.disabled_at would.
type disablingStore struct {
	identity.Store
	disabled bool
}

func (d *disablingStore) GetTenant(ctx context.Context, id core.UUID) (identity.Tenant, error) {
	t, err := d.Store.GetTenant(ctx, id)
	if err == nil && d.disabled {
		t.DisabledAt = t.CreatedAt.Add(time.Hour)
	}
	return t, err
}

// TestRegress_G_SVC_21_RefreshDisabledTenant: Login and API keys refused a
// disabled tenant, but Refresh kept minting access tokens for its
// sessions for up to the 90-day family lifetime. Refresh now revokes the
// session and fails like an invalid token.
func TestRegress_G_SVC_21_RefreshDisabledTenant(t *testing.T) {
	ctx := context.Background()
	st := &disablingStore{Store: identity.NewMemStore()}
	f := identitytest.New(t, testkit.NewFakeClock(t0), identitytest.Options{Store: st})
	_, accts := f.SeedTenant(t, "acme")
	a := accts[auth.RoleAdmin]
	s, err := f.Service.Login(ctx, identity.LoginInput{Email: a.Email, Password: a.Password}, identity.ClientInfo{IP: "192.0.2.1"})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := f.Service.Refresh(ctx, s.RefreshToken)
	if err != nil {
		t.Fatalf("refresh of an enabled tenant: %v", err)
	}
	st.disabled = true
	if _, err := f.Service.Refresh(ctx, s2.RefreshToken); reason(err) != core.ReasonTokenInvalid {
		t.Fatalf("refresh of a disabled tenant: %v", err)
	}
	// The family is revoked, so re-enabling does not revive the session.
	st.disabled = false
	if _, err := f.Service.Refresh(ctx, s2.RefreshToken); err == nil {
		t.Fatal("the session survived its tenant being disabled")
	}
}

// TestRegress_G_SVC_22_MemIdemAbandonKeepsCompleted: MemStore.IdemAbandon
// deleted the row unconditionally while Postgres deletes only an in-flight
// one, so in memory a request whose stale row another request had taken
// over and completed erased that stored response on its own 5xx.
func TestRegress_G_SVC_22_MemIdemAbandonKeepsCompleted(t *testing.T) {
	ctx := context.Background()
	st := identity.NewMemStore()
	tenant := core.MustParseUUID("0190a1b2-0000-7000-8000-00000000a001")
	rec := identity.IdemRecord{TenantID: tenant, Key: "k", Method: "POST", Path: "/v1/runs", CreatedAt: t0, ExpiresAt: t0.Add(24 * time.Hour)}
	if _, fresh, err := st.IdemBegin(ctx, rec, time.Minute); err != nil || !fresh {
		t.Fatal(fresh, err)
	}
	if err := st.IdemComplete(ctx, tenant, "k", 201, []byte("{}"), "application/json", t0); err != nil {
		t.Fatal(err)
	}
	if err := st.IdemAbandon(ctx, tenant, "k"); err != nil {
		t.Fatal(err)
	}
	if got, fresh, _ := st.IdemBegin(ctx, rec, time.Minute); fresh || got.Status != 201 {
		t.Fatalf("completed response lost: fresh=%v %+v", fresh, got)
	}
}
