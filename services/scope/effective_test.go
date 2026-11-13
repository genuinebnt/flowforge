package scope_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
	"github.com/genuinebnt/flowforge/testkit"
)

// direct adapts a Server to pb.ScopeServiceClient without a network, for
// the parts of the client contract the test exercises.
type direct struct {
	pb.ScopeServiceClient
	s *scopesvc.Server
}

func (d direct) GetScope(ctx context.Context, r *pb.GetScopeRequest, _ ...grpc.CallOption) (*pb.ScopeVersion, error) {
	return d.s.GetScope(ctx, r)
}

func TestRegress_G_CORE_01_EffectiveFrom_Memory(t *testing.T) {
	runEffectiveFrom(t, scopesvc.NewMemRepo())
}

func TestRegress_G_CORE_01_EffectiveFrom_Postgres(t *testing.T) {
	dsn := servicetest.FreshDB(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrations.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	runEffectiveFrom(t, &scopesvc.PgRepo{DB: pool})
}

// runEffectiveFrom: a future-dated scope widening is stored and visible as
// pending, but Check (version 0), GetScope (version 0) and engine pinning
// keep using the version in effect until effective_from passes on the
// injected clock; a pinned check of the pending version fails closed.
func runEffectiveFrom(t *testing.T, repo scopesvc.Repo) {
	ctx := context.Background()
	clock := testkit.NewFakeClock(time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC))
	resolver := servicetest.NewResolver(map[string]string{"app.test": "203.0.113.10", "api.test": "203.0.113.11"})
	srv := scopesvc.NewServer(scopesvc.Options{Repo: repo, Clock: clock, Resolver: resolver})
	a := platform.WithIdentity(ctx, platform.Identity{Tenant: servicetest.TenantA, Actor: servicetest.UserA, Role: "admin"})

	p, err := srv.CreateProgram(a, &pb.CreateProgramRequest{Name: "acme", InitialScope: servicetest.ScopeDoc("app.test")})
	if err != nil {
		t.Fatal(err)
	}
	future := clock.Now().Add(time.Hour)
	v2, err := srv.PutScope(a, &pb.PutScopeRequest{ProgramId: p.GetId(), BaseVersion: 1,
		Document: servicetest.ScopeDoc("app.test", "api.test"), EffectiveFrom: timestamppb.New(future)})
	if err != nil || v2.GetVersion() != 2 {
		t.Fatalf("put: %v %v", v2, err)
	}
	check := func(version int32) (*pb.Decision, error) {
		return srv.Check(a, &pb.CheckRequest{ProgramId: p.GetId(), ScopeVersion: version, Target: "api.test",
			Kind: pb.TargetKind_TARGET_KIND_HOST, ActivityClass: pb.ActivityClass_ACTIVITY_CLASS_PASSIVE})
	}
	client := scopesvc.NewClient(direct{s: srv}, "engine")

	// Before effective_from: version 1 governs.
	d, err := check(0)
	if err != nil || d.GetResult() != pb.DecisionResult_DECISION_RESULT_DENY || d.GetScopeVersion() != 1 {
		t.Fatalf("future widening applied early: %v %v", d, err)
	}
	if cur, err := srv.GetScope(a, &pb.GetScopeRequest{ProgramId: p.GetId()}); err != nil || cur.GetVersion() != 1 {
		t.Fatalf("current version before effective_from: %v %v", cur, err)
	}
	if pin, err := client.CurrentVersion(ctx, servicetest.TenantA, programUUID(t, p.GetId())); err != nil || pin != 1 {
		t.Fatalf("engine pin before effective_from = %d %v", pin, err)
	}
	_, err = check(2)
	servicetest.IsKind(t, err, core.KindFailedPrecondition, core.ReasonValidationFailed)
	// The pending version is visible by number and in the version list.
	if pend, err := srv.GetScope(a, &pb.GetScopeRequest{ProgramId: p.GetId(), Version: 2}); err != nil || !pend.GetEffectiveFrom().AsTime().Equal(future) {
		t.Fatalf("pending version: %v %v", pend, err)
	}
	if vs, err := srv.ListScopeVersions(a, &pb.ListScopeVersionsRequest{ProgramId: p.GetId()}); err != nil || len(vs.GetVersions()) != 2 {
		t.Fatalf("versions: %v %v", vs, err)
	}
	// Nothing is due yet, so no activation notice is written.
	if last, err := srv.ActivateDue(ctx, clock.Now().Add(-time.Hour), clock.Now()); err != nil || !last.Equal(clock.Now().Add(-time.Hour)) {
		t.Fatalf("activation before due: %v %v", last, err)
	}

	// At effective_from: version 2 governs.
	clock.Advance(time.Hour)
	d, err = check(0)
	if err != nil || d.GetResult() != pb.DecisionResult_DECISION_RESULT_ALLOW || d.GetScopeVersion() != 2 {
		t.Fatalf("widening not applied at effective_from: %v %v", d, err)
	}
	if pin, err := client.CurrentVersion(ctx, servicetest.TenantA, programUUID(t, p.GetId())); err != nil || pin != 2 {
		t.Fatalf("engine pin after effective_from = %d %v", pin, err)
	}
	if _, err := check(2); err != nil {
		t.Fatalf("pinned check of an effective version: %v", err)
	}
	last, err := srv.ActivateDue(ctx, clock.Now().Add(-2*time.Hour), clock.Now())
	if err != nil || !last.Equal(future) {
		t.Fatalf("activation: %v %v", last, err)
	}
	// A second pass over the same window re-emits nothing new.
	if _, err := srv.ActivateDue(ctx, clock.Now().Add(-2*time.Hour), clock.Now()); err != nil {
		t.Fatal(err)
	}
	var published, scheduled int
	_, _ = repo.Outbox().Claim(ctx, 100, func(_ context.Context, rows []bus.OutboxRow) error {
		for _, r := range rows {
			switch eventType(t, r.Payload) {
			case scopesvc.EventVersionPublished:
				published++
			case scopesvc.EventVersionScheduled:
				scheduled++
			}
		}
		return nil
	})
	// version 1 at creation, version 2 once on activation; version 2 was
	// announced as scheduled at PutScope.
	if published != 2 || scheduled != 1 {
		t.Fatalf("outbox: %d published, %d scheduled", published, scheduled)
	}
}

func programUUID(t *testing.T, id string) core.UUID {
	t.Helper()
	u, err := core.ParsePublicID(core.PrefixProgram, id)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func eventType(t *testing.T, payload []byte) string {
	t.Helper()
	var v struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(payload, &v); err != nil {
		t.Fatal(err)
	}
	return v.Type
}
