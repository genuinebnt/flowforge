package engine_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/devstack"
	enginesvc "github.com/genuinebnt/flowforge/services/engine"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	"github.com/genuinebnt/flowforge/testkit"
)

// monitoringDefault reads the default of a variable of
// infra/modules/monitoring, so the names the services emit and the names
// the alerts and log metrics filter on cannot drift apart.
func monitoringDefault(t *testing.T, variable string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testkit.RepoRoot(t), "infra", "modules", "monitoring", "variables.tf"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?s)variable "` + regexp.QuoteMeta(variable) + `" \{.*?default\s*=\s*"([^"]*)"`)
	m := re.FindSubmatch(b)
	if m == nil {
		t.Fatalf("variable %s has no string default in infra/modules/monitoring/variables.tf", variable)
	}
	return string(m[1])
}

// TestRegress_G_OPS_04_SLOMetrics runs a workflow through the dev stack
// and scrapes /metrics: the scope check latency and the run
// submit-to-start histograms exist under the names the monitoring
// module's SLO alerts query.
func TestRegress_G_OPS_04_SLOMetrics(t *testing.T) {
	st, a, _ := servicetest.Stack(t, devstack.Options{})
	if _, err := st.Engine.RegisterWorkflow(a, &pb.RegisterWorkflowRequest{Name: "echo", Source: echoSource}); err != nil {
		t.Fatal(err)
	}
	prog, err := st.Scope.CreateProgram(a, &pb.CreateProgramRequest{Name: "p", InitialScope: servicetest.ScopeDoc("example")})
	if err != nil {
		t.Fatal(err)
	}
	in, _ := structpb.NewStruct(map[string]any{"x": "hello.example"})
	run, err := st.Engine.SubmitRun(platform.WithIdempotencyKey(a, "slo"), &pb.SubmitRunRequest{WorkflowName: "echo", ProgramId: prog.GetId(), Input: in})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(a, 20*time.Second)
	defer cancel()
	ws, err := st.Engine.WatchRun(ctx, &pb.WatchRunRequest{RunId: run.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	for {
		ev, err := ws.Recv()
		if err != nil {
			t.Fatal(err)
		}
		if ev.GetType() == "run_succeeded" {
			break
		}
	}
	// A direct Check, as the tool runner and egress proxy make them.
	if _, err := st.Scope.Check(a, &pb.CheckRequest{ProgramId: prog.GetId(), Target: "a.example", Kind: pb.TargetKind_TARGET_KIND_HOST, ActivityClass: pb.ActivityClass_ACTIVITY_CLASS_PASSIVE}); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + st.Addrs[devstack.EngineName] + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	text := string(body)
	for _, name := range []string{monitoringDefault(t, "metric_scope_check_seconds"), monitoringDefault(t, "metric_submit_to_start_seconds")} {
		// Unlabelled histograms: "<name>_bucket{le=...}", "<name>_count N".
		if !strings.Contains(text, "# TYPE "+name+" histogram") || !strings.Contains(text, name+`_bucket{le="+Inf"}`) {
			t.Errorf("/metrics has no histogram %s:\n%s", name, text)
		}
		if strings.Contains(text, name+"_count 0\n") {
			t.Errorf("%s was never observed", name)
		}
	}
}

type nopPublisher struct{}

func (nopPublisher) Publish(context.Context, []engine.Envelope) error { return nil }

// TestRegress_G_OPS_04_ApprovalLogLine pins the log line the ff_approvals
// log-based metric counts: msg equal to the monitoring module's
// approval_msg, service "engine", and the status and role labels it
// extracts.
func TestRegress_G_OPS_04_ApprovalLogLine(t *testing.T) {
	var buf bytes.Buffer
	pub := enginesvc.MeteredPublisher{Next: nopPublisher{}, Metrics: platform.NewMetrics(nil),
		Logger: platform.NewLogger(&buf, "engine", "dev", slog.LevelInfo)}
	if enginesvc.ApprovalLogMsg != monitoringDefault(t, "approval_msg") {
		t.Fatalf("ApprovalLogMsg %q differs from infra/modules/monitoring approval_msg", enginesvc.ApprovalLogMsg)
	}
	env := func(status string) engine.Envelope {
		p, _ := json.Marshal(map[string]any{"approval_id": "apr_1", "run_id": "run_1", "role": "finance_ap", "status": status})
		return engine.Envelope{EventID: core.Nil, Topic: engine.TopicApprovalEvents, Type: "approval." + status, Payload: p}
	}
	if err := pub.Publish(context.Background(), []engine.Envelope{env(engine.ApprovalPending), env(engine.ApprovalDenied),
		{Topic: engine.TopicRunEvents, Type: "run.run_started", Payload: json.RawMessage(`{}`)}}); err != nil {
		t.Fatal(err)
	}
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("%q: %v", l, err)
		}
		lines = append(lines, m)
	}
	if len(lines) != 2 {
		t.Fatalf("want one line per approval change, got %d: %s", len(lines), buf.String())
	}
	for i, want := range []string{"requested", "rejected"} {
		l := lines[i]
		if l["msg"] != enginesvc.ApprovalLogMsg || l["service"] != "engine" || l["env"] != "dev" || l["status"] != want || l["role"] != "finance_ap" {
			t.Errorf("line %d = %v, want msg %q service engine status %q role finance_ap", i, l, enginesvc.ApprovalLogMsg, want)
		}
	}
}
