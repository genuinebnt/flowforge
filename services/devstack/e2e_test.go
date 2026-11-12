package devstack_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	"github.com/genuinebnt/flowforge/services/notifier"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
	"github.com/genuinebnt/flowforge/services/toolrunner"
)

// ReconSource is the spec-like recon workflow (spec 7.1) over the bundled
// tools: dnsresolve every root, then probe the in-scope ones over HTTP.
const ReconSource = `
workflow recon(roots: List<Fqdn>) -> Int {
  let ips = roots |> flat_map(dnsresolve);
  let web = roots
    |> filter(n => scope.allows(n))
    |> par_map(n => httpprobe(scope.check(Host::Name(n))?), limit = 4)
    |> filter_map(x => x);
  len(ips) + len(web)
}
`

type target struct {
	*httptest.Server
	mu    sync.Mutex
	hosts []string
}

func (t *target) seen() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.hosts...)
}

func newTarget(t *testing.T) (*target, int) {
	tg := &target{}
	tg.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tg.mu.Lock()
		tg.hosts = append(tg.hosts, r.Host)
		tg.mu.Unlock()
		w.Header().Set("Server", "nginx/1.25.3")
		_, _ = w.Write([]byte("<html><head><title>Jenkins Login</title></head><body>hi</body></html>"))
	}))
	t.Cleanup(tg.Close)
	_, ps, _ := net.SplitHostPort(tg.Listener.Addr().String())
	port, _ := strconv.Atoi(ps)
	return tg, port
}

func TestE2E_Memory(t *testing.T) { runE2E(t, "") }

func TestE2E_Postgres(t *testing.T) { runE2E(t, servicetest.FreshDB(t)) }

func runE2E(t *testing.T, dsn string) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	tg, port := newTarget(t)
	resolver := servicetest.NewResolver(map[string]string{"app.test": "127.0.0.1", "evil.test": "127.0.0.1"})
	hook := servicetest.NewWebhook(t, "hook-secret")
	secrets := notifier.NewMapSecrets(map[string]string{"test://hook": "hook-secret"})

	st, err := devstack.Start(ctx, devstack.Options{DBDSN: dsn, Resolver: resolver, Secrets: secrets, AllowInsecureWebhooks: true,
		ProbePorts: []toolrunner.ProbePort{{Scheme: "http", Port: port}},
		Engine:     engine.Config{PollInterval: 10 * time.Millisecond, IdleBackoff: 50 * time.Millisecond}, Logger: testLogger()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	tenant := core.MustParseUUID("0190a1b2-0000-7000-8000-00000000e2e1")
	user := core.MustParseUUID("0190a1b2-0000-7000-8000-000000000001")
	ctx = st.As(ctx, tenant, "user:"+user.String(), auth.RoleAdmin)

	// A program whose scope allows app.test (and, explicitly, the loopback
	// address it resolves to, which the egress proxy otherwise refuses).
	prog, err := st.Scope.CreateProgram(ctx, &pb.CreateProgramRequest{Name: "e2e", Platform: "test", InitialScope: &pb.ScopeDocument{
		Rules: []*pb.ScopeRule{
			{Id: "app", Effect: pb.RuleEffect_RULE_EFFECT_ALLOW, Kind: pb.RuleKind_RULE_KIND_DOMAIN, Value: "app.test"},
			{Id: "lo", Effect: pb.RuleEffect_RULE_EFFECT_ALLOW, Kind: pb.RuleKind_RULE_KIND_CIDR, Value: "127.0.0.1/32"},
		},
		Activity: &pb.ActivityPolicy{MaxClass: pb.ActivityClass_ACTIVITY_CLASS_ACTIVE},
	}})
	if err != nil {
		t.Fatal(err)
	}

	// An alert rule that fires a signed webhook for every new URL.
	if _, err := st.Notifier.PutChannel(ctx, &pb.PutChannelRequest{Kind: notifier.KindWebhook, Name: "e2e", Enabled: true,
		Config: mustStruct(t, map[string]any{"url": hook.URL + "/hook", "secret_ref": "test://hook"})}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Notifier.PutRule(ctx, &pb.PutRuleRequest{Name: "new_url", Enabled: true,
		Source: `alert new_url when change.type == "entity_added" && entity.asset_type == "URL" -> webhook("e2e") severity = High`}); err != nil {
		t.Fatal(err)
	}

	wf, err := st.Engine.RegisterWorkflow(ctx, &pb.RegisterWorkflowRequest{Name: "recon", Source: ReconSource, ProgramId: prog.GetId()})
	if err != nil {
		t.Fatal(err)
	}
	if wf.GetInferredClass() != pb.ActivityClass_ACTIVITY_CLASS_ACTIVE {
		t.Fatalf("class %v", wf.GetInferredClass())
	}

	// SubmitRun requires the idempotency-key metadata.
	in := mustStruct(t, map[string]any{"roots": []any{"app.test", "evil.test"}})
	if _, err := st.Engine.SubmitRun(ctx, &pb.SubmitRunRequest{WorkflowName: "recon", ProgramId: prog.GetId(), Input: in}); core.KindOf(err) != core.KindInvalidArgument {
		t.Fatalf("submit without idempotency key: %v", err)
	}
	run, err := st.Engine.SubmitRun(platform.WithIdempotencyKey(ctx, "e2e-1"), &pb.SubmitRunRequest{WorkflowName: "recon", ProgramId: prog.GetId(), Input: in})
	if err != nil {
		t.Fatal(err)
	}
	var final *pb.Run
	servicetest.Eventually(t, 60*time.Second, "run to finish", func() bool {
		r, err := st.Engine.GetRun(ctx, &pb.GetRunRequest{RunId: run.GetId()})
		if err != nil {
			t.Fatal(err)
		}
		final = r
		return r.GetStatus() == pb.RunStatus_RUN_STATUS_SUCCEEDED || r.GetStatus() == pb.RunStatus_RUN_STATUS_FAILED
	})
	if final.GetStatus() != pb.RunStatus_RUN_STATUS_SUCCEEDED {
		evs, _ := st.Engine.ListRunEvents(ctx, &pb.ListRunEventsRequest{RunId: run.GetId(), Page: &pb.PageRequest{PageSize: 500}})
		for _, e := range evs.GetEvents() {
			b, _ := json.Marshal(e.GetPayload())
			t.Logf("%d %s %s", e.GetSeq(), e.GetType(), b)
		}
		t.Fatalf("run %s: %s %s", final.GetStatus(), final.GetFailureCode(), final.GetFailureMessage())
	}
	if got := final.GetOutput().GetNumberValue(); got != 2 { // one address + one web service
		t.Fatalf("output %v", final.GetOutput())
	}

	// The graph holds the URL, the FQDN and the address.
	for _, at := range []string{"URL", "FQDN", "IPAddress", "Service", "Product"} {
		res, err := st.Graph.FindEntities(ctx, &pb.FindEntitiesRequest{ProgramId: prog.GetId(), AssetTypes: []string{at}})
		if err != nil {
			t.Fatal(err)
		}
		if len(res.GetEntities()) == 0 {
			t.Errorf("no %s entity in the graph", at)
		}
	}

	// The alert reached the webhook with a valid signature.
	servicetest.Eventually(t, 30*time.Second, "webhook delivery", func() bool { return len(hook.Received()) > 0 })
	var body map[string]any
	if err := json.Unmarshal(hook.Received()[0], &body); err != nil || body["rule"] != "new_url" || body["severity"] != "high" {
		t.Fatalf("webhook body %s (%v)", hook.Received()[0], err)
	}
	if hook.Rejected != 0 {
		t.Fatalf("%d deliveries had a bad signature", hook.Rejected)
	}

	// The scope-denied target never reached the network: no lookup of
	// evil.test, no request to the target with its name.
	if n := resolver.Count("evil.test"); n != 0 {
		t.Errorf("evil.test was resolved %d times", n)
	}
	for _, h := range tg.seen() {
		if h != "app.test:"+strconv.Itoa(port) {
			t.Errorf("the target saw Host %q", h)
		}
	}
	if len(tg.seen()) == 0 {
		t.Error("the target was never probed")
	}
	// Every decision is in the log; the denied passive re-check is there too.
	var denied bool
	for _, d := range scopeDecisions(t, st, tenant) {
		if d == "deny:evil.test" {
			denied = true
		}
	}
	if !denied {
		t.Error("no recorded deny for evil.test")
	}
}

func scopeDecisions(t *testing.T, st *devstack.Stack, tenant core.UUID) []string {
	t.Helper()
	var out []string
	if m, ok := st.ScopeRepo.(*scopesvc.MemRepo); ok {
		for _, d := range m.Decisions(tenant) {
			out = append(out, d.Decision.Result.String()+":"+d.Decision.Target)
		}
		return out
	}
	rows, err := st.DB.Query(context.Background(), `SELECT decision || ':' || target FROM scope.scope_decisions WHERE tenant_id = $1`, tenant.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func testLogger() *slog.Logger {
	if os.Getenv("FF_TEST_LOG") == "" {
		return nil
	}
	return platform.NewLogger(os.Stderr, "devstack", "test", slog.LevelDebug)
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
