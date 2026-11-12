package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/enginetest"
	"github.com/genuinebnt/flowforge/eventstore"
)

func TestEngFencedWorkerStops(t *testing.T) {
	h := enginetest.New(t)
	var calls atomic.Int32
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	h.Exec.Handle("block", func(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
		if calls.Add(1) == 1 {
			close(firstStarted)
			<-releaseFirst // the paused worker (GC pause, VM freeze)
		}
		return engine.ToolResult{Output: call.Input}, nil
	})
	h.Exec.Handle("post", enginetest.Output("id"))
	h.Exec.Handle("undo", enginetest.Output(nil))
	c := enginetest.Analyst(enginetest.TenantA)
	h.Register(c, "saga", enginetest.Saga(enginetest.SagaOptions{Steps: 1, Block: true}), core.Nil)
	r := h.Submit(c, "saga", core.Nil, map[string]any{"amount": 1}, "k")

	cfg := h.Config
	cfg.RenewEvery = time.Hour // the paused worker never renews
	stale := enginetest.NewCrashStore(h.Store, -1)
	ea, err := engine.New(h.Deps(stale), cfg)
	if err != nil {
		t.Fatal(err)
	}
	a := ea.NewWorker("paused")
	done := make(chan error, 1)
	go func() { _, err := a.RunOnce(h.Ctx()); done <- err }()
	<-firstStarted
	appendsBefore := stale.Appends()

	h.Clock.Advance(cfg.LeaseTTL + time.Second)
	b := h.Engine.NewWorker("successor")
	if st := h.RunToEnd(b, r.ID); st != engine.StatusSucceeded {
		t.Fatalf("successor: %s", st)
	}
	final := h.Events(r.ID)
	close(releaseFirst)
	if err := <-done; !errors.Is(err, engine.ErrLeaseLost) {
		t.Fatalf("paused worker returned %v, want ErrLeaseLost", err)
	}
	if stale.Appends() != appendsBefore || len(h.Events(r.ID)) != len(final) {
		t.Fatal("the stale worker committed an event after losing its lease")
	}
	if countType(final, engine.EvEffectCompleted, "1") != 1 {
		t.Fatal("block result not recorded exactly once")
	}
}

func TestEngParMapOrdering(t *testing.T) {
	h := enginetest.New(t)
	const n, limit = 5, 3
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	started := make(chan int, n)
	release := make([]chan struct{}, n+1)
	for i := range release {
		release[i] = make(chan struct{})
	}
	h.Exec.Handle("block", func(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
		var x int
		_ = json.Unmarshal(call.Input, &x)
		mu.Lock()
		inFlight++
		maxInFlight = max(maxInFlight, inFlight)
		mu.Unlock()
		started <- x
		<-release[x]
		mu.Lock()
		inFlight--
		mu.Unlock()
		return engine.ToolResult{Output: mustJSON(x * 10)}, nil
	})
	c := enginetest.Analyst(enginetest.TenantA)
	h.Register(c, "fan", enginetest.ParMapEcho(limit, false), core.Nil)
	r := h.Submit(c, "fan", core.Nil, map[string]any{"xs": []int{1, 2, 3, 4, 5}}, "k")
	w := h.Engine.NewWorker("w")
	done := make(chan error, 1)
	go func() { _, err := w.RunOnce(h.Ctx()); done <- err }()
	// Release each batch in reverse order of starting.
	for _, batch := range []int{3, 2} {
		var xs []int
		for range batch {
			xs = append(xs, <-started)
		}
		slices.Sort(xs)
		for i := len(xs) - 1; i >= 0; i-- {
			close(release[xs[i]])
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ := h.Engine.GetRun(h.Ctx(), c, r.ID)
	if got.Status != engine.StatusSucceeded || string(got.Output) != "[10,20,30,40,50]" {
		t.Fatalf("run %s output %s", got.Status, got.Output)
	}
	if maxInFlight != limit {
		t.Fatalf("max concurrency %d, want %d", maxInFlight, limit)
	}
	var paths []string
	for _, e := range h.Events(r.ID) {
		if e.Type == engine.EvEffectCompleted {
			var p engine.EffectCompletedPayload
			_ = json.Unmarshal(e.Payload, &p)
			paths = append(paths, p.Path)
		}
	}
	if !slices.Equal(paths, []string{"0/0/0", "0/1/0", "0/2/0", "0/3/0", "0/4/0", "0"}) {
		t.Fatalf("results recorded in order %v", paths)
	}

	t.Run("collect_and_fail", func(t *testing.T) {
		for _, collect := range []bool{true, false} {
			h := enginetest.New(t)
			h.Exec.Handle("block", func(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
				if string(call.Input) == "2" {
					return engine.ToolResult{}, &engine.ToolError{Code: "bad_input", Message: "no"}
				}
				return engine.ToolResult{Output: call.Input}, nil
			})
			h.Register(c, "fan", enginetest.ParMapEcho(2, collect), core.Nil)
			r := h.Submit(c, "fan", core.Nil, map[string]any{"xs": []int{1, 2, 3, 4}}, "k")
			st := h.RunToEnd(h.Engine.NewWorker("w"), r.ID)
			got, _ := h.Engine.GetRun(h.Ctx(), c, r.ID)
			if collect {
				var out []map[string]any
				_ = json.Unmarshal(got.Output, &out)
				if st != engine.StatusSucceeded || len(out) != 4 || out[1]["err"] == nil || out[2]["ok"] != 3.0 {
					t.Fatalf("collect: %s %s", st, got.Output)
				}
			} else if st != engine.StatusFailed || len(h.Exec.CallsTo("block")) > 3 {
				t.Fatalf("fail: %s after %d calls", st, len(h.Exec.CallsTo("block")))
			}
		}
	})
}

func TestEngNondeterminismDetected(t *testing.T) {
	h := enginetest.New(t)
	h.Exec.Handle("echo", enginetest.Echo())
	c := enginetest.Analyst(enginetest.TenantA)
	h.Register(c, "chain", enginetest.Chain(3), core.Nil)
	r := h.Submit(c, "chain", core.Nil, map[string]any{}, "k")
	h.RunToEnd(h.Engine.NewWorker("w"), r.ID)
	evs := h.Events(r.ID)
	// Copy the log without its terminal event into a fresh store and
	// tamper with the recorded arguments of effect 1.
	tampered := make([]eventstore.Event, 0, len(evs)-1)
	for _, e := range evs[:len(evs)-1] {
		if e.Type == engine.EvEffectScheduled {
			var p engine.EffectScheduledPayload
			_ = json.Unmarshal(e.Payload, &p)
			if p.Path == "1" {
				p.ArgsHash = "00" + p.ArgsHash[2:]
				e.Payload = mustJSON(p)
			}
		}
		tampered = append(tampered, e)
	}
	rec, err := h.Repo.GetRun(h.Ctx(), core.Nil, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	store := eventstore.NewMemStore(h.Clock)
	if err := store.CreateRun(h.Ctx(), eventstore.RunSpec{ID: r.ID, Tenant: rec.Tenant, WorkflowVersion: rec.WorkflowVersionID,
		CreatedBy: rec.CreatedBy, IdempotencyKey: rec.IdempotencyKey, RequestHash: rec.RequestHash}, tampered); err != nil {
		t.Fatal(err)
	}
	e2 := h.NewEngine(store)
	if _, err := e2.NewWorker("w2").RunOnce(h.Ctx()); err != nil {
		t.Fatal(err)
	}
	got, err := e2.GetRun(h.Ctx(), c, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != engine.StatusFailed || got.FailureCode != "nondeterminism_detected" {
		t.Fatalf("run %s %s", got.Status, got.FailureCode)
	}
}

func TestEngRegistrationRejectsAgentEffects(t *testing.T) {
	h := enginetest.New(t)
	c := enginetest.Analyst(enginetest.TenantA)
	h.Compiler.Add("agent", enginetest.Agent)
	_, err := h.Engine.RegisterWorkflow(h.Ctx(), c, engine.RegisterWorkflowRequest{Name: "triage", Source: "agent"})
	wantReason(t, err, core.KindInvalidArgument, core.ReasonDSLDiagnostics)
	var ce *core.Error
	errors.As(err, &ce)
	if len(ce.Details) == 0 || ce.Details[len(ce.Details)-1].(engine.Diagnostic).Code != engine.DiagAgentEffectsDisabled {
		t.Fatalf("details %+v", ce.Details)
	}
	res, err := h.Engine.CheckWorkflow(h.Ctx(), c, engine.CheckWorkflowRequest{Name: "triage", Source: "agent"})
	if err != nil || res.OK || res.Diagnostics[len(res.Diagnostics)-1].Code != engine.DiagAgentEffectsDisabled {
		t.Fatalf("check %+v %v", res, err)
	}
	// Ordinary diagnostics are reported too.
	_, err = h.Engine.RegisterWorkflow(h.Ctx(), c, engine.RegisterWorkflowRequest{Name: "bad", Source: "does not parse"})
	wantReason(t, err, core.KindInvalidArgument, core.ReasonDSLDiagnostics)
	// The policy check: an active workflow against a passive program.
	passive := core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000a9")
	if _, err := h.Scope.PutScope(enginetest.TenantA, passive, enginetest.DocAllowing(core.ClassPassive, "acme.com")); err != nil {
		t.Fatal(err)
	}
	h.Compiler.Add("recon", enginetest.Recon)
	_, err = h.Engine.RegisterWorkflow(h.Ctx(), c, engine.RegisterWorkflowRequest{Name: "recon", Source: "recon", ProgramID: passive})
	wantReason(t, err, core.KindInvalidArgument, core.ReasonDSLDiagnostics)
}

func TestEngTenantIsolation(t *testing.T) {
	h := enginetest.New(t)
	h.Exec.Handle("post", enginetest.Output("id"))
	h.Exec.Handle("block", enginetest.Echo())
	a := enginetest.Analyst(enginetest.TenantA)
	b := enginetest.Approver(enginetest.TenantB)
	b.Roles = append(b.Roles, "analyst")
	h.Register(a, "saga", enginetest.Saga(enginetest.SagaOptions{Approval: true, Steps: 1}), core.Nil)
	r := h.Submit(a, "saga", core.Nil, map[string]any{"amount": 1}, "k")
	h.Drain(h.Engine.NewWorker("w"))
	ap := pendingApproval(t, h, enginetest.Approver(enginetest.TenantA), r.ID)
	ctx := h.Ctx()
	notFound := func(what string, err error) {
		t.Helper()
		var ce *core.Error
		if !errors.As(err, &ce) || ce.Kind != core.KindNotFound {
			t.Errorf("%s across tenants: %v, want NOT_FOUND", what, err)
		}
	}
	_, err := h.Engine.GetRun(ctx, b, r.ID)
	notFound("GetRun", err)
	_, _, err = h.Engine.ListRunEvents(ctx, b, r.ID, 0, nil, engine.Page{})
	notFound("ListRunEvents", err)
	_, err = h.Engine.WatchRun(ctx, b, r.ID, 0)
	notFound("WatchRun", err)
	_, err = h.Engine.CancelRun(ctx, b, r.ID, "")
	notFound("CancelRun", err)
	_, err = h.Engine.ResolveUnknownEffect(ctx, b, r.ID, "0", engine.ResolveRetry, nil)
	notFound("ResolveUnknownEffect", err)
	_, err = h.Engine.GetWorkflowVersion(ctx, b, "saga", 0)
	notFound("GetWorkflowVersion", err)
	_, err = h.Engine.DeprecateWorkflowVersion(ctx, b, "saga", 1)
	notFound("DeprecateWorkflowVersion", err)
	_, err = h.Engine.GetApproval(ctx, b, ap.ID)
	notFound("GetApproval", err)
	_, err = h.Engine.DecideApproval(ctx, b, ap.ID, engine.DecisionApprove, "")
	notFound("DecideApproval", err)
	_, err = h.SubmitErr(b, "saga", core.Nil, map[string]any{"amount": 1}, "k2", 0)
	notFound("SubmitRun of another tenant's workflow", err)
	_, err = h.Engine.CheckWorkflow(ctx, b, engine.CheckWorkflowRequest{Source: "x", ProgramID: enginetest.ProgramA})
	notFound("CheckWorkflow with another tenant's program", err)
	if runs, _, _ := h.Engine.ListRuns(ctx, b, engine.RunFilter{}, engine.Page{}); len(runs) != 0 {
		t.Error("ListRuns shows another tenant's runs")
	}
	if as, _, _ := h.Engine.ListApprovals(ctx, b, engine.ApprovalFilter{}, engine.Page{}); len(as) != 0 {
		t.Error("ListApprovals shows another tenant's approvals")
	}
	if ws, _, _ := h.Engine.ListWorkflows(ctx, b, engine.Page{}); len(ws) != 0 {
		t.Error("ListWorkflows shows another tenant's workflows")
	}
	if ws, _, _ := h.Engine.ListWorkflows(ctx, a, engine.Page{}); len(ws) != 1 {
		t.Error("ListWorkflows misses the tenant's workflow")
	}
}

func TestEngScopeChange(t *testing.T) {
	h, r := multiHarness(t)
	c := enginetest.Analyst(enginetest.TenantA)
	w := h.Engine.NewWorker("w")
	h.Drain(w) // waits on sleep(1s)
	// A version that still allows the target: the run continues on it.
	v2, _ := h.Scope.PutScope(enginetest.TenantA, enginetest.ProgramA, enginetest.DocAllowing(core.ClassIntrusive, "acme.com"))
	if err := h.Engine.HandleScopeChanged(h.Ctx(), enginetest.TenantA, enginetest.ProgramA, v2); err != nil {
		t.Fatal(err)
	}
	h.Drain(w)
	got, _ := h.Engine.GetRun(h.Ctx(), c, r.ID)
	if got.ScopeVersion != v2 || got.Status != engine.StatusWaiting {
		t.Fatalf("after v2: version %d status %s", got.ScopeVersion, got.Status)
	}
	// A version that no longer allows it: the run pauses.
	v3, _ := h.Scope.PutScope(enginetest.TenantA, enginetest.ProgramA, enginetest.DocAllowing(core.ClassIntrusive, "other.com"))
	if err := h.Engine.HandleScopeChanged(h.Ctx(), enginetest.TenantA, enginetest.ProgramA, v3); err != nil {
		t.Fatal(err)
	}
	if st := h.RunToEnd(w, r.ID); st != engine.StatusPaused {
		t.Fatalf("after v3: %s", st)
	}
	// Restoring the allowance resumes it.
	v4, _ := h.Scope.PutScope(enginetest.TenantA, enginetest.ProgramA, enginetest.DocAllowing(core.ClassIntrusive, "acme.com"))
	if err := h.Engine.HandleScopeChanged(h.Ctx(), enginetest.TenantA, enginetest.ProgramA, v4); err != nil {
		t.Fatal(err)
	}
	if st := h.RunToEnd(w, r.ID); st != engine.StatusSucceeded {
		t.Fatalf("after v4: %s", st)
	}
	types := enginetest.Types(h.Events(r.ID))
	if !containsSeq(types, []string{engine.EvScopeChanged, engine.EvScopeChanged, engine.EvRunPaused, engine.EvScopeChanged, engine.EvRunResumed, engine.EvRunSucceeded}) {
		t.Fatalf("events %v", types)
	}
}

func TestEngScopeUnavailableDenies(t *testing.T) {
	h, r := multiHarness(t)
	h.Scope.Unavailable = true
	if st := h.RunToEnd(h.Engine.NewWorker("w"), r.ID); st != engine.StatusFailed {
		t.Fatalf("status %s", st)
	}
	if len(h.Exec.CallsTo("httpx")) != 0 {
		t.Fatal("tool called without a scope decision")
	}
}

func TestEngEventsAPI(t *testing.T) {
	h := enginetest.New(t)
	h.Exec.Handle("echo", enginetest.Echo())
	c := enginetest.Analyst(enginetest.TenantA)
	h.Register(c, "chain", enginetest.Chain(3), core.Nil)
	r := h.Submit(c, "chain", core.Nil, map[string]any{}, "k")
	h.RunToEnd(h.Engine.NewWorker("w"), r.ID)
	all := h.Events(r.ID)
	var seen []uint64
	tok := ""
	for {
		page, next, err := h.Engine.ListRunEvents(h.Ctx(), c, r.ID, 0, nil, engine.Page{Size: 4, Token: tok})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page {
			seen = append(seen, e.Seq)
		}
		if next == "" {
			break
		}
		tok = next
	}
	if len(seen) != len(all) || seen[0] != 1 || seen[len(seen)-1] != uint64(len(all)) {
		t.Fatalf("paged seqs %v of %d", seen, len(all))
	}
	only, _, _ := h.Engine.ListRunEvents(h.Ctx(), c, r.ID, 0, []string{engine.EvEffectCompleted}, engine.Page{})
	if len(only) != 3 {
		t.Fatalf("filtered %d", len(only))
	}
	ctx, cancel := context.WithCancel(h.Ctx())
	defer cancel()
	stream, err := h.Engine.WatchRun(ctx, c, r.ID, 5)
	if err != nil {
		t.Fatal(err)
	}
	var watched []uint64
	for e, err := range stream {
		if err != nil {
			break
		}
		watched = append(watched, e.Seq)
		if e.Seq == uint64(len(all)) {
			cancel()
		}
	}
	if len(watched) != len(all)-5 || watched[0] != 6 {
		t.Fatalf("watched %v", watched)
	}
	runs, next, _ := h.Engine.ListRuns(h.Ctx(), c, engine.RunFilter{Status: engine.StatusSucceeded}, engine.Page{Size: 1})
	if len(runs) != 1 || next != "" || runs[0].Status != engine.StatusSucceeded {
		t.Fatalf("ListRuns %+v %q", runs, next)
	}
	effs, _ := h.Repo.ListEffects(h.Ctx(), r.ID)
	if len(effs) != 3 || effs[2].State != engine.EffectStateCompleted {
		t.Fatalf("effects projection %+v", effs)
	}
}

func TestEngWorkerRunLoop(t *testing.T) {
	h := enginetest.New(t)
	h.Exec.Handle("echo", enginetest.Echo())
	c := enginetest.Analyst(enginetest.TenantA)
	h.Register(c, "chain", enginetest.Chain(2), core.Nil)
	ctx, cancel := context.WithCancel(h.Ctx())
	w := h.Engine.NewWorker("loop")
	stopped := make(chan error, 1)
	go func() { stopped <- w.Run(ctx) }()
	var ids []core.UUID
	for i := range 5 {
		ids = append(ids, h.Submit(c, "chain", core.Nil, map[string]any{}, string(rune('a'+i))).ID)
	}
	deadline := time.After(10 * time.Second)
	for _, id := range ids {
		for {
			info, _ := h.Store.GetRun(h.Ctx(), id)
			if info.Status == engine.StatusSucceeded {
				break
			}
			select {
			case <-deadline:
				t.Fatalf("run %s still %s", id, info.Status)
			case <-time.After(time.Millisecond):
			}
		}
	}
	cancel()
	if err := <-stopped; err != nil {
		t.Fatal(err)
	}
}
