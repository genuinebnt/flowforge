package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	"github.com/genuinebnt/flowforge/services/toolrunner"
)

const echoSource = `workflow echo(x: String) -> String { mock(x) }`

func TestEngineService_Memory(t *testing.T)   { runEngine(t, "") }
func TestEngineService_Postgres(t *testing.T) { runEngine(t, servicetest.FreshDB(t)) }

func runEngine(t *testing.T, dsn string) {
	st, a, b := servicetest.Stack(t, devstack.Options{DBDSN: dsn})
	tools, err := st.ToolRunner.ListTools(a, &pb.ListToolsRequest{})
	// The dev stack registers the default tools and the content tools.
	content, cerr := toolrunner.ContentAdapters(toolrunner.ContentConfig{})
	if err != nil || cerr != nil || len(tools.GetTools()) != len(toolrunner.DefaultAdapters(toolrunner.HTTPProbe{}))+len(content) {
		t.Fatalf("tools %v %v %v", tools, err, cerr)
	}
	for _, tl := range tools.GetTools() {
		if tl.GetSpecSha256() == "" {
			t.Fatalf("tool %s has no spec_sha256", tl.GetName())
		}
	}

	// Source errors come back as DSL_DIAGNOSTICS with the diagnostics.
	_, err = st.Engine.RegisterWorkflow(a, &pb.RegisterWorkflowRequest{Name: "bad", Source: `workflow bad() -> Int { nope() }`})
	servicetest.IsKind(t, err, core.KindInvalidArgument, core.ReasonDSLDiagnostics)
	if ce := err.(*core.Error); len(ce.Details) == 0 {
		t.Fatal("no diagnostics in the error details")
	}
	chk, err := st.Engine.CheckWorkflow(a, &pb.CheckWorkflowRequest{Source: echoSource})
	if err != nil || !chk.GetOk() {
		t.Fatalf("check %v %v", chk, err)
	}
	if _, err := st.Engine.RegisterWorkflow(a, &pb.RegisterWorkflowRequest{Name: "echo", Source: echoSource}); err != nil {
		t.Fatal(err)
	}
	prog, err := st.Scope.CreateProgram(a, &pb.CreateProgramRequest{Name: "p", InitialScope: servicetest.ScopeDoc("example")})
	if err != nil {
		t.Fatal(err)
	}
	in, _ := structpb.NewStruct(map[string]any{"x": "hello.example"})
	run, err := st.Engine.SubmitRun(platform.WithIdempotencyKey(a, "k1"), &pb.SubmitRunRequest{WorkflowName: "echo", ProgramId: prog.GetId(), Input: in})
	if err != nil {
		t.Fatal(err)
	}
	// Same key, same payload: the same run. Same key, other payload: 409.
	again, err := st.Engine.SubmitRun(platform.WithIdempotencyKey(a, "k1"), &pb.SubmitRunRequest{WorkflowName: "echo", ProgramId: prog.GetId(), Input: in})
	if err != nil || again.GetId() != run.GetId() {
		t.Fatalf("idempotent resubmit: %v %v", again, err)
	}
	other, _ := structpb.NewStruct(map[string]any{"x": "other"})
	_, err = st.Engine.SubmitRun(platform.WithIdempotencyKey(a, "k1"), &pb.SubmitRunRequest{WorkflowName: "echo", ProgramId: prog.GetId(), Input: other})
	servicetest.IsKind(t, err, core.KindAlreadyExists, core.ReasonIdempotencyKeyReused)

	// WatchRun streams to the end and is resumable by after_seq.
	collect := func(after int64) []*pb.RunEvent {
		ctx, cancel := context.WithTimeout(a, 20*time.Second)
		defer cancel()
		s, err := st.Engine.WatchRun(ctx, &pb.WatchRunRequest{RunId: run.GetId(), AfterSeq: after})
		if err != nil {
			t.Fatal(err)
		}
		var out []*pb.RunEvent
		for {
			ev, err := s.Recv()
			if err != nil {
				t.Fatalf("watch: %v (after %d events)", err, len(out))
			}
			out = append(out, ev)
			if ev.GetType() == "run_succeeded" || ev.GetType() == "run_failed" {
				return out
			}
		}
	}
	all := collect(0)
	if all[0].GetSeq() != 1 || all[len(all)-1].GetType() != "run_succeeded" {
		t.Fatalf("events %v", all)
	}
	rest := collect(2)
	if rest[0].GetSeq() != 3 || len(rest) != len(all)-2 {
		t.Fatalf("resume after 2: first %d, %d events", rest[0].GetSeq(), len(rest))
	}
	r, err := st.Engine.GetRun(a, &pb.GetRunRequest{RunId: run.GetId()})
	if err != nil || r.GetOutput().GetStringValue() != "hello.example" {
		t.Fatalf("run %v %v", r, err)
	}
	// The mock observation reached the graph.
	if evs, err := st.Engine.ListRunEvents(a, &pb.ListRunEventsRequest{RunId: run.GetId(), Types: []string{"observation_ingested"}}); err != nil || len(evs.GetEvents()) != 1 {
		t.Fatalf("observation_ingested: %v %v", evs, err)
	}

	// Tenant isolation on every read and write.
	_, err = st.Engine.GetRun(b, &pb.GetRunRequest{RunId: run.GetId()})
	servicetest.IsKind(t, err, core.KindNotFound, core.ReasonNotFound)
	_, err = st.Engine.CancelRun(b, &pb.CancelRunRequest{RunId: run.GetId()})
	servicetest.IsKind(t, err, core.KindNotFound, "")
	_, err = st.Engine.ListRunEvents(b, &pb.ListRunEventsRequest{RunId: run.GetId()})
	servicetest.IsKind(t, err, core.KindNotFound, "")
	_, err = st.Engine.GetWorkflowVersion(b, &pb.GetWorkflowVersionRequest{Name: "echo"})
	servicetest.IsKind(t, err, core.KindNotFound, "")
	if ws, err := st.Engine.ListWorkflows(b, &pb.ListWorkflowsRequest{}); err != nil || len(ws.GetLatest()) != 0 {
		t.Fatalf("B lists workflows %v %v", ws, err)
	}
	if rs, err := st.Engine.ListRuns(b, &pb.ListRunsRequest{}); err != nil || len(rs.GetRuns()) != 0 {
		t.Fatalf("B lists runs %v %v", rs, err)
	}
	if rs, err := st.Engine.ListRuns(a, &pb.ListRunsRequest{}); err != nil || len(rs.GetRuns()) != 1 {
		t.Fatalf("A lists runs %v %v", rs, err)
	}
	if as, err := st.Engine.ListApprovals(b, &pb.ListApprovalsRequest{}); err != nil || len(as.GetApprovals()) != 0 {
		t.Fatalf("approvals %v %v", as, err)
	}
	ws, err := st.Engine.WatchRun(b, &pb.WatchRunRequest{RunId: run.GetId()})
	if err == nil {
		_, err = ws.Recv()
	}
	servicetest.IsKind(t, err, core.KindNotFound, "")

	// Cloud Scheduler firing: idempotent per (schedule, fire time).
	fire := func() string {
		tok, err := st.Tokens.Token(a, platform.Identity{Tenant: servicetest.TenantA, Actor: "service:scheduler", Role: auth.RoleAnalyst})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]any{"workflow_name": "echo", "input": map[string]any{"x": "s"}, "fire_time": "2026-11-12T14:00:00Z"})
		req, _ := http.NewRequest(http.MethodPost, "http://"+st.Addrs[devstack.EngineName]+"/internal/schedules/nightly:fire", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("fire: %d %s", resp.StatusCode, b)
		}
		var out map[string]string
		_ = json.Unmarshal(b, &out)
		return out["run_id"]
	}
	if r1, r2 := fire(), fire(); r1 == "" || r1 != r2 {
		t.Fatalf("schedule runs %q %q", r1, r2)
	}
	resp, _ := http.Post("http://"+st.Addrs[devstack.EngineName]+"/internal/schedules/nightly:fire", "application/json", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated fire: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestEngineService_ScopeEvents: scope.events reach the engine through the
// bus; an engaged program kill switch cancels the program's runs.
func TestEngineService_ScopeEvents(t *testing.T) {
	st, a, _ := servicetest.Stack(t, devstack.Options{})
	prog, err := st.Scope.CreateProgram(a, &pb.CreateProgramRequest{Name: "k", InitialScope: servicetest.ScopeDoc("example")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Engine.RegisterWorkflow(a, &pb.RegisterWorkflowRequest{Name: "nap", Source: `workflow nap(x: String) -> String { sleep(1h); mock(x) }`}); err != nil {
		t.Fatal(err)
	}
	in, _ := structpb.NewStruct(map[string]any{"x": "a"})
	run, err := st.Engine.SubmitRun(platform.WithIdempotencyKey(a, "nap-1"), &pb.SubmitRunRequest{WorkflowName: "nap", ProgramId: prog.GetId(), Input: in})
	if err != nil {
		t.Fatal(err)
	}
	servicetest.Eventually(t, 10*time.Second, "run waiting", func() bool {
		r, _ := st.Engine.GetRun(a, &pb.GetRunRequest{RunId: run.GetId()})
		return r.GetStatus() == pb.RunStatus_RUN_STATUS_WAITING
	})
	if _, err := st.Scope.SetKillSwitch(a, &pb.SetKillSwitchRequest{Level: "program", TargetId: prog.GetId(), Enabled: true, Reason: "incident"}); err != nil {
		t.Fatal(err)
	}
	servicetest.Eventually(t, 15*time.Second, "run cancelled by the kill switch", func() bool {
		r, _ := st.Engine.GetRun(a, &pb.GetRunRequest{RunId: run.GetId()})
		return r.GetStatus() == pb.RunStatus_RUN_STATUS_CANCELLED
	})
}

// TestRegress_G_SEC_02_OperatorGlobalSwitchCancelsRuns: the global kill
// switch is the platform operator's (G-SEC-02). Its scope.events message
// carries the operator's nil tenant, and the engine still cancels the runs
// of every tenant.
func TestRegress_G_SEC_02_OperatorGlobalSwitchCancelsRuns(t *testing.T) {
	st, a, _ := servicetest.Stack(t, devstack.Options{})
	prog, err := st.Scope.CreateProgram(a, &pb.CreateProgramRequest{Name: "g", InitialScope: servicetest.ScopeDoc("example")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Engine.RegisterWorkflow(a, &pb.RegisterWorkflowRequest{Name: "nap", Source: `workflow nap(x: String) -> String { sleep(1h); mock(x) }`}); err != nil {
		t.Fatal(err)
	}
	in, _ := structpb.NewStruct(map[string]any{"x": "a"})
	run, err := st.Engine.SubmitRun(platform.WithIdempotencyKey(a, "nap-g"), &pb.SubmitRunRequest{WorkflowName: "nap", ProgramId: prog.GetId(), Input: in})
	if err != nil {
		t.Fatal(err)
	}
	servicetest.Eventually(t, 10*time.Second, "run waiting", func() bool {
		r, _ := st.Engine.GetRun(a, &pb.GetRunRequest{RunId: run.GetId()})
		return r.GetStatus() == pb.RunStatus_RUN_STATUS_WAITING
	})
	op := platform.WithIdentity(context.Background(), platform.Operator())
	if _, err := st.Scope.SetKillSwitch(op, &pb.SetKillSwitchRequest{Level: "global", Enabled: true, Reason: "abuse"}); err != nil {
		t.Fatal(err)
	}
	servicetest.Eventually(t, 15*time.Second, "run cancelled by the global kill switch", func() bool {
		r, _ := st.Engine.GetRun(a, &pb.GetRunRequest{RunId: run.GetId()})
		return r.GetStatus() == pb.RunStatus_RUN_STATUS_CANCELLED
	})
}
