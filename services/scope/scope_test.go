package scope_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
)

func TestScopeService_Memory(t *testing.T)   { runScope(t, "") }
func TestScopeService_Postgres(t *testing.T) { runScope(t, servicetest.FreshDB(t)) }

func runScope(t *testing.T, dsn string) {
	resolver := servicetest.NewResolver(map[string]string{"app.test": "203.0.113.10"})
	st, a, b := servicetest.Stack(t, devstack.Options{DBDSN: dsn, Resolver: resolver})
	var mu sync.Mutex
	var events []string
	_ = st.Bus.Subscribe(a, "scope.events", "test", func(_ context.Context, env *pb.EventEnvelope) error {
		mu.Lock()
		events = append(events, env.GetType())
		mu.Unlock()
		return nil
	})

	p, err := st.Scope.CreateProgram(a, &pb.CreateProgramRequest{Name: "acme", InitialScope: servicetest.ScopeDoc("app.test")})
	if err != nil {
		t.Fatal(err)
	}
	if p.GetCurrentVersion() != 1 {
		t.Fatalf("version %d", p.GetCurrentVersion())
	}
	_, err = st.Scope.CreateProgram(a, &pb.CreateProgramRequest{Name: "acme"})
	servicetest.IsKind(t, err, core.KindAlreadyExists, core.ReasonAlreadyExists)

	// Tenant isolation: B sees nothing of A's program, on every path.
	_, err = st.Scope.GetProgram(b, &pb.GetProgramRequest{ProgramId: p.GetId()})
	servicetest.IsKind(t, err, core.KindNotFound, core.ReasonNotFound)
	_, err = st.Scope.GetScope(b, &pb.GetScopeRequest{ProgramId: p.GetId()})
	servicetest.IsKind(t, err, core.KindNotFound, core.ReasonNotFound)
	_, err = st.Scope.ListScopeVersions(b, &pb.ListScopeVersionsRequest{ProgramId: p.GetId()})
	servicetest.IsKind(t, err, core.KindNotFound, core.ReasonNotFound)
	_, err = st.Scope.PutScope(b, &pb.PutScopeRequest{ProgramId: p.GetId(), BaseVersion: 1, Document: servicetest.ScopeDoc("x.test")})
	servicetest.IsKind(t, err, core.KindNotFound, core.ReasonNotFound)
	_, err = st.Scope.Check(b, &pb.CheckRequest{ProgramId: p.GetId(), Target: "app.test", Kind: pb.TargetKind_TARGET_KIND_HOST, ActivityClass: pb.ActivityClass_ACTIVITY_CLASS_PASSIVE})
	servicetest.IsKind(t, err, core.KindNotFound, core.ReasonNotFound)
	_, err = st.Scope.SetKillSwitch(b, &pb.SetKillSwitchRequest{Level: "program", TargetId: p.GetId(), Enabled: true})
	servicetest.IsKind(t, err, core.KindNotFound, core.ReasonNotFound)
	if l, err := st.Scope.ListPrograms(b, &pb.ListProgramsRequest{}); err != nil || len(l.GetPrograms()) != 0 {
		t.Fatalf("B lists %v (%v)", l.GetPrograms(), err)
	}
	if l, err := st.Scope.ListPrograms(a, &pb.ListProgramsRequest{}); err != nil || len(l.GetPrograms()) != 1 {
		t.Fatalf("A lists %v (%v)", l.GetPrograms(), err)
	}

	// Optimistic concurrency.
	_, err = st.Scope.PutScope(a, &pb.PutScopeRequest{ProgramId: p.GetId(), BaseVersion: 0, Document: servicetest.ScopeDoc("app.test")})
	servicetest.IsKind(t, err, core.KindAborted, core.ReasonVersionConflict)
	v2, err := st.Scope.PutScope(a, &pb.PutScopeRequest{ProgramId: p.GetId(), BaseVersion: 1, Document: servicetest.ScopeDoc("app.test", "api.test")})
	if err != nil || v2.GetVersion() != 2 {
		t.Fatalf("put scope: %v %v", v2, err)
	}
	_, err = st.Scope.PutScope(a, &pb.PutScopeRequest{ProgramId: p.GetId(), BaseVersion: 1, Document: servicetest.ScopeDoc()})
	servicetest.IsKind(t, err, core.KindAborted, core.ReasonVersionConflict)
	_, err = st.Scope.PutScope(a, &pb.PutScopeRequest{ProgramId: p.GetId(), BaseVersion: 2, Document: &pb.ScopeDocument{Rules: []*pb.ScopeRule{{Id: "x", Effect: pb.RuleEffect_RULE_EFFECT_ALLOW, Kind: pb.RuleKind_RULE_KIND_DOMAIN, Value: "bad..name"}}}})
	servicetest.IsKind(t, err, core.KindInvalidArgument, core.ReasonValidationFailed)
	vs, err := st.Scope.ListScopeVersions(a, &pb.ListScopeVersionsRequest{ProgramId: p.GetId(), Page: &pb.PageRequest{PageSize: 1}})
	if err != nil || len(vs.GetVersions()) != 1 || vs.GetPage().GetNextPageToken() == "" {
		t.Fatalf("paging: %v %v", vs, err)
	}

	// Check: active checks resolve and pin; the version is pinned when given.
	check := func(ctx context.Context, target string, version int32, class pb.ActivityClass) *pb.Decision {
		t.Helper()
		d, err := st.Scope.Check(ctx, &pb.CheckRequest{ProgramId: p.GetId(), ScopeVersion: version, Target: target, Kind: pb.TargetKind_TARGET_KIND_HOST, ActivityClass: class, Resolve: true})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := check(a, "app.test", 0, pb.ActivityClass_ACTIVITY_CLASS_ACTIVE)
	if d.GetResult() != pb.DecisionResult_DECISION_RESULT_ALLOW || !slices.Equal(d.GetPinnedIps(), []string{"203.0.113.10"}) || d.GetScopeVersion() != 2 || d.GetDecisionId() == "" {
		t.Fatalf("decision %v", d)
	}
	if d := check(a, "api.test", 1, pb.ActivityClass_ACTIVITY_CLASS_PASSIVE); d.GetResult() != pb.DecisionResult_DECISION_RESULT_DENY || d.GetReason() != "no_matching_allow" {
		t.Fatalf("pinned version 1 must not allow api.test: %v", d)
	}
	if d := check(a, "api.test", 0, pb.ActivityClass_ACTIVITY_CLASS_ACTIVE); d.GetReason() != "resolution_failed" {
		t.Fatalf("unresolvable active target: %v", d)
	}
	batch, err := st.Scope.CheckBatch(a, &pb.CheckBatchRequest{Requests: []*pb.CheckRequest{
		{ProgramId: p.GetId(), Target: "app.test", ActivityClass: pb.ActivityClass_ACTIVITY_CLASS_PASSIVE},
		{ProgramId: p.GetId(), Target: "https://app.test@evil.test/", ActivityClass: pb.ActivityClass_ACTIVITY_CLASS_PASSIVE},
	}})
	if err != nil || len(batch.GetDecisions()) != 2 || batch.GetDecisions()[1].GetReason() != "invalid_target" {
		t.Fatalf("batch %v %v", batch, err)
	}
	// Kill switches are checked first.
	if _, err := st.Scope.SetKillSwitch(a, &pb.SetKillSwitchRequest{Level: "program", TargetId: p.GetId(), Enabled: true, Reason: "incident"}); err != nil {
		t.Fatal(err)
	}
	if d := check(a, "app.test", 0, pb.ActivityClass_ACTIVITY_CLASS_PASSIVE); d.GetReason() != "kill_switch" {
		t.Fatalf("kill switch: %v", d)
	}
	ks, err := st.Scope.GetKillSwitches(a, &pb.GetKillSwitchesRequest{})
	if err != nil || len(ks.GetSwitches()) != 1 {
		t.Fatalf("switches %v %v", ks, err)
	}
	if ks, _ := st.Scope.GetKillSwitches(b, &pb.GetKillSwitchesRequest{}); len(ks.GetSwitches()) != 0 {
		t.Fatalf("B sees A's switch: %v", ks)
	}
	_, err = st.Scope.SetKillSwitch(a, &pb.SetKillSwitchRequest{Level: "global", Enabled: true})
	servicetest.IsKind(t, err, core.KindPermissionDenied, core.ReasonForbiddenRole)

	// Every decision is in the decision log, and can be read back by id.
	rec, err := scopesvc.DecisionLog{Repo: st.ScopeRepo}.Decision(a, servicetest.TenantA, d.GetDecisionId())
	if err != nil || !rec.Decision.Allowed() || rec.Decision.PinnedIPs[0].String() != "203.0.113.10" {
		t.Fatalf("decision log: %+v %v", rec, err)
	}
	if _, err := (scopesvc.DecisionLog{Repo: st.ScopeRepo}).Decision(a, servicetest.TenantB, d.GetDecisionId()); !errors.Is(err, scopesvc.ErrNotFound) {
		t.Fatalf("another tenant read a decision: %v", err)
	}
	servicetest.Eventually(t, 5*time.Second, "scope.events", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(events, scopesvc.EventProgramCreated) && slices.Contains(events, scopesvc.EventVersionPublished) && slices.Contains(events, scopesvc.EventKillSwitchSet)
	})
}

type failingRepo struct{ scopesvc.Repo }

func (failingRepo) RecordDecisions(context.Context, []scopesvc.DecisionRecord) error {
	return errors.New("disk full")
}

// TestScopeService_FailClosed: a decision that cannot be recorded is never
// returned (UNAVAILABLE, which every caller treats as deny).
func TestScopeService_FailClosed(t *testing.T) {
	mem := scopesvc.NewMemRepo()
	srv := scopesvc.NewServer(scopesvc.Options{Repo: failingRepo{mem}})
	ctx := platform.WithIdentity(context.Background(), platform.Identity{Tenant: servicetest.TenantA, Actor: servicetest.UserA, Role: "admin"})
	p, err := srv.CreateProgram(ctx, &pb.CreateProgramRequest{Name: "p", InitialScope: servicetest.ScopeDoc("app.test")})
	if err != nil {
		t.Fatal(err)
	}
	d, err := srv.Check(ctx, &pb.CheckRequest{ProgramId: p.GetId(), Target: "app.test", ActivityClass: pb.ActivityClass_ACTIVITY_CLASS_PASSIVE})
	if d != nil {
		t.Fatalf("returned an unrecorded decision %v", d)
	}
	servicetest.IsKind(t, err, core.KindUnavailable, core.ReasonUnavailable)
	// A program without a scope document cannot allow anything.
	p2, _ := srv.CreateProgram(ctx, &pb.CreateProgramRequest{Name: "empty"})
	_, err = srv.Check(ctx, &pb.CheckRequest{ProgramId: p2.GetId(), Target: "app.test"})
	servicetest.IsKind(t, err, core.KindFailedPrecondition, "")
}
