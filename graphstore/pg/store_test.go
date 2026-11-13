package pg_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/graphstore/pg"
	"github.com/genuinebnt/flowforge/graphstore/storetest"
	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/testkit"
)

// The tests run against FF_TEST_PG_DSN: a migrated template database is
// created once, and every store gets a fresh clone of it.

var (
	tmplOnce sync.Once
	tmplName string
	tmplErr  error
	dbSeq    atomic.Int64
)

func dsn(t testing.TB) string {
	t.Helper()
	d := os.Getenv("FF_TEST_PG_DSN")
	if d == "" {
		t.Skip("FF_TEST_PG_DSN not set")
	}
	return d
}

func TestMain(m *testing.M) {
	code := m.Run()
	if tmplName != "" {
		ctx := context.Background()
		if c, err := pgx.Connect(ctx, os.Getenv("FF_TEST_PG_DSN")); err == nil {
			_, _ = c.Exec(ctx, "DROP DATABASE IF EXISTS "+tmplName+" WITH (FORCE)")
			_ = c.Close(ctx)
		}
	}
	os.Exit(code)
}

func template(t testing.TB) string {
	t.Helper()
	base := dsn(t)
	tmplOnce.Do(func() {
		ctx := context.Background()
		admin, err := pgx.Connect(ctx, base)
		if err != nil {
			tmplErr = err
			return
		}
		defer admin.Close(ctx)
		name := fmt.Sprintf("ff_graph_tmpl_%d", os.Getpid())
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
			tmplErr = err
			return
		}
		cfg, _ := pgx.ParseConfig(base)
		cfg.Database = name
		c, err := pgx.ConnectConfig(ctx, cfg)
		if err != nil {
			tmplErr = err
			return
		}
		tmplErr = migrations.Apply(ctx, c)
		_ = c.Close(ctx)
		tmplName = name
	})
	if tmplErr != nil {
		t.Fatalf("template database: %v", tmplErr)
	}
	return tmplName
}

// freshPool returns a pool on a new clone of the migrated template.
func freshPool(t testing.TB) (*pgxpool.Pool, string) { return freshPoolAs(t, "") }

// freshPoolAs is freshPool with every connection running as role (empty:
// the DSN's superuser), through the "role" startup parameter.
func freshPoolAs(t testing.TB, role string) (*pgxpool.Pool, string) {
	t.Helper()
	tmpl := template(t)
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("ff_graph_%d_%d", os.Getpid(), dbSeq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name+" TEMPLATE "+tmpl); err != nil {
		t.Fatal(err)
	}
	cfg, err := pgxpool.ParseConfig(dsn(t))
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	if role != "" {
		cfg.ConnConfig.RuntimeParams["role"] = role
	}
	cfg.MaxConns = 16
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(ctx)
	})
	return pool, name
}

func pgFactory(t testing.TB, o storetest.Options) graphstore.Store {
	pool, _ := freshPool(t)
	return pg.New(pool, pg.Config{Config: o.Config(), PollInterval: 50 * time.Millisecond})
}

func memFactory(_ testing.TB, o storetest.Options) graphstore.Store {
	return graphstore.NewMem(o.Config())
}

func mergeCases(t *testing.T) []storetest.MergeCase {
	var out []storetest.MergeCase
	for _, c := range testkit.LoadCases[storetest.MergeFile](t, "oam/merge") {
		out = append(out, storetest.MergeCase{Name: c.Name, Data: c.Data})
	}
	return out
}

func TestGraphIngest_MergeConformance(t *testing.T) {
	dsn(t)
	storetest.MergeConformance(t, pgFactory, mergeCases(t), memFactory)
}
func TestGraphIngest_Idempotent(t *testing.T) { storetest.Idempotent(t, pgFactory) }
func TestGraphIngest_Atomic(t *testing.T)     { storetest.Atomic(t, pgFactory) }
func TestGraphProp_Convergence(t *testing.T) {
	dsn(t)
	storetest.Convergence(t, pgFactory, testkit.Rng(t), max(testkit.Cases()/20, 10), memFactory)
}
func TestGraphNeighbors(t *testing.T)              { storetest.Neighbors(t, pgFactory) }
func TestGraphPath(t *testing.T)                   { storetest.Path(t, pgFactory) }
func TestGraphBlastRadius_Components(t *testing.T) { storetest.BlastAndComponents(t, pgFactory) }
func TestGraphFindEntities(t *testing.T)           { storetest.FindEntities(t, pgFactory) }
func TestGraphSearch(t *testing.T)                 { storetest.Search(t, pgFactory) }
func TestGraphDiff(t *testing.T)                   { storetest.Diff(t, pgFactory) }
func TestGraphStaleness(t *testing.T)              { storetest.Staleness(t, pgFactory) }
func TestGraphTenantIsolation(t *testing.T)        { storetest.TenantIsolation(t, pgFactory) }
func TestGraphWatchChanges_Resume(t *testing.T)    { storetest.WatchResume(t, pgFactory) }
func TestGraphSnapshot_Deterministic(t *testing.T) {
	dsn(t)
	storetest.Snapshot(t, pgFactory, memFactory)
}
func TestGraphAddTag(t *testing.T) { storetest.AddTag(t, pgFactory) }

func TestOAMInv05_AuthoritativeConvergence(t *testing.T) {
	dsn(t)
	storetest.AuthConvergence(t, pgFactory, testkit.Rng(t), max(testkit.Cases()/20, 10), memFactory)
}
func TestOAMInv12_AsOf(t *testing.T) {
	dsn(t)
	storetest.AsOf(t, pgFactory, testkit.Rng(t), max(testkit.Cases()/100, 5))
}

// TestOAMInv13_MemEqualsPG checks OAM-INV-13: Postgres and Mem end with
// byte-identical canonical state and change logs on random v2 histories.
func TestOAMInv13_MemEqualsPG(t *testing.T) {
	dsn(t)
	storetest.SameHistory(t, pgFactory, testkit.Rng(t), max(testkit.Cases()/50, 10), memFactory)
}
func TestOAMInv14_Commonness(t *testing.T) { storetest.Commonness(t, pgFactory) }
func TestGraphContentChanges(t *testing.T) { storetest.ContentChanges(t, pgFactory) }

// TestGraphIngest_ConcurrentAuthority ingests authoritative captures of
// the same pages from 8 goroutines (with duplicates) and compares the
// result with Mem: the head preload and retry keep concurrent
// replacements convergent.
func TestGraphIngest_ConcurrentAuthority(t *testing.T) {
	pool, _ := freshPool(t)
	s := pg.New(pool, pg.Config{})
	mem := graphstore.NewMem(graphstore.Config{})
	r := testkit.Rng(t)
	tenant, prog := storetest.TenantID("ca"), storetest.ProgramID("ca")
	var list []graphstore.IngestRequest
	for i := range 120 {
		list = append(list, graphstore.IngestRequest{TenantID: tenant, ProgramID: prog, Observation: storetest.RandomAuthObservation(r, i, "", "")})
	}
	ctx := context.Background()
	for _, req := range list {
		if _, err := mem.IngestObservation(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range list {
				if _, err := s.IngestObservation(ctx, list[(i*(w+3))%len(list)]); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	c := graphstore.Caller{TenantID: tenant}
	a, _ := s.Export(ctx, c, prog)
	b, _ := mem.Export(ctx, c, prog)
	if storetest.Canon(t, a) != storetest.Canon(t, b) {
		t.Fatal("concurrent authoritative ingest diverged from Mem")
	}
}

// BenchmarkGraphAuth_Ingest is the authoritative ingest benchmark on
// Postgres (5 pages x 200 edges; -benchtime 1000x for 1,000 re-captures).
func BenchmarkGraphAuth_Ingest(b *testing.B) {
	pool, _ := freshPool(b)
	storetest.BenchAuthoritative(b, pg.New(pool, pg.Config{}), 5, 200)
}

// TestGraphIngest_ConcurrentConvergence ingests the same observation
// multiset from many goroutines (with duplicates) and compares the result
// with Mem: concurrent writers converge (spec 23.5).
func TestGraphIngest_ConcurrentConvergence(t *testing.T) {
	pool, _ := freshPool(t)
	s := pg.New(pool, pg.Config{})
	mem := graphstore.NewMem(graphstore.Config{})
	r := testkit.Rng(t)
	tenant, prog := storetest.TenantID("cc"), storetest.ProgramID("cc")
	var list []graphstore.IngestRequest
	for i := range 200 {
		list = append(list, graphstore.IngestRequest{TenantID: tenant, ProgramID: prog, Observation: storetest.RandomObservation(r, i, "", "")})
	}
	ctx := context.Background()
	for _, req := range list {
		if _, err := mem.IngestObservation(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range list {
				if _, err := s.IngestObservation(ctx, list[(i*(w+3))%len(list)]); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	c := graphstore.Caller{TenantID: tenant}
	a, _ := s.Export(ctx, c, prog)
	b, _ := mem.Export(ctx, c, prog)
	if storetest.Canon(t, a) != storetest.Canon(t, b) {
		t.Fatal("concurrent ingest diverged from Mem")
	}
	evs, err := s.ReadChanges(ctx, c, graphstore.ChangesRequest{ProgramID: prog, Limit: graphstore.MaxPageSize})
	if err != nil || len(evs) == 0 {
		t.Fatalf("changes: %d %v", len(evs), err)
	}
}

// TestGraphPG_RowLevelSecurity connects as a role subject to the RLS
// policies: the store still works (it sets app.tenant_id per
// transaction) and a session that does not set the tenant sees nothing.
func TestGraphPG_RowLevelSecurity(t *testing.T) {
	pool, db := freshPool(t)
	ctx := context.Background()
	role := fmt.Sprintf("ff_graph_app_%d", os.Getpid())
	for _, q := range []string{
		"DROP ROLE IF EXISTS " + role,
		"CREATE ROLE " + role + " LOGIN",
		"GRANT USAGE ON SCHEMA graph TO " + role,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA graph TO " + role, // DELETE: pending references
		"GRANT USAGE ON ALL SEQUENCES IN SCHEMA graph TO " + role,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	cfg, _ := pgxpool.ParseConfig(dsn(t))
	cfg.ConnConfig.Database, cfg.ConnConfig.User = db, role
	app, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		app.Close()
		_, _ = pool.Exec(ctx, "DROP OWNED BY "+role)
		_, _ = pool.Exec(ctx, "DROP ROLE IF EXISTS "+role)
	})
	s := pg.New(app, pg.Config{})
	tenant, prog := storetest.TenantID("rls"), storetest.ProgramID("rls")
	o := oam.Observation{ObservationID: "rls-1", Tool: oam.Tool{Name: "dnsx"}, ObservedAt: storetest.T0, Confidence: 0.9,
		Entities: []oam.ObservedEntity{{Ref: "a", Asset: oam.FQDN{Name: "rls.acme.com"}}}}
	res, err := s.IngestObservation(ctx, graphstore.IngestRequest{TenantID: tenant, ProgramID: prog, Observation: o})
	if err != nil || res.EntitiesCreated != 1 {
		t.Fatalf("ingest as app role: %+v %v", res, err)
	}
	if _, err := s.GetEntity(ctx, graphstore.Caller{TenantID: tenant}, graphstore.GetEntityRequest{EntityID: res.RefToEntityID["a"], IncludeTags: true}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := app.QueryRow(ctx, "SELECT count(*) FROM graph.entities").Scan(&n); err != nil || n != 0 {
		t.Fatalf("RLS without app.tenant_id: %d rows, %v", n, err)
	}
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM graph.entities").Scan(&n); err != nil || n != 1 {
		t.Fatalf("owner sees %d rows, %v", n, err)
	}
}

// TestGraphPG_Relay checks the outbox relay: publish, then mark; a failed
// publish marks nothing (at least once, never lost).
func TestGraphPG_Relay(t *testing.T) {
	pool, _ := freshPool(t)
	checkRelay(t, pool)
}

func checkRelay(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	s := pg.New(pool, pg.Config{})
	tenant, prog := storetest.TenantID("relay"), storetest.ProgramID("relay")
	for i := range 3 {
		o := oam.Observation{ObservationID: fmt.Sprint("relay-", i), Tool: oam.Tool{Name: "dnsx"}, ObservedAt: storetest.T0, Confidence: 0.9,
			Entities: []oam.ObservedEntity{{Ref: "a", Asset: oam.FQDN{Name: fmt.Sprintf("r%d.acme.com", i)}}}}
		if _, err := s.IngestObservation(ctx, graphstore.IngestRequest{TenantID: tenant, ProgramID: prog, Observation: o}); err != nil {
			t.Fatal(err)
		}
	}
	boom := errors.New("broker down")
	var got []graphstore.ChangeEvent
	relay := &pg.Relay{DB: pool, BatchSize: 2, Clock: storetest.NewClock(storetest.T0), Publish: func(_ context.Context, evs []graphstore.ChangeEvent) error {
		if boom != nil {
			return boom
		}
		got = append(got, evs...)
		return nil
	}}
	if _, err := relay.RunOnce(ctx); !errors.Is(err, boom) {
		t.Fatalf("failed publish: %v", err)
	}
	boom = nil
	total := 0
	for {
		n, err := relay.RunOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		total += n
	}
	if total != 3 || len(got) != 3 || got[0].Entity == nil || got[0].TenantID != tenant || got[0].Seq >= got[2].Seq {
		t.Fatalf("relayed %d: %+v", total, got)
	}
	var pending int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM graph.change_events WHERE published_at IS NULL").Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("pending after relay: %d %v", pending, err)
	}
}

// TestRegress_G_OPS_02_GraphAsServiceRole runs the store suites as
// ff_graph, the non-owner service role of migration 0018, with row-level
// security forced on every graph table: graphstore/pg must set
// app.tenant_id in every transaction, and the cross-tenant relay may only
// rely on its policy on graph.change_events (it reads entity snapshots per
// tenant).
func TestRegress_G_OPS_02_GraphAsServiceRole(t *testing.T) {
	dsn(t)
	asGraph := func(t testing.TB, o storetest.Options) graphstore.Store {
		pool, _ := freshPoolAs(t, "ff_graph")
		return pg.New(pool, pg.Config{Config: o.Config(), PollInterval: 50 * time.Millisecond})
	}
	t.Run("merge", func(t *testing.T) { storetest.MergeConformance(t, asGraph, mergeCases(t), memFactory) })
	t.Run("idempotent", func(t *testing.T) { storetest.Idempotent(t, asGraph) })
	t.Run("atomic", func(t *testing.T) { storetest.Atomic(t, asGraph) })
	t.Run("convergence", func(t *testing.T) {
		storetest.Convergence(t, asGraph, testkit.Rng(t), 10, memFactory)
	})
	t.Run("reads", func(t *testing.T) {
		storetest.Neighbors(t, asGraph)
		storetest.Path(t, asGraph)
		storetest.BlastAndComponents(t, asGraph)
		storetest.FindEntities(t, asGraph)
		storetest.Search(t, asGraph)
		storetest.Diff(t, asGraph)
	})
	t.Run("staleness", func(t *testing.T) { storetest.Staleness(t, asGraph) })
	t.Run("tenant isolation", func(t *testing.T) { storetest.TenantIsolation(t, asGraph) })
	t.Run("watch", func(t *testing.T) { storetest.WatchResume(t, asGraph) })
	t.Run("snapshot", func(t *testing.T) { storetest.Snapshot(t, asGraph, memFactory) })
	t.Run("add tag", func(t *testing.T) { storetest.AddTag(t, asGraph) })
	t.Run("relay", func(t *testing.T) {
		pool, _ := freshPoolAs(t, "ff_graph")
		checkRelay(t, pool)
	})
}

// TestGraphPG_SearchTextMaintained checks that search_text follows
// content merges (spec 10.7).
func TestGraphPG_SearchTextMaintained(t *testing.T) {
	pool, _ := freshPool(t)
	ctx := context.Background()
	s := pg.New(pool, pg.Config{})
	tenant, prog := storetest.TenantID("st"), storetest.ProgramID("st")
	ingest := func(id, out string, at time.Time) string {
		o := oam.Observation{ObservationID: id, Tool: oam.Tool{Name: "httpx"}, ObservedAt: at, Confidence: 0.9,
			Entities: []oam.ObservedEntity{{Ref: "s", Asset: oam.Service{UniqueID: "svc-1", Output: out}}}}
		res, err := s.IngestObservation(ctx, graphstore.IngestRequest{TenantID: tenant, ProgramID: prog, Observation: o})
		if err != nil {
			t.Fatal(err)
		}
		return res.RefToEntityID["s"]
	}
	id := ingest("s1", "Apache Tomcat", storetest.T0)
	ingest("s2", "Grafana v9", storetest.T0.Add(time.Minute))
	u, _ := core.ParsePublicID(core.PrefixEntity, id)
	var text string
	if err := pool.QueryRow(ctx, "SELECT search_text FROM graph.entities WHERE id = $1::uuid", u.String()).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != graphstore.SearchText(map[string]any{"unique_id": "svc-1", "output": "Grafana v9"}) {
		t.Fatalf("search_text %q", text)
	}
}

// BenchmarkSys01_Ingest is b_sys_01 against Postgres: benchWorkers
// concurrent writers into one program, as when the engine forwards
// observations from many tool runners at once.
func BenchmarkSys01_Ingest(b *testing.B) {
	pool, _ := freshPool(b)
	storetest.BenchIngest(b, pg.New(pool, pg.Config{}), benchWorkers)
}

const benchWorkers = 8

// TestRegress_G_CORE_03_PendingReferencesPG pins the Postgres side of
// pending references: observations that name an entity id racing with the
// ingest that creates it all end up applied (none stuck in
// graph.pending_observations), the per-tenant bound answers
// RESOURCE_EXHAUSTED, and old rows expire by observed_at on the next
// buffering ingest of the tenant.
func TestRegress_G_CORE_03_PendingReferencesPG(t *testing.T) {
	pool, _ := freshPool(t)
	ctx := context.Background()
	tenant, prog := storetest.TenantID("pend"), storetest.ProgramID("pend")
	clock := storetest.NewClock(storetest.T0)
	s := pg.New(pool, pg.Config{Config: graphstore.Config{Clock: clock, MaxPending: 50, PendingTTL: time.Hour}})
	target := oam.FQDN{Name: "target.acme.com"}
	id := storetest.EntityIDIn(tenant, prog, target)
	ref := func(i int, at time.Time) graphstore.IngestRequest {
		return graphstore.IngestRequest{TenantID: tenant, ProgramID: prog, Observation: oam.Observation{
			ObservationID: fmt.Sprint("ref-", i), Tool: oam.Tool{Name: "httpx"}, ObservedAt: at, Confidence: 0.5,
			Entities: []oam.ObservedEntity{{Ref: "u", Asset: oam.URL{URL: fmt.Sprintf("https://target.acme.com/%d", i)}}},
			Edges:    []oam.ObservedEdge{{From: "u", To: id, Relation: oam.SimpleRelation{Label: "related_to"}}}}}
	}
	var wg sync.WaitGroup
	errs := make(chan error, 40)
	for i := range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.IngestObservation(ctx, ref(i, storetest.T0)); err != nil {
				errs <- err
			}
		}()
		if i == 15 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, err := s.IngestObservation(ctx, graphstore.IngestRequest{TenantID: tenant, ProgramID: prog, Observation: oam.Observation{
					ObservationID: "creates", Tool: oam.Tool{Name: "dnsx"}, ObservedAt: storetest.T0, Confidence: 0.9,
					Entities: []oam.ObservedEntity{{Ref: "a", Asset: target}}}})
				if err != nil {
					errs <- err
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var left, edges int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.pending_observations`).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.edges`).Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if left != 0 || edges != 30 {
		t.Fatalf("after the race: %d pending rows, %d edges; want 0 and 30", left, edges)
	}

	// Bound and expiry, on ids nobody creates.
	other := storetest.EntityIDIn(tenant, prog, oam.FQDN{Name: "never.acme.com"})
	small := pg.New(pool, pg.Config{Config: graphstore.Config{Clock: clock, MaxPending: 1, PendingTTL: time.Hour}})
	req := func(i int, at time.Time) graphstore.IngestRequest {
		r := ref(100+i, at)
		r.Observation.Edges[0].To = other
		return r
	}
	if res, err := small.IngestObservation(ctx, req(0, storetest.T0)); err != nil || !res.Pending {
		t.Fatalf("first pending: %+v %v", res, err)
	}
	if _, err := small.IngestObservation(ctx, req(1, storetest.T0)); core.KindOf(err) != core.KindResourceExhausted {
		t.Fatalf("over the bound: %v, want RESOURCE_EXHAUSTED", err)
	}
	clock.Set(storetest.T0.Add(2 * time.Hour))
	res, err := small.IngestObservation(ctx, req(2, storetest.T0.Add(2*time.Hour)))
	if err != nil || !res.Pending || res.Expired != 1 {
		t.Fatalf("after the TTL: %+v %v, want pending with 1 expired", res, err)
	}
}
