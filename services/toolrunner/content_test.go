package toolrunner_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"

	"github.com/genuinebnt/flowforge/compiler"
	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/dnsmsg"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
	contentsvc "github.com/genuinebnt/flowforge/services/content"
	"github.com/genuinebnt/flowforge/services/egressproxy"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/lab"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/wire"
	"github.com/genuinebnt/flowforge/services/toolrunner"
	"github.com/genuinebnt/flowforge/testkit"
)

// The content tools against the lab (services/internal/lab) without the
// egress proxy: a lab network routes requests to the lab's listeners,
// a scope fake allows the lab's in-scope hosts, and the content service
// runs in process. The full stack (engine, proxy, graph, notifier) is
// the lab's acceptance test.

// directContent calls a content server in process (toolrunner.ContentClient).
type directContent struct{ s *contentsvc.Server }

func (d directContent) IngestCapture(ctx context.Context, in *pb.IngestCaptureRequest, _ ...grpc.CallOption) (*pb.IngestCaptureResponse, error) {
	return d.s.IngestCapture(ctx, in)
}
func (d directContent) GetSnapshot(ctx context.Context, in *pb.GetSnapshotRequest, _ ...grpc.CallOption) (*pb.ContentSnapshot, error) {
	return d.s.GetSnapshot(ctx, in)
}
func (d directContent) ListSnapshots(ctx context.Context, in *pb.ListSnapshotsRequest, _ ...grpc.CallOption) (*pb.ListSnapshotsResponse, error) {
	return d.s.ListSnapshots(ctx, in)
}
func (d directContent) DiffSnapshots(ctx context.Context, in *pb.DiffSnapshotsRequest, _ ...grpc.CallOption) (*pb.SnapshotDiff, error) {
	return d.s.DiffSnapshots(ctx, in)
}
func (d directContent) SearchChanges(ctx context.Context, in *pb.SearchChangesRequest, _ ...grpc.CallOption) (*pb.SearchChangesResponse, error) {
	return d.s.SearchChanges(ctx, in)
}

// labNet is a toolrunner.Network that dials the lab's in-scope address for
// every allowed decision (as the proxy dials the decision's pinned IPs).
type labNet struct {
	t   *testing.T
	lab *lab.Lab
}

func (n labNet) HTTPClient(_ *toolrunner.ToolCtx, d scope.Decision, _ func(string, string)) *http.Client {
	if !d.Allowed() || len(d.PinnedIPs) == 0 {
		n.t.Errorf("request under decision %+v", d)
	}
	pin := d.PinnedIPs[0]
	tr := &http.Transport{TLSClientConfig: n.lab.ClientTLS(), DisableKeepAlives: true,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, _ := net.SplitHostPort(addr)
			var dl net.Dialer
			return dl.DialContext(ctx, network, net.JoinHostPort(pin.String(), port))
		}}
	return &http.Client{Transport: tr, Timeout: 10 * time.Second}
}
func (n labNet) IntelClient(*toolrunner.ToolCtx, scope.Decision) *http.Client {
	n.t.Error("intel client used by a content tool")
	return nil
}
func (n labNet) DialTCP(context.Context, *toolrunner.ToolCtx, scope.Decision, string, int, []int) (net.Conn, error) {
	n.t.Error("raw TCP used by a content tool")
	return nil, net.ErrClosed
}
func (n labNet) LookupDNS(context.Context, *toolrunner.ToolCtx, scope.Decision, string, dnsmsg.Type) (egressproxy.DNSAnswer, error) {
	n.t.Error("DNS used by a content tool")
	return egressproxy.DNSAnswer{}, toolrunner.ErrNoDNS
}

// rec records an execution's stream.
type rec struct {
	mu      sync.Mutex
	skipped []string
	obs     int
}

func (r *rec) Observation(oam.Observation) error   { r.mu.Lock(); r.obs++; r.mu.Unlock(); return nil }
func (r *rec) Progress(int64, int64, string) error { return nil }
func (r *rec) Skipped(target, reason string) error {
	r.mu.Lock()
	r.skipped = append(r.skipped, reason+" "+target)
	r.mu.Unlock()
	return nil
}

type contentRig struct {
	t      *testing.T
	lab    *lab.Lab
	tools  map[string]toolrunner.Adapter
	store  *contentsvc.Server
	clock  *lab.Clock
	checks int
	mu     sync.Mutex
}

var (
	rigTenant  = core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000a1")
	rigProgram = core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000a2")
)

func newContentRig(t *testing.T, cfg toolrunner.ContentConfig) *contentRig {
	t.Helper()
	l, err := lab.Start(os.DirFS(testkit.ConformanceDir(t, "")), "lab/shop/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Close)
	env := &wire.Env{Service: "content", Logger: platform.DiscardLogger(), Clock: &core.SystemClock{}}
	comp, srv, err := wire.Content(env, wire.ContentOptions{SweepInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(comp.Close)
	cfg.Content = directContent{srv}
	if cfg.Ports == nil {
		cfg.Ports = []toolrunner.ProbePort{{Scheme: "https", Port: l.Port(lab.ListenerHTTPS)}}
	}
	ads, err := toolrunner.ContentAdapters(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := &contentRig{t: t, lab: l, tools: map[string]toolrunner.Adapter{}, store: srv, clock: l.Clock}
	for _, a := range ads {
		r.tools[a.Spec().Name] = a
	}
	return r
}

// check allows URLs and hosts of the lab's in-scope hosts, pinned to the
// in-scope address; everything else is denied.
func (r *contentRig) check(_ context.Context, target string, kind scope.TargetKind, _ core.ActivityClass) (scope.Decision, error) {
	r.mu.Lock()
	r.checks++
	r.mu.Unlock()
	t, err := scope.Normalize(target, kind)
	if err != nil {
		return scope.Decision{Result: scope.ResultDeny, Reason: "invalid_target"}, nil
	}
	if slices.Contains(r.lab.Hosts("in"), t.Name()) {
		return scope.Decision{ID: "dec_" + target, Result: scope.ResultAllow, Reason: "allow", Target: target,
			PinnedIPs: []netip.Addr{lab.InAddr}}, nil
	}
	return scope.Decision{Result: scope.ResultDeny, Reason: "no_matching_allow", Target: target}, nil
}

// run executes a tool as run number run.
func (r *contentRig) run(run int, tool string, input any) (json.RawMessage, *rec, error) {
	r.t.Helper()
	in, err := json.Marshal(input)
	if err != nil {
		r.t.Fatal(err)
	}
	tc := &toolrunner.ToolCtx{Tenant: rigTenant, Program: rigProgram, Run: core.MustParseUUID("0190a1b2-0000-7000-8000-0000000001" + strconv.Itoa(10+run)),
		Path: "0", IdempotencyKey: "k", Clock: r.clock, Check: r.check, Net: labNet{t: r.t, lab: r.lab}}
	e := &rec{}
	out, err := r.tools[tool].Run(context.Background(), in, tc, e)
	return out, e, err
}

func (r *contentRig) hits(path string) int {
	n := 0
	for _, h := range r.lab.Hits() {
		if h.Path == path {
			n++
		}
	}
	return n
}

func (r *contentRig) outOfScopeHits() []lab.Hit {
	var out []lab.Hit
	for _, h := range r.lab.Hits() {
		if h.Listener == lab.ListenerOut {
			out = append(out, h)
		}
	}
	return out
}

type snap struct {
	ID              string  `json:"id"`
	URL             string  `json:"url"`
	Status          int     `json:"status"`
	Kind            string  `json:"kind"`
	LineageParentID *string `json:"lineage_parent_id"`
	NewInRun        bool    `json:"new_in_run"`
	Redirects       []string
	Resources       []snap `json:"resources"`
}

func decode[T any](t *testing.T, b json.RawMessage) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

// TestContentTools_Signatures pins the V2_DESIGN §8 table: names, DSL
// signatures and effect classes of the content tools.
func TestContentTools_Signatures(t *testing.T) {
	ads, err := toolrunner.ContentAdapters(toolrunner.ContentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][3]string{
		"collect":          {"(InScope<Url>, String)", "Snapshot", "active"},
		"crawl":            {"(InScope<Url>, Int)", "List<Url>", "active"},
		"wellknown":        {"InScope<Host>", "List<Url>", "active"},
		"extract":          {"Snapshot", "Features", "read"},
		"unpack_sourcemap": {"Snapshot", "List<Snapshot>", "read"},
		"diff_latest":      {"Snapshot", "ChangeSet", "read"},
		"search_changes":   {"(String, Int)", "List<ChangeHit>", "read"},
	}
	if len(ads) != len(want) {
		t.Fatalf("%d content tools, want %d", len(ads), len(want))
	}
	for _, a := range ads {
		es := a.Spec().EngineSpec(convert.DefaultToolRetry)
		w, ok := want[es.Name]
		if !ok || es.InputType != w[0] || es.OutputType != w[1] || es.EffectClass != w[2] {
			t.Errorf("%s: (%s) -> %s ! %s, want %v", es.Name, es.InputType, es.OutputType, es.EffectClass, w)
		}
	}
	if _, err := toolrunner.ContentAdapters(toolrunner.ContentConfig{Profiles: []content.Profile{{Name: "authed", Method: "GET",
		Headers: []content.Header{{Name: "Authorization", Value: "Bearer x"}}}}}); err == nil {
		t.Error("a profile carrying credentials was accepted")
	}
}

// TestContentTools_MonitorWorkflowCompiles compiles the reference
// monitoring workflow against the tool runner's registry exactly as the
// engine does (spec hashes, effect classes): it type-checks, is active,
// and a passive program refuses it.
func TestContentTools_MonitorWorkflowCompiles(t *testing.T) {
	src := testkit.ReadFile(t, "workflows", "monitor.ff")
	ads, err := toolrunner.ContentAdapters(toolrunner.ContentConfig{})
	if err != nil {
		t.Fatal(err)
	}
	var tools []compiler.AdapterTool
	for _, a := range append(toolrunner.DefaultAdapters(toolrunner.HTTPProbe{}), ads...) {
		es := a.Spec().EngineSpec(convert.DefaultToolRetry)
		tools = append(tools, compiler.AdapterTool{Name: es.Name, InputType: es.InputType, OutputType: es.OutputType,
			EffectClass: es.EffectClass, Idempotent: es.Idempotent, SpecSHA256: es.SpecSHA256})
	}
	res, err := compiler.EngineAdapter{}.Compile(context.Background(), compiler.AdapterRequest{Name: "monitor", Source: string(src),
		Policy: &compiler.AdapterPolicy{MaxClass: core.ClassActive}, Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK {
		b, _ := json.MarshalIndent(res.Diagnostics, "", " ")
		t.Fatalf("monitor.ff does not compile:\n%s", b)
	}
	if res.Class != core.ClassActive || !slices.Contains(res.Effects, "active") || !slices.Contains(res.Effects, "read") ||
		!slices.Contains(res.Effects, "alert") || len(res.Module) == 0 {
		t.Fatalf("class %v effects %v", res.Class, res.Effects)
	}
	passive, err := compiler.EngineAdapter{}.Check(context.Background(), compiler.AdapterRequest{Name: "monitor", Source: string(src),
		Policy: &compiler.AdapterPolicy{MaxClass: core.ClassPassive}, Tools: tools})
	if err != nil || passive.OK || len(passive.Diagnostics) == 0 || passive.Diagnostics[0].Code != "E0402" {
		t.Fatalf("passive program: ok %v %+v %v", passive.OK, passive.Diagnostics, err)
	}
}

// TestContentTools_CollectPageAndResources: collect stores the page and
// the scripts, styles and source maps it loads (page_url set, so the
// renamed bundle of the next deploy links to its predecessor); the
// third-party script is scope-denied and never contacted; a second
// collect in the same run reuses the capture; an unknown profile is
// invalid input.
func TestContentTools_CollectPageAndResources(t *testing.T) {
	r := newContentRig(t, toolrunner.ContentConfig{})
	root := r.lab.URL(lab.ListenerHTTPS, "shop.lab.test", "/")
	if err := r.lab.Deploy(1); err != nil {
		t.Fatal(err)
	}
	out, e, err := r.run(1, "collect", []any{root, "default"})
	if err != nil {
		t.Fatal(err)
	}
	page := decode[snap](t, out)
	if page.Kind != "html" || page.Status != 200 || !page.NewInRun || page.URL != root {
		t.Fatalf("page %+v", page)
	}
	var got []string
	for _, s := range page.Resources {
		got = append(got, strings.TrimPrefix(s.URL, strings.TrimSuffix(root, "/")))
	}
	// Feature order (kind, then key), then each script's source map.
	want := []string{"/assets/index-a1b2c3d4.js", "/assets/store-0f0f0f0f.js", "/static/js/main.77e0d41a.js", "/static/css/main.0a1b2c3d.css",
		"/static/js/main.77e0d41a.js.map"}
	if !slices.Equal(got, want) {
		t.Fatalf("resources %v, want %v (skipped %v)", got, want, e.skipped)
	}
	if e.obs != 0 {
		t.Fatalf("collect streamed %d observations; the content service publishes them", e.obs)
	}
	hits := r.hits("/")
	again, _, err := r.run(1, "collect", []any{root, "default"})
	if err != nil || decode[snap](t, again).ID != page.ID || r.hits("/") != hits {
		t.Fatalf("second collect in the run: %s %v (page fetched %d times, was %d)", again, err, r.hits("/"), hits)
	}

	// The next deploy renames the bundle; its capture links to the
	// previous one (V2 §5 lineage) and is new in run 2.
	if err := r.lab.Deploy(2); err != nil {
		t.Fatal(err)
	}
	out, e, err = r.run(2, "collect", []any{root, "default"})
	if err != nil {
		t.Fatal(err)
	}
	page = decode[snap](t, out)
	var bundle *snap
	for i := range page.Resources {
		if strings.HasSuffix(page.Resources[i].URL, "/static/js/main.5b6c7d8e.js") {
			bundle = &page.Resources[i]
		}
	}
	old := decode[snap](t, mustRun(t, r, 1, "collect", []any{r.lab.URL(lab.ListenerHTTPS, "shop.lab.test", "/static/js/main.77e0d41a.js"), "default"}))
	if bundle == nil || bundle.LineageParentID == nil || *bundle.LineageParentID != old.ID || !bundle.NewInRun {
		t.Fatalf("renamed bundle %+v, want lineage parent %s", bundle, old.ID)
	}
	cdn := "no_matching_allow http://cdn.thirdparty.test:" + strconv.Itoa(r.lab.Port(lab.ListenerOut)) + "/widget.js"
	if !slices.Contains(e.skipped, cdn) {
		t.Fatalf("third-party script not skipped: %v", e.skipped)
	}
	if hs := r.outOfScopeHits(); len(hs) != 0 {
		t.Fatalf("out-of-scope hosts contacted: %+v", hs)
	}
	if _, _, err := r.run(2, "collect", []any{root, "nonexistent"}); err == nil || !strings.Contains(err.Error(), "invalid_input") {
		t.Fatalf("unknown profile: %v", err)
	}
}

func mustRun(t *testing.T, r *contentRig, run int, tool string, input any) json.RawMessage {
	t.Helper()
	out, _, err := r.run(run, tool, input)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return out
}

// TestContentTools_CollectRedirects: every hop is re-checked; a hop to an
// out-of-scope host is never followed (the redirect response itself is
// the capture), an in-scope one is followed and recorded; a profile that
// does not follow redirects captures the redirect, under its own
// (URL, profile) history.
func TestContentTools_CollectRedirects(t *testing.T) {
	r := newContentRig(t, toolrunner.ContentConfig{Profiles: []content.Profile{{Name: "no-follow", Method: "GET", Accept: "text/html"}}})
	if err := r.lab.Deploy(0); err != nil {
		t.Fatal(err)
	}
	gone := decode[snap](t, mustRun(t, r, 1, "collect", []any{r.lab.URL(lab.ListenerHTTPS, "shop.lab.test", "/go"), "default"}))
	if gone.Status != 302 || len(gone.Redirects) != 0 {
		t.Fatalf("redirect out of scope: %+v", gone)
	}
	legacy := r.lab.URL(lab.ListenerHTTP, "shop.lab.test", "/legacy")
	moved := decode[snap](t, mustRun(t, r, 1, "collect", []any{legacy, "default"}))
	catalog := r.lab.URL(lab.ListenerHTTPS, "shop.lab.test", "/catalog")
	if moved.Status != 200 || !slices.Equal(moved.Redirects, []string{catalog}) || moved.URL != legacy {
		t.Fatalf("redirect in scope: %+v", moved)
	}
	type profiled struct {
		snap
		Profile string `json:"profile"`
	}
	stay := decode[profiled](t, mustRun(t, r, 1, "collect", []any{legacy, "no-follow"}))
	if stay.Status != 301 || len(stay.Redirects) != 0 || stay.Profile != "no-follow" || stay.ID == moved.ID {
		t.Fatalf("no-follow profile: %+v", stay)
	}
	if hs := r.outOfScopeHits(); len(hs) != 0 {
		t.Fatalf("out-of-scope hosts contacted: %+v", hs)
	}
}

// TestContentTools_CrawlFrontier: the crawl is robots-aware (a
// disallowed path is never requested), scope-checks every URL (the
// out-of-scope link is skipped, never contacted), stops fetching at the
// budget and returns the rest of the frontier; a URL that is not due is
// expanded from its stored version without a fetch, and fetched again
// once the revisit interval has passed.
func TestContentTools_CrawlFrontier(t *testing.T) {
	r := newContentRig(t, toolrunner.ContentConfig{})
	if err := r.lab.Deploy(0); err != nil {
		t.Fatal(err)
	}
	u := func(p string) string { return r.lab.URL(lab.ListenerHTTPS, "shop.lab.test", p) }
	root := u("/")
	out, e, err := r.run(1, "crawl", []any{root, 20})
	if err != nil {
		t.Fatal(err)
	}
	got := decode[[]string](t, out)
	// Breadth first, links in feature (key) order; the redirecting
	// /legacy contributes its target (the catalog) instead of its links.
	want := []string{root, r.lab.URL(lab.ListenerHTTP, "shop.lab.test", "/legacy"), u("/account"), u("/go"), u("/products"), u("/catalog")}
	if !slices.Equal(got, want) {
		t.Fatalf("frontier %v, want %v", got, want)
	}
	if r.hits("/private/admin") != 0 || !slices.Contains(e.skipped, "robots_disallowed "+u("/private/admin")) {
		t.Fatalf("robots.txt not honoured: %v", e.skipped)
	}
	evil := "http://evil.lab.test:" + strconv.Itoa(r.lab.Port(lab.ListenerOut)) + "/"
	if !slices.Contains(e.skipped, "no_matching_allow "+evil) || len(r.outOfScopeHits()) != 0 {
		t.Fatalf("out-of-scope link: skipped %v, hits %v", e.skipped, r.outOfScopeHits())
	}

	// Run 2 at the same time: nothing is due, so nothing is fetched but
	// robots.txt, and the frontier is the same.
	before := len(r.lab.Hits())
	out, _, err = r.run(2, "crawl", []any{root, 20})
	if err != nil || !slices.Equal(decode[[]string](t, out), want) {
		t.Fatalf("not-due crawl: %s %v", out, err)
	}
	for _, h := range r.lab.Hits()[before:] {
		if h.Path != "/robots.txt" {
			t.Fatalf("not-due crawl fetched %s", h.Path)
		}
	}
	// 25 h later (past the first 24 h interval) every page is due again.
	r.clock.Set(r.clock.Now().Add(25 * time.Hour))
	before = r.hits("/")
	if _, _, err := r.run(3, "crawl", []any{root, 20}); err != nil || r.hits("/") != before+1 {
		t.Fatalf("due crawl did not fetch the root: %v", err)
	}

	// A budget of 2 fetches the root and /legacy and returns the rest of
	// the frontier unfetched.
	r.clock.Set(r.clock.Now().Add(8 * 24 * time.Hour))
	before = r.hits("/account")
	out, _, err = r.run(4, "crawl", []any{root, 2})
	if err != nil || !slices.Equal(decode[[]string](t, out), want) || r.hits("/account") != before {
		t.Fatalf("budget 2: %s %v (account fetched %d times, was %d)", out, err, r.hits("/account"), before)
	}
}

// TestContentTools_WellKnown: robots.txt, security.txt, the sitemap and
// its in-scope entries on the shop; the OpenAPI document and GraphQL
// endpoint on the API host; a missing resource (404) is not found and
// the out-of-scope sitemap entry is skipped.
func TestContentTools_WellKnown(t *testing.T) {
	r := newContentRig(t, toolrunner.ContentConfig{})
	if err := r.lab.Deploy(0); err != nil {
		t.Fatal(err)
	}
	shop := func(p string) string { return r.lab.URL(lab.ListenerHTTPS, "shop.lab.test", p) }
	out, e, err := r.run(1, "wellknown", "shop.lab.test")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{shop("/robots.txt"), shop("/.well-known/security.txt"), shop("/sitemap.xml"), shop("/"), shop("/about"), shop("/products")}
	if got := decode[[]string](t, out); !slices.Equal(got, want) {
		t.Fatalf("shop: %v, want %v (skipped %v)", got, want, e.skipped)
	}
	api := func(p string) string { return r.lab.URL(lab.ListenerHTTPS, "api.lab.test", p) }
	out, _, err = r.run(1, "wellknown", map[string]any{"variant": "Name", "values": []string{"api.lab.test"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := decode[[]string](t, out); !slices.Equal(got, []string{api("/openapi.json"), api("/graphql")}) {
		t.Fatalf("api: %v", got)
	}
	if len(r.outOfScopeHits()) != 0 {
		t.Fatalf("out-of-scope hosts contacted: %v", r.outOfScopeHits())
	}
}

// TestContentTools_ReadTools: extract, diff_latest, unpack_sourcemap and
// search_changes over two deploys of the page and bundle.
func TestContentTools_ReadTools(t *testing.T) {
	r := newContentRig(t, toolrunner.ContentConfig{})
	root := r.lab.URL(lab.ListenerHTTPS, "shop.lab.test", "/")
	origin := strings.TrimSuffix(root, "/")
	var pageRaw json.RawMessage
	for run, d := range []int{1, 2} {
		if err := r.lab.Deploy(d); err != nil {
			t.Fatal(err)
		}
		pageRaw = mustRun(t, r, run+1, "collect", []any{root, "default"})
	}
	page := decode[snap](t, pageRaw)
	type features struct {
		Scripts, Forms, Secrets, Endpoints []string
	}
	f := decode[features](t, mustRun(t, r, 2, "extract", json.RawMessage(pageRaw)))
	if !slices.Contains(f.Forms, "ep:POST "+origin+"/account/delete") || !slices.Contains(f.Secrets, "aws_access_key_id:AKIA...LE") ||
		!slices.Contains(f.Scripts, origin+"/static/js/main.5b6c7d8e.js") {
		t.Fatalf("features %+v", f)
	}
	for _, s := range f.Secrets {
		if strings.Contains(s, "IOSFODNN7") {
			t.Fatalf("a secret left the content store: %v", f.Secrets)
		}
	}
	type changeSet struct {
		Outcome   string   `json:"outcome"`
		Semantic  bool     `json:"semantic"`
		Alertable bool     `json:"alertable"`
		Baseline  bool     `json:"baseline"`
		Changes   []string `json:"changes"`
	}
	// The d3 page against the d2 version: the planted changes.
	cs := decode[changeSet](t, mustRun(t, r, 2, "diff_latest", json.RawMessage(pageRaw)))
	if !cs.Semantic || !cs.Alertable || !slices.Contains(cs.Changes, "added form|ep:POST "+origin+"/account/delete") {
		t.Fatalf("page change %+v", cs)
	}
	var bundle json.RawMessage
	pr := decode[struct {
		Resources []json.RawMessage `json:"resources"`
	}](t, pageRaw)
	for i, s := range page.Resources {
		if strings.HasSuffix(s.URL, "main.5b6c7d8e.js") {
			bundle = pr.Resources[i]
		}
	}
	bs := decode[changeSet](t, mustRun(t, r, 2, "diff_latest", bundle))
	if !bs.Semantic || !slices.Contains(bs.Changes, "added endpoint|ep:POST "+origin+"/api/v1/admin/export") {
		t.Fatalf("bundle change %+v", bs)
	}
	origs := decode[[]snap](t, mustRun(t, r, 2, "unpack_sourcemap", bundle))
	if len(origs) != 2 || !strings.HasSuffix(origs[0].ID, "#src/shop/node_modules/tinylib/index.js") ||
		!strings.HasSuffix(origs[1].ID, "#src/shop/src/api.js") || origs[1].Kind != "js" {
		t.Fatalf("originals %+v", origs)
	}
	of := decode[features](t, mustRun(t, r, 2, "extract", map[string]any{"id": origs[1].ID}))
	if !slices.Contains(of.Endpoints, "ep:POST "+origin+"/api/v1/admin/export") || len(of.Forms) != 0 {
		t.Fatalf("original's features %v", of)
	}
	hits := decode[[]map[string]any](t, mustRun(t, r, 2, "search_changes", []any{"kind:endpoint_added /admin\\/export/", 10}))
	if len(hits) == 0 || !strings.HasSuffix(hits[0]["url"].(string), "main.5b6c7d8e.js") {
		t.Fatalf("search hits %v", hits)
	}
	if _, _, err := r.run(2, "search_changes", []any{"(", 10}); err == nil || !strings.Contains(err.Error(), "invalid_input") {
		t.Fatalf("bad query: %v", err)
	}
	// Another program's snapshot is not readable.
	other := &toolrunner.ToolCtx{Tenant: rigTenant, Program: core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000a3"), Clock: r.clock}
	if _, err := r.tools["extract"].Run(context.Background(), pageRaw, other, &rec{}); err == nil || !strings.Contains(err.Error(), "not_found") {
		t.Fatalf("cross-program read: %v", err)
	}
}

// TestContentTools_CollectBodyBound: a body longer than MaxBody is cut
// there and stored as a partial capture (never complete coverage, so it
// cannot withdraw graph facts, V2 §3.4).
func TestContentTools_CollectBodyBound(t *testing.T) {
	r := newContentRig(t, toolrunner.ContentConfig{MaxBody: 4096})
	if err := r.lab.Deploy(0); err != nil {
		t.Fatal(err)
	}
	type bounded struct {
		RawSize  int64  `json:"raw_size"`
		Coverage string `json:"coverage"`
	}
	got := decode[bounded](t, mustRun(t, r, 1, "collect", []any{r.lab.URL(lab.ListenerHTTPS, "shop.lab.test", "/products"), "default"}))
	if got.Coverage == "complete" || got.RawSize < 4096 || got.RawSize > 4096+2048 {
		t.Fatalf("bounded capture %+v", got)
	}
}
