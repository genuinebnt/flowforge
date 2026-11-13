package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/devstack"
	enginesvc "github.com/genuinebnt/flowforge/services/engine"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	"github.com/genuinebnt/flowforge/testkit"
)

func TestCron(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	for _, c := range []struct{ expr, from, want string }{
		{"* * * * *", "2026-11-12T14:00:30Z", "2026-11-12T14:01:00Z"},
		{"*/15 * * * *", "2026-11-12T14:01:00Z", "2026-11-12T14:15:00Z"},
		{"0 3 * * *", "2026-11-12T14:00:00Z", "2026-11-13T03:00:00Z"},
		{"30 9 * * 1-5", "2026-11-13T10:00:00Z", "2026-11-16T09:30:00Z"}, // Friday -> Monday
		{"0 0 1 * *", "2026-11-12T00:00:00Z", "2026-12-01T00:00:00Z"},
		{"0 0 29 2 *", "2026-03-01T00:00:00Z", "2028-02-29T00:00:00Z"},
		{"0 12 13 * 5", "2026-11-12T13:00:00Z", "2026-11-13T12:00:00Z"}, // dom OR dow
		{"0 0 * * 7", "2026-11-12T00:00:00Z", "2026-11-15T00:00:00Z"},   // 7 = Sunday
	} {
		cr, err := enginesvc.ParseCron(c.expr)
		if err != nil {
			t.Fatalf("%s: %v", c.expr, err)
		}
		if got := cr.Next(at(c.from)); !got.Equal(at(c.want)) {
			t.Errorf("%s after %s = %s, want %s", c.expr, c.from, got, c.want)
		}
	}
	for _, bad := range []string{"", "* * * *", "60 * * * *", "* 24 * * *", "*/0 * * * *", "5-1 * * * *", "MON * * * *"} {
		if _, err := enginesvc.ParseCron(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	never, _ := enginesvc.ParseCron("0 0 30 2 *")
	if !never.Next(at("2026-01-01T00:00:00Z")).IsZero() {
		t.Error("30 February fired")
	}
}

func TestRegress_GOPS10_Schedules_Memory(t *testing.T)   { runSchedules(t, "") }
func TestRegress_GOPS10_Schedules_Postgres(t *testing.T) { runSchedules(t, servicetest.FreshDB(t)) }

// runSchedules: the REST-facing API on the engine (create, list, get,
// delete, tick auth) and the firing rules: a due schedule fires once per
// firing time with idempotency key schedule:<id>:<time> (a second engine
// firing the same time gets the same run), missed firings coalesce, and a
// bad request is refused.
func runSchedules(t *testing.T, dsn string) {
	st, a, _ := servicetest.Stack(t, devstack.Options{DBDSN: dsn})
	if _, err := st.Engine.RegisterWorkflow(a, &pb.RegisterWorkflowRequest{Name: "echo", Source: echoSource}); err != nil {
		t.Fatal(err)
	}
	tok, err := st.Tokens.Token(a, platform.Identity{Tenant: servicetest.TenantA, Actor: servicetest.UserA, Role: auth.RoleAnalyst})
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + st.Addrs[devstack.EngineName] + "/internal/schedules"
	call := func(method, url string, body any, auth bool) (int, []byte) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, url, rd)
		if auth {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	code, b := call(http.MethodPost, base, map[string]any{"name": "nightly", "cron": "0 3 * * *", "workflow_name": "echo", "input": map[string]any{"x": "n"}}, true)
	if code != http.StatusCreated {
		t.Fatalf("create: %d %s", code, b)
	}
	var created enginesvc.ScheduleJSON
	_ = json.Unmarshal(b, &created)
	if created.ID == "" || created.NextFireAt == "" || created.CreatedBy != servicetest.UserA {
		t.Fatalf("created %s", b)
	}
	for _, bad := range []map[string]any{
		{"name": "nightly", "cron": "0 3 * * *", "workflow_name": "echo"},  // duplicate name: 409
		{"name": "x", "cron": "61 * * * *", "workflow_name": "echo"},       // bad cron
		{"name": "y", "cron": "0 3 * * *", "workflow_name": "nope"},        // unknown workflow
		{"name": "Bad Name", "cron": "0 3 * * *", "workflow_name": "echo"}, // name
	} {
		if code, b := call(http.MethodPost, base, bad, true); code != http.StatusConflict && code != http.StatusBadRequest {
			t.Fatalf("bad create %v: %d %s", bad, code, b)
		}
	}
	if code, b := call(http.MethodGet, base, nil, true); code != http.StatusOK || !bytes.Contains(b, []byte(created.ID)) {
		t.Fatalf("list: %d %s", code, b)
	}
	if code, _ := call(http.MethodGet, base, nil, false); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list: %d", code)
	}
	if code, _ := call(http.MethodPost, base+":tick", nil, false); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated tick: %d", code)
	}
	if code, b := call(http.MethodPost, base+":tick", nil, true); code != http.StatusOK || !bytes.Contains(b, []byte(`"fired":0`)) {
		t.Fatalf("tick: %d %s", code, b)
	}
	if code, _ := call(http.MethodDelete, base+"/"+created.ID, nil, true); code != http.StatusNoContent {
		t.Fatalf("delete: %d", code)
	}
	if code, _ := call(http.MethodGet, base+"/"+created.ID, nil, true); code != http.StatusNotFound {
		t.Fatalf("get after delete: %d", code)
	}

	// Firing, with a fake clock and the stack's engine.
	clock := testkit.NewFakeClock(time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC))
	var store enginesvc.ScheduleStore = enginesvc.NewMemSchedules()
	if st.DB != nil {
		store = enginesvc.PgSchedules{DB: st.DB}
	}
	sch := enginesvc.Schedule{ID: core.MustParseUUID("0190a1b2-0000-7000-8000-0000000005c1"), Tenant: servicetest.TenantA, Name: "every-minute",
		Cron: "* * * * *", WorkflowName: "echo", Input: json.RawMessage(`{"x":"m"}`), Enabled: true, NextFireAt: clock.Now(),
		CreatedBy: servicetest.UserA, CreatedAt: clock.Now()}
	if err := store.Create(context.Background(), sch); err != nil {
		t.Fatal(err)
	}
	s := &enginesvc.Scheduler{Store: store, Engine: st.EngineService.Engine, Clock: clock, Logger: platform.DiscardLogger()}
	if n, err := s.Tick(context.Background()); err != nil || n != 1 {
		t.Fatalf("first tick fired %d (%v)", n, err)
	}
	if n, _ := s.Tick(context.Background()); n != 0 {
		t.Fatalf("second tick in the same minute fired %d", n)
	}
	got, _ := store.Get(context.Background(), servicetest.TenantA, sch.ID)
	if got.LastRunID.IsZero() || got.LastError != "" || !got.NextFireAt.Equal(clock.Now().Add(time.Minute)) {
		t.Fatalf("after firing: %+v", got)
	}
	// Another engine with its own view of the same schedule at the same
	// firing time submits the same idempotency key: the same run.
	other := enginesvc.NewMemSchedules()
	_ = other.Create(context.Background(), sch)
	s2 := &enginesvc.Scheduler{Store: other, Engine: st.EngineService.Engine, Clock: clock, Logger: platform.DiscardLogger()}
	if _, err := s2.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	dup, _ := other.Get(context.Background(), servicetest.TenantA, sch.ID)
	if dup.LastRunID != got.LastRunID {
		t.Fatalf("second engine created run %s, first %s", dup.LastRunID, got.LastRunID)
	}
	// Ten minutes down: one firing (for 14:01), next after now.
	clock.Advance(10 * time.Minute)
	if n, _ := s.Tick(context.Background()); n != 1 {
		t.Fatalf("after downtime fired %d, want 1 (coalesced)", n)
	}
	got2, _ := store.Get(context.Background(), servicetest.TenantA, sch.ID)
	if got2.LastRunID == got.LastRunID || !got2.NextFireAt.After(clock.Now()) {
		t.Fatalf("after downtime: %+v", got2)
	}
}
