package content_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/contentstore"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	"github.com/genuinebnt/flowforge/services/notifier"
	"github.com/genuinebnt/flowforge/testkit"
)

var t0 = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

func spa(t *testing.T, name string) []byte { return testkit.ReadFile(t, "content/sites/spa", name) }

func headers(t *testing.T, name string) []*pb.HttpHeader {
	var hs []*pb.HttpHeader
	for _, l := range strings.Split(strings.TrimSpace(string(spa(t, name))), "\n") {
		k, v, _ := strings.Cut(l, ":")
		hs = append(hs, &pb.HttpHeader{Name: strings.TrimSpace(k), Value: strings.TrimSpace(v)})
	}
	return hs
}

type version struct {
	url, file, headers, page string
	day                      int
}

var versions = []version{
	{"https://shop.example/", "v1.index.html", "v1.headers.txt", "", 0},
	{"https://shop.example/static/js/main.3f9a1c2b.js", "main.3f9a1c2b.js", "", "https://shop.example/", 0},
	{"https://shop.example/", "v2.index.html", "v2.headers.txt", "", 7},
	{"https://shop.example/static/js/main.77e0d41a.js", "main.77e0d41a.js", "", "https://shop.example/", 7},
	{"https://shop.example/", "v3.index.html", "v3.headers.txt", "", 14},
	{"https://shop.example/static/js/main.5b6c7d8e.js", "main.5b6c7d8e.js", "", "https://shop.example/", 14},
}

func (v version) request(t *testing.T, program string, minute int) *pb.IngestCaptureRequest {
	hs := []*pb.HttpHeader{{Name: "Content-Type", Value: "application/javascript"}}
	if v.headers != "" {
		hs = headers(t, v.headers)
	}
	return &pb.IngestCaptureRequest{ProgramId: program, RunId: core.PublicID(core.PrefixRun, core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000e1")), PageUrl: v.page, Capture: &pb.Capture{Url: v.url,
		Status: 200, Headers: hs, Body: spa(t, v.file), Outcome: "complete",
		FetchedAt: timestamppb.New(t0.AddDate(0, 0, v.day).Add(time.Duration(minute) * time.Minute))}}
}

// TestContentService_EndToEnd drives the content service in the dev
// stack: captures of the SPA fixture are ingested over gRPC; the graph
// receives their observations and content_changed events through the
// bus; the planted v3 change raises an alert (baselines and non-changes
// do not); diffs, search, pinning, quotas and tenant isolation work over
// the API; raw reads need an admin.
func TestContentService_EndToEnd(t *testing.T) {
	runEndToEnd(t, "")
}

// TestContentService_EndToEndPostgres is the same on Postgres (content
// schema, RLS per transaction, the content outbox and the Postgres-backed
// graph and notifier).
func TestContentService_EndToEndPostgres(t *testing.T) {
	if os.Getenv("FF_TEST_PG_DSN") == "" {
		t.Skip("FF_TEST_PG_DSN not set")
	}
	runEndToEnd(t, servicetest.FreshDB(t))
}

func runEndToEnd(t *testing.T, dsn string) {
	hook := servicetest.NewWebhook(t, "hook-secret")
	st, ctxA, ctxB := servicetest.Stack(t, devstack.Options{DBDSN: dsn, AllowInsecureWebhooks: true,
		Secrets: notifier.NewMapSecrets(map[string]string{"test://hook": "hook-secret"}), ContentLimits: contentstore.Limits{CapturesPerDay: 50}})
	prog, err := st.Scope.CreateProgram(ctxA, &pb.CreateProgramRequest{Name: "content", Platform: "test", InitialScope: servicetest.ScopeDoc("shop.example")})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := structpb.NewStruct(map[string]any{"url": hook.URL + "/hook", "secret_ref": "test://hook"})
	if _, err := st.Notifier.PutChannel(ctxA, &pb.PutChannelRequest{Kind: notifier.KindWebhook, Name: "c", Enabled: true, Config: cfg}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Notifier.PutRule(ctxA, &pb.PutRuleRequest{Name: "content", Enabled: true,
		Source: `alert content when change.type == "content_changed" -> webhook("c") severity = High`}); err != nil {
		t.Fatal(err)
	}
	var res []*pb.IngestCaptureResponse
	alertable := 0
	for i, v := range versions {
		r, err := st.Content.IngestCapture(ctxA, v.request(t, prog.GetId(), i))
		if err != nil {
			t.Fatalf("ingest %s: %v", v.url, err)
		}
		if r.GetChange().GetAlertable() {
			alertable++
		}
		res = append(res, r)
	}
	if alertable == 0 || res[0].GetChange().GetAlertable() {
		t.Fatalf("alertable changes %d (baseline alertable: %v)", alertable, res[0].GetChange().GetAlertable())
	}
	again, err := st.Content.IngestCapture(ctxA, versions[0].request(t, prog.GetId(), 0))
	if err != nil || !again.GetDuplicate() || again.GetSnapshot().GetId() != res[0].GetSnapshot().GetId() {
		t.Fatalf("re-ingest: %v %v", again, err)
	}
	if res[3].GetLineageParentUrl() != versions[1].url {
		t.Fatalf("lineage parent %q", res[3].GetLineageParentUrl())
	}

	// The graph: the page URL entity exists and carries content_changed.
	servicetest.Eventually(t, 20*time.Second, "graph content events", func() bool {
		n, tok := 0, ""
		for {
			d, err := st.Graph.Diff(ctxA, &pb.DiffRequest{ProgramId: prog.GetId(), From: &pb.DiffRequest_Since{Since: timestamppb.New(t0.AddDate(-1, 0, 0))},
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
		// Baselines of the page and the bundle, the v3 page and the edited
		// bundle; masked-only and reordered versions are no changes.
		return n == 4
	})
	es, err := st.Graph.FindEntities(ctxA, &pb.FindEntitiesRequest{ProgramId: prog.GetId(), AssetTypes: []string{"URL"}})
	if err != nil || len(es.GetEntities()) == 0 {
		t.Fatalf("graph URL entities: %v %v", es, err)
	}
	// The alert for the planted change (and nothing for the baselines).
	servicetest.Eventually(t, 20*time.Second, "content alert webhook", func() bool { return len(hook.Received()) > 0 })
	ae, err := st.Notifier.ListAlertEvents(ctxA, &pb.ListAlertEventsRequest{})
	if err != nil || len(ae.GetEvents()) == 0 || len(ae.GetEvents()) > alertable {
		t.Fatalf("alert events %d for %d alertable changes: %v", len(ae.GetEvents()), alertable, err)
	}

	// Diff views.
	d, err := st.Content.DiffSnapshots(ctxA, &pb.DiffSnapshotsRequest{ToSnapshotId: res[4].GetSnapshot().GetId()})
	if err != nil || d.GetFromSnapshotId() != res[2].GetSnapshot().GetId() || len(d.GetArtifacts()) == 0 {
		t.Fatalf("diff v2→v3: %v %v", d, err)
	}
	unified, side := false, false
	for _, a := range d.GetArtifacts() {
		unified = unified || strings.Contains(a.GetUnified(), "@@ ")
		var sbs struct {
			Hunks []json.RawMessage `json:"hunks"`
		}
		if json.Unmarshal([]byte(a.GetSideBySideJson()), &sbs) == nil && len(sbs.Hunks) > 0 {
			side = true
		}
	}
	if !unified || !side {
		t.Fatalf("diff views: unified %v side-by-side %v", unified, side)
	}
	base, err := st.Content.DiffSnapshots(ctxA, &pb.DiffSnapshotsRequest{ToSnapshotId: res[0].GetSnapshot().GetId(), Format: pb.DiffFormat_DIFF_FORMAT_UNIFIED})
	if err != nil || base.GetFromSnapshotId() != "" || !base.GetChange().GetBaseline() {
		t.Fatalf("baseline diff: %v %v", base, err)
	}

	// Search and first appeared.
	hits, err := st.Content.SearchChanges(ctxA, &pb.SearchChangesRequest{ProgramId: prog.GetId(), Query: "kind:endpoint_added admin"})
	if err != nil || hits.GetTotal() == 0 {
		t.Fatalf("search: %v %v", hits, err)
	}
	fa, err := st.Content.FirstAppeared(ctxA, &pb.FirstAppearedRequest{ProgramId: prog.GetId(), Text: "/account/delete"})
	if err != nil || !fa.GetFound() || fa.GetHit().GetSnapshotId() != res[4].GetSnapshot().GetId() {
		t.Fatalf("first appeared: %v %v", fa, err)
	}
	if _, err := st.Content.SearchChanges(ctxA, &pb.SearchChangesRequest{ProgramId: prog.GetId(), Query: "("}); !isKind(err, core.KindInvalidArgument) {
		t.Fatalf("bad query: %v", err)
	}

	// Content reads: redacted by default, raw for admins only.
	sn, err := st.Content.GetSnapshot(ctxA, &pb.GetSnapshotRequest{SnapshotId: res[4].GetSnapshot().GetId(), IncludeBody: true, IncludeFeatures: true})
	if err != nil || sn.GetBodyRaw() || !strings.Contains(string(sn.GetBody()), "[REDACTED:") || len(sn.GetFindings()) == 0 || sn.GetFeaturesJson() == "" {
		t.Fatalf("redacted read: %v", err)
	}
	analyst := st.As(context.Background(), servicetest.TenantA, servicetest.UserA, auth.RoleAnalyst)
	if _, err := st.Content.GetSnapshot(analyst, &pb.GetSnapshotRequest{SnapshotId: res[4].GetSnapshot().GetId(), Raw: true}); !isKind(err, core.KindPermissionDenied) {
		t.Fatalf("analyst raw read: %v", err)
	}
	raw, err := st.Content.GetSnapshot(ctxA, &pb.GetSnapshotRequest{SnapshotId: res[4].GetSnapshot().GetId(), Raw: true})
	if err != nil || !raw.GetBodyRaw() || string(raw.GetBody()) != string(spa(t, "v3.index.html")) {
		t.Fatalf("admin raw read: %v", err)
	}

	// Pinning, listing and quotas.
	p, err := st.Content.PinSnapshot(ctxA, &pb.PinSnapshotRequest{SnapshotId: res[2].GetSnapshot().GetId(), Reason: "reviewed"})
	if err != nil || !p.GetPinned() {
		t.Fatalf("pin: %v %v", p, err)
	}
	ls, err := st.Content.ListSnapshots(ctxA, &pb.ListSnapshotsRequest{ProgramId: prog.GetId(), Page: &pb.PageRequest{PageSize: 2}})
	if err != nil || len(ls.GetSnapshots()) != 2 || ls.GetPage().GetNextPageToken() == "" {
		t.Fatalf("list page 1: %v %v", ls, err)
	}
	ls2, err := st.Content.ListSnapshots(ctxA, &pb.ListSnapshotsRequest{ProgramId: prog.GetId(), Page: &pb.PageRequest{PageSize: 10, PageToken: ls.GetPage().GetNextPageToken()}})
	if err != nil || len(ls2.GetSnapshots()) != len(versions)-2 {
		t.Fatalf("list page 2: %d %v", len(ls2.GetSnapshots()), err)
	}
	q, err := st.Content.GetQuotaStatus(ctxA, &pb.GetQuotaStatusRequest{})
	if err != nil || q.GetCapturesToday() != int64(len(versions)) || q.GetCapturesPerDay() != 50 || q.GetStoredBytes() == 0 {
		t.Fatalf("quota: %v %v", q, err)
	}

	// Tenant B sees none of it.
	if _, err := st.Content.GetSnapshot(ctxB, &pb.GetSnapshotRequest{SnapshotId: res[0].GetSnapshot().GetId()}); !isKind(err, core.KindNotFound) {
		t.Fatalf("tenant B reads A's snapshot: %v", err)
	}
	hb, err := st.Content.SearchChanges(ctxB, &pb.SearchChangesRequest{ProgramId: prog.GetId(), Query: "admin"})
	if err != nil || hb.GetTotal() != 0 {
		t.Fatalf("tenant B search: %v %v", hb, err)
	}
}

func isKind(err error, k core.Kind) bool {
	var ce *core.Error
	return errors.As(platform.FromStatus(err), &ce) && ce.Kind == k
}
