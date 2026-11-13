package scope_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestRegress_G_SEC_02_KillSwitchLevels pins SetKillSwitch authorization
// by level (GAPS G-SEC-02): before the fix any tenant owner could set the
// global switch, which stops every tenant's runs, and any identity of a
// tenant (a viewer, a service) could set its tenant and program switches.
// Every caller kind sets and then clears every level through the real
// gRPC stack (tokens minted and verified), so the operator's nil-tenant
// token is exercised end to end.
func TestRegress_G_SEC_02_KillSwitchLevels(t *testing.T) {
	t.Run("memory", func(t *testing.T) { runKillSwitchLevels(t, "") })
	t.Run("postgres", func(t *testing.T) { runKillSwitchLevels(t, servicetest.FreshDB(t)) })
}

func runKillSwitchLevels(t *testing.T, dsn string) {
	st, a, b := servicetest.Stack(t, devstack.Options{DBDSN: dsn, Resolver: servicetest.NewResolver(nil)})
	progA, err := st.Scope.CreateProgram(a, &pb.CreateProgramRequest{Name: "acme", InitialScope: servicetest.ScopeDoc("app.test")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Scope.CreateProgram(b, &pb.CreateProgramRequest{Name: "other", InitialScope: servicetest.ScopeDoc("b.test")}); err != nil {
		t.Fatal(err)
	}
	bg := context.Background()
	const apikeyA = "apikey:0190a1b2-0000-7000-8000-0000000000a9"
	as := func(tenant core.UUID, actor string, role auth.Role) context.Context {
		return st.As(bg, tenant, actor, role)
	}

	// Expected outcome per level, in the order of levels below.
	const (
		ok       = "ok"
		denied   = "denied"   // PERMISSION_DENIED FORBIDDEN_ROLE
		notFound = "notfound" // the target is another tenant's
	)
	levels := []struct {
		name string
		req  *pb.SetKillSwitchRequest
	}{
		{"global", &pb.SetKillSwitchRequest{Level: scopesvc.LevelGlobal}},
		{"tenant(own)", &pb.SetKillSwitchRequest{Level: scopesvc.LevelTenant}},
		{"tenant(A)", &pb.SetKillSwitchRequest{Level: scopesvc.LevelTenant, TargetId: core.PublicID(core.PrefixTenant, servicetest.TenantA)}},
		{"program(A)", &pb.SetKillSwitchRequest{Level: scopesvc.LevelProgram, TargetId: progA.GetId()}},
	}
	callers := []struct {
		name string
		ctx  context.Context
		want [4]string
	}{
		{"operator", as(core.Nil, platform.OperatorActor, auth.RoleViewer), [4]string{ok, denied, denied, denied}},
		// The operator actor with a real tenant is an ordinary service.
		{"operator actor on tenant A", as(servicetest.TenantA, platform.OperatorActor, auth.RoleOwner), [4]string{denied, denied, denied, denied}},
		{"owner A", as(servicetest.TenantA, servicetest.UserA, auth.RoleOwner), [4]string{denied, ok, ok, ok}},
		{"admin A", as(servicetest.TenantA, servicetest.UserA, auth.RoleAdmin), [4]string{denied, ok, ok, ok}},
		{"admin API key A", as(servicetest.TenantA, apikeyA, auth.RoleAdmin), [4]string{denied, ok, ok, ok}},
		{"approver A", as(servicetest.TenantA, servicetest.UserA, auth.RoleApprover), [4]string{denied, denied, denied, denied}},
		{"analyst A", as(servicetest.TenantA, servicetest.UserA, auth.RoleAnalyst), [4]string{denied, denied, denied, denied}},
		{"viewer A", as(servicetest.TenantA, servicetest.UserA, auth.RoleViewer), [4]string{denied, denied, denied, denied}},
		{"viewer API key A", as(servicetest.TenantA, apikeyA, auth.RoleViewer), [4]string{denied, denied, denied, denied}},
		// Tenant B's owner manages B's own switch, never A's.
		{"owner B", as(servicetest.TenantB, servicetest.UserB, auth.RoleOwner), [4]string{denied, ok, notFound, notFound}},
		// Services act for tenants but are not members (spec 13.6), even
		// with an owner role in the token.
		{"service A (engine)", as(servicetest.TenantA, platform.ServiceActor("engine"), auth.RoleAnalyst), [4]string{denied, denied, denied, denied}},
		{"service A with owner role", as(servicetest.TenantA, platform.ServiceActor("engine"), auth.RoleOwner), [4]string{denied, denied, denied, denied}},
		{"schedule A with admin role", as(servicetest.TenantA, "schedule:0190a1b2-0000-7000-8000-0000000000c1", auth.RoleAdmin), [4]string{denied, denied, denied, denied}},
	}
	for _, c := range callers {
		for i, l := range levels {
			for _, enabled := range []bool{true, false} {
				req := &pb.SetKillSwitchRequest{Level: l.req.GetLevel(), TargetId: l.req.GetTargetId(), Enabled: enabled, Reason: "test"}
				_, err := st.Scope.SetKillSwitch(c.ctx, req)
				switch c.want[i] {
				case ok:
					if err != nil {
						t.Errorf("%s sets %s (enabled %v): %v", c.name, l.name, enabled, err)
					}
				case denied:
					if !isKind(err, core.KindPermissionDenied, core.ReasonForbiddenRole) {
						t.Errorf("%s sets %s (enabled %v): want PERMISSION_DENIED FORBIDDEN_ROLE, got %v", c.name, l.name, enabled, err)
					}
				case notFound:
					if !isKind(err, core.KindNotFound, core.ReasonNotFound) {
						t.Errorf("%s sets %s (enabled %v): want NOT_FOUND, got %v", c.name, l.name, enabled, err)
					}
				}
			}
		}
	}
	// An unknown level is invalid for everyone, the operator included.
	for _, c := range callers[:3] {
		_, err := st.Scope.SetKillSwitch(c.ctx, &pb.SetKillSwitchRequest{Level: "platform", Enabled: true})
		servicetest.IsKind(t, err, core.KindInvalidArgument, "")
	}

	// Only the operator ever wrote the global switch. Postgres keeps
	// set_by only for user actors (a nil uuid, read back as "", for
	// services), so a user write would show as user:<uuid>.
	ks, err := st.Scope.GetKillSwitches(a, &pb.GetKillSwitchesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range ks.GetSwitches() {
		if by := k.GetSetBy(); k.GetLevel() == scopesvc.LevelGlobal && by != platform.OperatorActor && by != "" {
			t.Fatalf("global switch written by %q", k.GetSetBy())
		}
		if k.GetEnabled() {
			t.Fatalf("switch left engaged: %v", k)
		}
	}

	// The operator's global switch reaches tenant A's checks, and clearing
	// it releases them.
	op := as(core.Nil, platform.OperatorActor, auth.RoleViewer)
	check := func() string {
		t.Helper()
		d, err := st.Scope.Check(a, &pb.CheckRequest{ProgramId: progA.GetId(), Target: "app.test", Kind: pb.TargetKind_TARGET_KIND_HOST,
			ActivityClass: pb.ActivityClass_ACTIVITY_CLASS_PASSIVE})
		if err != nil {
			t.Fatal(err)
		}
		return d.GetReason()
	}
	if _, err := st.Scope.SetKillSwitch(op, &pb.SetKillSwitchRequest{Level: scopesvc.LevelGlobal, Enabled: true, Reason: "abuse"}); err != nil {
		t.Fatal(err)
	}
	if r := check(); r != "kill_switch" {
		t.Fatalf("global switch engaged: check reason %q", r)
	}
	if _, err := st.Scope.SetKillSwitch(op, &pb.SetKillSwitchRequest{Level: scopesvc.LevelGlobal, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	if r := check(); r != "allow" {
		t.Fatalf("global switch cleared: check reason %q", r)
	}
}

// TestRegress_G_SEC_02_KillSwitchUntokenedIdentities covers identities the
// minter refuses to put in a token (no role, an unknown role) and a call
// with no identity, against the server directly: all fail closed.
func TestRegress_G_SEC_02_KillSwitchUntokenedIdentities(t *testing.T) {
	repo := scopesvc.NewMemRepo()
	srv := scopesvc.NewServer(scopesvc.Options{Repo: repo, Clock: testkit.NewFakeClock(time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)), Resolver: servicetest.NewResolver(nil)})
	admin := platform.WithIdentity(context.Background(), platform.Identity{Tenant: servicetest.TenantA, Actor: servicetest.UserA, Role: auth.RoleAdmin})
	p, err := srv.CreateProgram(admin, &pb.CreateProgramRequest{Name: "acme"})
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []auth.Role{"", "superuser"} {
		ctx := platform.WithIdentity(context.Background(), platform.Identity{Tenant: servicetest.TenantA, Actor: servicetest.UserA, Role: role})
		for _, req := range []*pb.SetKillSwitchRequest{
			{Level: scopesvc.LevelGlobal, Enabled: true},
			{Level: scopesvc.LevelTenant, Enabled: true},
			{Level: scopesvc.LevelProgram, TargetId: p.GetId(), Enabled: true},
		} {
			_, err := srv.SetKillSwitch(ctx, req)
			servicetest.IsKind(t, err, core.KindPermissionDenied, core.ReasonForbiddenRole)
		}
	}
	_, err = srv.SetKillSwitch(context.Background(), &pb.SetKillSwitchRequest{Level: scopesvc.LevelGlobal, Enabled: true})
	servicetest.IsKind(t, err, core.KindUnauthenticated, "")
	if kss, _ := repo.KillSwitches(context.Background(), servicetest.TenantA); len(kss) != 0 {
		t.Fatalf("denied calls stored switches: %v", kss)
	}
}

func isKind(err error, kind core.Kind, reason string) bool {
	var ce *core.Error
	return errors.As(err, &ce) && ce.Kind == kind && ce.Reason == reason
}
