package lab_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/contentstore"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/egressproxy"
	"github.com/genuinebnt/flowforge/services/internal/lab"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	"github.com/genuinebnt/flowforge/services/notifier"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
	"github.com/genuinebnt/flowforge/services/toolrunner"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestLabAcceptance_Memory is the V2_DESIGN §10 end-to-end acceptance:
// the reference monitoring workflow (conformance/workflows/monitor.ff)
// runs once per deploy of the lab site through the dev stack (engine,
// tool runner, egress proxy, scope, content, graph, notifier) and must
//  1. capture each deploy;
//  2. report exactly the planted level-3 changes and no noise;
//  3. link renamed chunks with previous_version;
//  4. raise each configured alert exactly as often as planted;
//  5. make every planted change findable through search_changes, by a
//     field query and by a regex query;
//  6. never contact an out-of-scope host (the lab's logs prove it);
//  7. store all versions in less than 25 % of their raw size.
func TestLabAcceptance_Memory(t *testing.T) { runAcceptance(t, "") }

// TestLabAcceptance_Postgres is the same with every store on Postgres.
func TestLabAcceptance_Postgres(t *testing.T) {
	if os.Getenv("FF_TEST_PG_DSN") == "" {
		t.Skip("FF_TEST_PG_DSN not set")
	}
	runAcceptance(t, servicetest.FreshDB(t))
}

type acceptance struct {
	t       *testing.T
	lab     *lab.Lab
	st      *devstack.Stack
	ctx     context.Context
	tenant  core.UUID
	program string
	runs    []string // run id per deploy
}

func runAcceptance(t *testing.T, dsn string) {
	l, err := lab.Start(os.DirFS(testkit.ConformanceDir(t, "")), "lab/shop/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	m := l.Manifest()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	hook := servicetest.NewWebhook(t, "hook-secret")
	st, err := devstack.Start(ctx, devstack.Options{DBDSN: dsn, Resolver: l.Resolver(), DNSUpstream: egressproxy.ServerUpstream{Addr: l.DNSAddr()},
		Secrets: notifier.NewMapSecrets(map[string]string{"test://hook": "hook-secret"}), AllowInsecureWebhooks: true,
		ToolClock: l.Clock, TargetTLS: l.ClientTLS(),
		Content: toolrunner.ContentConfig{Ports: []toolrunner.ProbePort{{Scheme: "https", Port: l.Port(lab.ListenerHTTPS)}}},
		Engine:  engine.Config{PollInterval: 10 * time.Millisecond, IdleBackoff: 50 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := &acceptance{t: t, lab: l, st: st, tenant: core.MustParseUUID("0190a1b2-0000-7000-8000-00000000c0de")}
	a.ctx = st.As(ctx, a.tenant, "user:0190a1b2-0000-7000-8000-0000000000c1", auth.RoleAdmin)

	// The program: the lab's in-scope hosts and their address (the proxy
	// dials loopback only under an explicit cidr rule); 127.0.0.2, where
	// the out-of-scope hosts live, is in no rule.
	doc := &pb.ScopeDocument{Activity: &pb.ActivityPolicy{MaxClass: pb.ActivityClass_ACTIVITY_CLASS_ACTIVE,
		Rate: &pb.RateLimit{RequestsPerSecond: 500, Burst: 1000, Concurrency: 32}},
		Rules: []*pb.ScopeRule{{Id: "lab-net", Effect: pb.RuleEffect_RULE_EFFECT_ALLOW, Kind: pb.RuleKind_RULE_KIND_CIDR, Value: lab.InAddr.String() + "/32"}}}
	for i, h := range l.Hosts("in") {
		doc.Rules = append(doc.Rules, &pb.ScopeRule{Id: fmt.Sprintf("host%d", i), Effect: pb.RuleEffect_RULE_EFFECT_ALLOW,
			Kind: pb.RuleKind_RULE_KIND_DOMAIN, Value: h})
	}
	prog, err := st.Scope.CreateProgram(a.ctx, &pb.CreateProgramRequest{Name: "lab", Platform: "lab", InitialScope: doc})
	if err != nil {
		t.Fatal(err)
	}
	a.program = prog.GetId()
	cfg, _ := structpb.NewStruct(map[string]any{"url": hook.URL + "/hook", "secret_ref": "test://hook"})
	if _, err := st.Notifier.PutChannel(a.ctx, &pb.PutChannelRequest{Kind: notifier.KindWebhook, Name: "lab", Enabled: true, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	rules := map[string]string{} // rule id -> name
	for _, r := range m.Expect.Alerts {
		rr, err := st.Notifier.PutRule(a.ctx, &pb.PutRuleRequest{Name: r.Name, Enabled: true, Source: r.Source, ProgramId: a.program})
		if err != nil {
			t.Fatalf("rule %s: %v", r.Name, err)
		}
		rules[rr.GetId()] = r.Name
	}
	src := testkit.ReadFile(t, "workflows", "monitor.ff")
	if _, err := st.Engine.RegisterWorkflow(a.ctx, &pb.RegisterWorkflowRequest{Name: "monitor", Source: string(src), ProgramId: a.program}); err != nil {
		t.Fatal(err)
	}

	root := l.URL(lab.ListenerHTTPS, "shop.lab.test", "/")
	reports := make([][]string, l.Deploys())
	for i := range l.Deploys() {
		if err := l.Deploy(i); err != nil {
			t.Fatal(err)
		}
		reports[i] = a.monitor(i, root)
	}

	changes := a.changes()
	a.checkCaptured()
	a.checkPlanted(changes, reports)
	a.checkLineage()
	a.checkAlerts(rules, hook)
	a.checkSearch(changes)
	a.checkOutOfScope()
	a.checkStorage()
}

// monitor runs monitor.ff for deploy i and returns its report.
func (a *acceptance) monitor(i int, root string) []string {
	t := a.t
	in, _ := structpb.NewStruct(map[string]any{"roots": []any{root}, "hosts": []any{"shop.lab.test", "api.lab.test"}, "budget": 30})
	run, err := a.st.Engine.SubmitRun(platform.WithIdempotencyKey(a.ctx, "lab-"+a.lab.DeployName(i)),
		&pb.SubmitRunRequest{WorkflowName: "monitor", ProgramId: a.program, Input: in})
	if err != nil {
		t.Fatal(err)
	}
	var final *pb.Run
	servicetest.Eventually(t, 2*time.Minute, "monitor run of "+a.lab.DeployName(i), func() bool {
		r, err := a.st.Engine.GetRun(a.ctx, &pb.GetRunRequest{RunId: run.GetId()})
		if err != nil {
			t.Fatal(err)
		}
		final = r
		return r.GetStatus() == pb.RunStatus_RUN_STATUS_SUCCEEDED || r.GetStatus() == pb.RunStatus_RUN_STATUS_FAILED
	})
	if final.GetStatus() != pb.RunStatus_RUN_STATUS_SUCCEEDED {
		evs, _ := a.st.Engine.ListRunEvents(a.ctx, &pb.ListRunEventsRequest{RunId: run.GetId(), Page: &pb.PageRequest{PageSize: 1000}})
		for _, e := range evs.GetEvents() {
			b, _ := json.Marshal(e.GetPayload())
			t.Logf("%d %s %s", e.GetSeq(), e.GetType(), b)
		}
		t.Fatalf("monitor %s: %s %s", a.lab.DeployName(i), final.GetFailureCode(), final.GetFailureMessage())
	}
	a.runs = append(a.runs, run.GetId())
	var out []string
	for _, v := range final.GetOutput().GetListValue().GetValues() {
		out = append(out, v.GetStringValue())
	}
	t.Logf("deploy %s report: %q", a.lab.DeployName(i), out)
	return out
}

// deployOf maps a capture time to its deploy index.
func (a *acceptance) deployOf(ts *timestamppb.Timestamp) int {
	m := a.lab.Manifest()
	return int(ts.AsTime().Sub(m.Start) / (time.Duration(m.DeployIntervalHours) * time.Hour))
}

// snapshots lists every stored version of the program, oldest first.
func (a *acceptance) snapshots() []*pb.ContentSnapshot {
	var out []*pb.ContentSnapshot
	tok := ""
	for {
		ls, err := a.st.Content.ListSnapshots(a.ctx, &pb.ListSnapshotsRequest{ProgramId: a.program, Page: &pb.PageRequest{PageSize: 1000, PageToken: tok}})
		if err != nil {
			a.t.Fatal(err)
		}
		out = append(out, ls.GetSnapshots()...)
		if tok = ls.GetPage().GetNextPageToken(); tok == "" {
			break
		}
	}
	slices.Reverse(out)
	return out
}

// change is one level-3 change of a stored version.
type change struct {
	deploy           int
	url, text        string
	snapshot, kindOp string
	alertable        bool
}

// key is the comparison form: deploy, URL and change, a secret's keyed
// hash cut off (it depends on the tenant's key).
func (c change) key(a *acceptance) string {
	text := c.text
	if op, rest, _ := strings.Cut(text, " "); strings.HasPrefix(rest, "secret|") {
		rule, _, _ := strings.Cut(strings.TrimPrefix(rest, "secret|"), ":")
		text = op + " secret|" + rule
	}
	return a.lab.DeployName(c.deploy) + " " + c.url + " " + text
}

// changes returns the level-3 changes of every stored version that is not
// a baseline, and fails on noise: a version whose change is not a counted
// non-change (no_change, normalized_equal, masked_only, reordered) must
// have level-3 changes.
func (a *acceptance) changes() []change {
	t := a.t
	var out []change
	outcomes := map[string]int{}
	for _, s := range a.snapshots() {
		d, err := a.st.Content.DiffSnapshots(a.ctx, &pb.DiffSnapshotsRequest{ToSnapshotId: s.GetId(), Format: pb.DiffFormat_DIFF_FORMAT_UNIFIED})
		if err != nil {
			t.Fatalf("diff %s: %v", s.GetUrl(), err)
		}
		cs := d.GetChange()
		if cs.GetBaseline() || d.GetFromSnapshotId() == "" {
			outcomes["baseline"]++
			if os.Getenv("FF_LAB_DEBUG") != "" {
				t.Logf("baseline %s %s status %d raw %d outcome %q degraded %v", a.lab.DeployName(a.deployOf(s.GetFetchedAt())), s.GetUrl(), s.GetStatus(), s.GetRawSize(), cs.GetOutcome(), s.GetDegraded())
			}
			continue
		}
		outcomes[cs.GetOutcome()]++
		if cs.GetOutcome() == "structural" {
			var arts []string
			for _, av := range d.GetArtifacts() {
				if av.GetOutcome() != "no_change" {
					arts = append(arts, av.GetDocId()+" "+av.GetOutcome()+" "+strings.Join(av.GetLevels(), ",")+":\n"+av.GetUnified())
				}
			}
			t.Errorf("noise: %s (%s) changed text without a level-3 change:\n%s", s.GetUrl(), a.lab.DeployName(a.deployOf(s.GetFetchedAt())),
				strings.Join(arts, "\n"))
		}
		for _, c := range cs.GetChanges() {
			out = append(out, change{deploy: a.deployOf(s.GetFetchedAt()), url: s.GetUrl(), text: c.GetOp() + " " + c.GetKind() + "|" + c.GetKey(),
				snapshot: s.GetId(), kindOp: c.GetKind() + "_" + c.GetOp(), alertable: cs.GetAlertable()})
		}
	}
	t.Logf("outcomes of stored versions: %v", outcomes)
	return out
}

// checkCaptured (§10 item 1): every deploy's run captured the page and
// every script, style and source map the page loads in that deploy: the
// lab served each to the tool runner during the deploy, and the content
// store holds a version of it valid at the deploy's time.
func (a *acceptance) checkCaptured() {
	t := a.t
	snaps := a.snapshots()
	m := a.lab.Manifest()
	for i := range a.lab.Deploys() {
		at := m.Start.Add(time.Duration(i*m.DeployIntervalHours) * time.Hour)
		want := map[string]bool{"/": true}
		for j := 0; j <= i; j++ {
			for _, f := range m.Deploys[j].Files {
				if f.Host == "shop.lab.test" && f.Listener == "" && (strings.HasPrefix(f.Path, "/static/") || strings.HasPrefix(f.Path, "/assets/")) {
					want[f.Path] = true
				}
			}
			for _, r := range m.Deploys[j].Remove {
				delete(want, r.Path)
			}
		}
		hit := map[string]bool{}
		for _, h := range a.lab.Hits() {
			if h.Deploy == a.lab.DeployName(i) && h.Host == "shop.lab.test" {
				hit[h.Path] = true
			}
		}
		for p := range want {
			u := a.lab.URL(lab.ListenerHTTPS, "shop.lab.test", p)
			if !hit[p] {
				t.Errorf("deploy %s: %s was not fetched", a.lab.DeployName(i), p)
			}
			valid := slices.ContainsFunc(snaps, func(s *pb.ContentSnapshot) bool {
				return s.GetUrl() == u && !s.GetFetchedAt().AsTime().After(at) && !s.GetLastSeen().AsTime().Before(at)
			})
			if !valid {
				t.Errorf("deploy %s: no stored version of %s covers %s", a.lab.DeployName(i), p, at.Format(time.RFC3339))
			}
		}
	}
}

// checkPlanted (§10 item 2): the level-3 changes are exactly the planted
// ones, each alertable, and each run reported its own deploy's.
func (a *acceptance) checkPlanted(changes []change, reports [][]string) {
	t := a.t
	var got, want []string
	for _, c := range changes {
		got = append(got, c.key(a))
		if !c.alertable {
			t.Errorf("planted change not alertable: %s", c.key(a))
		}
	}
	for _, p := range a.lab.Manifest().Expect.Planted {
		want = append(want, p.Deploy+" "+a.lab.Expand(p.URL)+" "+a.lab.Expand(p.Change))
	}
	sort.Strings(got)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Errorf("level-3 changes:\n%s\nwant (planted):\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i, rep := range reports {
		urls := map[string]bool{}
		for _, c := range changes {
			if c.deploy == i {
				urls[c.url] = true
			}
		}
		if len(rep) != len(urls) {
			t.Errorf("deploy %s: the workflow reported %d changed versions, %d have planted changes: %q", a.lab.DeployName(i), len(rep), len(urls), rep)
		}
		for _, line := range rep {
			u, _, _ := strings.Cut(line, ": ")
			if !urls[u] {
				t.Errorf("deploy %s: the workflow reported an unplanted change: %s", a.lab.DeployName(i), line)
			}
		}
	}
}

// checkLineage (§10 item 3): each renamed bundle's first version names its
// predecessor's version as lineage parent, and the graph links their
// Files with previous_version.
func (a *acceptance) checkLineage() {
	t := a.t
	snaps := a.snapshots()
	first := func(u string) *pb.ContentSnapshot {
		for _, s := range snaps {
			if s.GetUrl() == u {
				return s
			}
		}
		return nil
	}
	m := a.lab.Manifest()
	if len(m.Expect.Lineage) == 0 {
		t.Fatal("the manifest plants no renamed bundles")
	}
	for _, l := range m.Expect.Lineage {
		cur, parent := first(a.lab.Expand(l.URL)), first(a.lab.Expand(l.Parent))
		if cur == nil || parent == nil {
			t.Errorf("lineage %s <- %s: not captured", a.lab.Expand(l.URL), a.lab.Expand(l.Parent))
			continue
		}
		if a.lab.DeployName(a.deployOf(cur.GetFetchedAt())) != l.Deploy {
			t.Errorf("%s first captured in %s, want %s", l.URL, a.lab.DeployName(a.deployOf(cur.GetFetchedAt())), l.Deploy)
		}
		// The parent is the predecessor's open version at the rename.
		var pv *pb.ContentSnapshot
		for _, s := range snaps {
			if s.GetUrl() == parent.GetUrl() && !s.GetFetchedAt().AsTime().After(cur.GetFetchedAt().AsTime()) {
				pv = s
			}
		}
		if cur.GetLineageParentId() != pv.GetId() {
			t.Errorf("%s: lineage parent %q, want %s (%s)", l.URL, cur.GetLineageParentId(), pv.GetId(), l.Parent)
		}
		servicetest.Eventually(t, 30*time.Second, "previous_version edge for "+cur.GetUrl(), func() bool {
			return a.previousVersion(cur.GetUrl(), pv.GetUrl())
		})
	}
}

// previousVersion reports a graph edge File(from) -previous_version->
// File(to), the Files of two script URLs (oam File key: file:<SHA-256 of
// the URL>).
func (a *acceptance) previousVersion(from, to string) bool {
	key := func(u string) string { return "file:" + core.SHA256Hex([]byte(u)) }
	es, err := a.st.Graph.FindEntities(a.ctx, &pb.FindEntitiesRequest{ProgramId: a.program, AssetTypes: []string{"File"}, KeyPrefix: key(from)})
	if err != nil || len(es.GetEntities()) != 1 {
		return false
	}
	sg, err := a.st.Graph.Neighbors(a.ctx, &pb.NeighborsRequest{EntityId: es.GetEntities()[0].GetId(), Labels: []string{"previous_version"},
		Direction: pb.Direction_DIRECTION_OUT, Depth: 1})
	if err != nil {
		return false
	}
	for _, e := range sg.GetEntities() {
		if e.GetKey() == key(to) {
			return true
		}
	}
	return false
}

// checkAlerts (§10 item 4): each configured rule raised exactly its
// planted number of alerts, every one delivered once, and the content
// alerts show what changed (URL, changes, excerpt) without the secret.
func (a *acceptance) checkAlerts(rules map[string]string, hook *servicetest.Webhook) {
	t := a.t
	m := a.lab.Manifest()
	want := map[string]int{}
	total := 0
	for _, r := range m.Expect.Alerts {
		want[r.Name] = r.Count
		total += r.Count
	}
	count := func() (map[string]int, []*pb.AlertEvent) {
		ae, err := a.st.Notifier.ListAlertEvents(a.ctx, &pb.ListAlertEventsRequest{Page: &pb.PageRequest{PageSize: 500}})
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for _, e := range ae.GetEvents() {
			got[rules[e.GetRuleId()]]++
		}
		return got, ae.GetEvents()
	}
	servicetest.Eventually(t, time.Minute, "configured alerts", func() bool {
		got, _ := count()
		n := 0
		for _, v := range got {
			n += v
		}
		return n >= total && len(hook.Received()) >= total
	})
	// The content outbox and the bus are drained once the graph holds a
	// content_changed event for every changed version; no alert may come
	// after that.
	a.waitContentEvents()
	got, events := count()
	for name, n := range want {
		if got[name] != n {
			t.Errorf("rule %s raised %d alerts, want %d", name, got[name], n)
		}
	}
	if len(hook.Received()) != total || hook.Rejected != 0 {
		t.Errorf("webhook received %d deliveries (%d rejected), want %d", len(hook.Received()), hook.Rejected, total)
	}
	diffs := 0
	for _, e := range events {
		var body struct {
			Summary string `json:"summary"`
			Diff    *struct {
				URL     string   `json:"url"`
				Changes []string `json:"changes"`
				Summary string   `json:"summary"`
				Excerpt string   `json:"excerpt"`
			} `json:"diff"`
		}
		raw, _ := json.Marshal(e.GetPayload().AsMap())
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "IOSFODNN7") || strings.Contains(string(raw), "EXAMPLEKEY") {
			t.Errorf("an alert carries the leaked key: %s", raw)
		}
		if body.Diff == nil {
			continue
		}
		diffs++
		if body.Diff.URL == "" || len(body.Diff.Changes) == 0 || body.Diff.Summary == "" || body.Diff.Excerpt == "" ||
			!strings.Contains(body.Summary, body.Diff.URL) {
			t.Errorf("content alert does not show the change: %s", raw)
		}
	}
	if diffs == 0 {
		t.Error("no alert carried a diff")
	}
}

// waitContentEvents waits until the graph recorded content_changed for
// every content event the content service published (bounded).
func (a *acceptance) waitContentEvents() {
	t := a.t
	want := a.contentChangedWant()
	servicetest.Eventually(t, 30*time.Second, "content events in the graph", func() bool {
		n, tok := 0, ""
		for {
			d, err := a.st.Graph.Diff(a.ctx, &pb.DiffRequest{ProgramId: a.program, From: &pb.DiffRequest_Since{Since: timestamppb.New(time.Unix(0, 0))},
				Page: &pb.PageRequest{PageSize: 500, PageToken: tok}})
			if err != nil {
				return false
			}
			for _, c := range d.GetChanges() {
				if c.GetType() == "content_changed" {
					n++
				}
			}
			if tok = d.GetPage().GetNextPageToken(); tok == "" {
				break
			}
		}
		return n >= want
	})
}

// contentChangedWant counts the versions that are changes or baselines
// (each publishes content.changed, which the graph records).
func (a *acceptance) contentChangedWant() int {
	n := 0
	for _, s := range a.snapshots() {
		d, err := a.st.Content.DiffSnapshots(a.ctx, &pb.DiffSnapshotsRequest{ToSnapshotId: s.GetId(), Format: pb.DiffFormat_DIFF_FORMAT_UNIFIED})
		if err != nil {
			a.t.Fatal(err)
		}
		switch c := d.GetChange(); {
		case c.GetBaseline(), c.GetOutcome() == "changed", c.GetOutcome() == "structural":
			n++
		}
	}
	return n
}

// search runs search_changes through the tool runner (the tool the
// workflow surface offers) and returns the hits' snapshot ids and kinds.
func (a *acceptance) search(q string) []map[string]any {
	in, _ := structpb.NewValue([]any{q, 100})
	ctx := a.st.As(context.WithoutCancel(a.ctx), a.tenant, "service:engine", auth.RoleAnalyst)
	s, err := a.st.ToolRunner.Execute(ctx, &pb.ExecuteRequest{Tool: "search_changes", Input: in, ProgramId: a.program,
		RunId: a.runs[len(a.runs)-1], EffectPath: "search", IdempotencyKey: q})
	if err != nil {
		a.t.Fatal(err)
	}
	for {
		m, err := s.Recv()
		if err != nil {
			a.t.Fatalf("search %q: %v", q, err)
		}
		if e := m.GetError(); e != nil {
			a.t.Fatalf("search %q: %s %s", q, e.GetCode(), e.GetMessage())
		}
		if d := m.GetDone(); d != nil {
			var out []map[string]any
			for _, v := range d.GetOutput().GetListValue().GetValues() {
				out = append(out, v.GetStructValue().AsMap())
			}
			return out
		}
	}
}

// checkSearch (§10 item 5): every planted change is found by a field
// query on its kind and by the manifest's regex query, at its version.
func (a *acceptance) checkSearch(changes []change) {
	t := a.t
	for _, p := range a.lab.Manifest().Expect.Planted {
		key := p.Deploy + " " + a.lab.Expand(p.URL) + " " + a.lab.Expand(p.Change)
		var c *change
		for i := range changes {
			if changes[i].key(a) == key {
				c = &changes[i]
			}
		}
		if c == nil {
			continue // reported by checkPlanted
		}
		found := func(q string, kind bool) bool {
			for _, h := range a.search(q) {
				if h["snapshot_id"] == c.snapshot && (!kind || h["kind"] == c.kindOp) {
					return true
				}
			}
			return false
		}
		if q := "kind:" + c.kindOp; !found(q, true) {
			t.Errorf("%s: not found by the field query %s", key, q)
		}
		if p.Regex == "" || !found(a.lab.Expand(p.Regex), false) {
			t.Errorf("%s: not found by the regex query %q", key, p.Regex)
		}
	}
}

// checkOutOfScope (§10 item 6): the out-of-scope listener saw no request,
// the lab's DNS server no question for an out-of-scope name, robots.txt's
// disallowed paths were never requested, and the scope service recorded
// the refusals that kept them out.
func (a *acceptance) checkOutOfScope() {
	t := a.t
	out := a.lab.Hosts("out")
	for _, h := range a.lab.Hits() {
		if h.Listener == lab.ListenerOut || slices.Contains(out, h.Host) {
			t.Errorf("out-of-scope host contacted: %+v", h)
		}
	}
	for _, q := range a.lab.Queries() {
		if slices.Contains(out, q.Name) {
			t.Errorf("out-of-scope name resolved: %+v", q)
		}
	}
	for _, r := range a.lab.Manifest().Expect.NeverRequested {
		for _, h := range a.lab.Hits() {
			if h.Host == r.Host && h.Path == r.Path {
				t.Errorf("robots.txt-disallowed %s%s was requested", r.Host, r.Path)
			}
		}
	}
	denied := map[string]bool{}
	if mr, ok := a.st.ScopeRepo.(*scopesvc.MemRepo); ok {
		for _, d := range mr.Decisions(a.tenant) {
			if !d.Decision.Allowed() {
				denied[d.Decision.Target] = true
			}
		}
	} else {
		rows, err := a.st.DB.Query(context.Background(), `SELECT target FROM scope.scope_decisions WHERE tenant_id = $1 AND decision = 'deny'`, a.tenant.String())
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			denied[s] = true
		}
		rows.Close()
	}
	for _, h := range out {
		if !slices.ContainsFunc(keys(denied), func(s string) bool { return strings.Contains(s, h) }) {
			t.Errorf("no recorded refusal for %s: the lab never tempted the monitor with it", h)
		}
	}
	t.Logf("lab requests %d (out-of-scope 0), DNS questions %d, recorded refusals %d", len(a.lab.Hits()), len(a.lab.Queries()), len(denied))
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// checkStorage (§10 item 7): the raw snapshots (capture record and redacted
// body of every version, CSTORE-INV-08's measure) take less than 25 % of
// the versions' raw size; the whole store is logged.
func (a *acceptance) checkStorage() {
	t := a.t
	store := a.st.ContentServer.Store()
	var raw, versions, derived int64
	for _, s := range a.snapshots() {
		id, err := core.ParsePublicID(contentstore.PrefixSnapshot, s.GetId())
		if err != nil {
			t.Fatal(err)
		}
		sn, err := store.GetSnapshot(a.ctx, a.tenant, id)
		if err != nil {
			t.Fatal(err)
		}
		ss, err := store.SnapshotStorage(a.ctx, a.tenant, sn)
		if err != nil {
			t.Fatal(err)
		}
		if os.Getenv("FF_LAB_DEBUG") != "" {
			t.Logf("storage %s %s raw %d stored %d derived %d", a.lab.DeployName(a.deployOf(s.GetFetchedAt())), s.GetUrl(), sn.RawSize, ss.Raw, ss.Derived)
		}
		raw += sn.RawSize
		versions += ss.Raw
		derived += ss.Derived
	}
	m, err := store.StorageMetrics(a.ctx, a.tenant)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("storage: %d versions, raw %d B; raw snapshots %d B (%.1f %%); with normalized text and features %d B (%.1f %%); all records incl. diffs %d B (%.1f %%)",
		len(a.snapshots()), raw, versions, pct(versions, raw), versions+derived, pct(versions+derived, raw), m.CompressedBytes, pct(int64(m.CompressedBytes), raw))
	if raw == 0 || 4*versions >= raw {
		t.Errorf("raw snapshots take %d of %d raw bytes (%.1f %%), want < 25 %%", versions, raw, pct(versions, raw))
	}
}

func pct(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return 100 * float64(a) / float64(b)
}
