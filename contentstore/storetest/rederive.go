package storetest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/contentstore"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// Re-derive (V2 §12.2) on top of an adapter: the Repo methods it uses,
// and the store invariants CSTORE-INV-14..16, which contentstore runs on
// MemRepo and contentstore/pg on Postgres.

// UpgradedStamp is the stamp of UpgradedPipeline.
const UpgradedStamp = "test/normalizers-v2"

// UpgradedPipeline is a stand-in for a normalizer upgrade: content.Process,
// then html@1 becomes html@2 (script start tags written in upper case)
// and js@1 becomes js@2 (spaces around every '='), each with a new rule
// table hash. It changes most normalized lines of the SPA fixture's
// bundles and some of its pages, and leaves the HTML attribute layout
// volatility learning reads intact.
func UpgradedPipeline() contentstore.Pipeline {
	return contentstore.Pipeline{Stamp: UpgradedStamp, Process: func(c *content.Capture, o content.ProcessOptions) (*content.Processed, error) {
		p, err := content.Process(c, o)
		if err != nil {
			return nil, err
		}
		for i := range p.Artifacts {
			a := &p.Artifacts[i]
			n := &a.Normalized
			switch a.Kind {
			case content.KindHTML:
				n.Text = bytes.ReplaceAll(n.Text, []byte("<script"), []byte("<SCRIPT"))
			case content.KindJS:
				n.Text = bytes.ReplaceAll(n.Text, []byte("="), []byte(" = "))
			default:
				continue
			}
			n.Version = strings.Replace(n.Version, "@1", "@2", 1)
			h := sha256.Sum256([]byte("v2:" + n.RuleTableSHA256))
			n.RuleTableSHA256 = hex.EncodeToString(h[:])
			t := sha256.Sum256(n.Text)
			a.NormalizedSHA256 = hex.EncodeToString(t[:])
		}
		return p, nil
	}}
}

// withPipeline returns a store over s's repository, chunks, keys and
// index with another pipeline (a process restarted after an upgrade).
func withPipeline(t *testing.T, s *contentstore.Store, p contentstore.Pipeline) *contentstore.Store {
	t.Helper()
	cfg := s.Config()
	cfg.Pipeline = p
	n, err := contentstore.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// aboutPage is a page whose first version loads a script and whose
// second does not: UpgradedPipeline changes the first's normalized text
// (html@2 upper-cases script start tags) and leaves the second's derived
// records as they were, so the diff between them must be recomputed when
// its new side is re-derived although that side's records do not change.
func aboutPage(v string, at time.Time) content.Capture {
	body := "<!doctype html><html><head><title>About</title></head><body><h1>About us</h1>\n" +
		"<p>We sell things.</p>\n<script src=\"/static/js/about.js\"></script>\n</body></html>\n"
	if v == "v2" {
		body = "<!doctype html><html><head><title>About</title></head><body><h1>About us</h1>\n" +
			"<p>We sell many things.</p>\n</body></html>\n"
	}
	return content.Capture{URL: "https://shop.example/about", Profile: content.DefaultProfile(), Status: 200,
		Headers: []content.Header{{Name: "Content-Type", Value: "text/html; charset=utf-8"}}, Body: []byte(body),
		FetchedAt: at, Outcome: content.OutcomeComplete}
}

// spaCaptures are the SPA fixture's captures in deploy order (as
// contentstore's own tests ingest them): pages, bundles, a renamed vite
// chunk (lineage) and an unrelated one, then two versions of aboutPage.
func spaCaptures(t *testing.T) []contentstore.IngestRequest {
	day := func(d int) time.Time { return t0.AddDate(0, 0, d) }
	js := func(name string, d int) contentstore.IngestRequest {
		return contentstore.IngestRequest{Program: programP, PageURL: "https://shop.example/", Capture: content.Capture{
			URL: "https://shop.example/static/js/" + name, Profile: content.DefaultProfile(), Status: 200,
			Headers: []content.Header{{Name: "Content-Type", Value: "application/javascript"}}, Body: spa(t, name),
			FetchedAt: day(d).Add(time.Minute), Outcome: content.OutcomeComplete}}
	}
	pg := func(v string, d int) contentstore.IngestRequest {
		return contentstore.IngestRequest{Program: programP, Capture: page(t, v, day(d))}
	}
	return []contentstore.IngestRequest{
		pg("v1", 0), js("main.3f9a1c2b.js", 0), js("index-a1b2c3d4.js", 0),
		pg("v2", 7), js("main.77e0d41a.js", 7),
		pg("v3", 14), js("main.5b6c7d8e.js", 14), js("app-e5f6a7b8.js", 14), js("other-0f0f0f0f.js", 14),
		{Program: programP, Capture: aboutPage("v1", day(15))}, {Program: programP, Capture: aboutPage("v2", day(16))},
	}
}

func ingestAll(t *testing.T, s *contentstore.Store, reqs []contentstore.IngestRequest) []contentstore.IngestResult {
	t.Helper()
	var out []contentstore.IngestResult
	for _, r := range reqs {
		res, err := s.Ingest(context.Background(), tenantA, r)
		if err != nil {
			t.Fatalf("ingest %s: %v", r.Capture.URL, err)
		}
		out = append(out, res)
	}
	return out
}

// storeState is everything a re-derive may touch, read through the
// store's public API: per snapshot its row, normalized text and features
// record; per diff its row, hunks and change set; the hits of a query
// set; the quota usage.
type storeState struct {
	snaps    []contentstore.Snapshot
	texts    []string
	features []string
	diffs    []contentstore.Diff
	hunks    []string
	changes  []string
	hits     []string
	captures int64
}

var rederiveQueries = []string{`kind:form_added`, `/admin\/export/`, `kind:endpoint_added admin`, `SCRIPT`, `fetch`,
	`side:added`, `side:removed`, `kind:hunk`, `cause:target`, `/ = /`}

func readState(t *testing.T, s *contentstore.Store, res []contentstore.IngestResult) storeState {
	t.Helper()
	ctx := context.Background()
	var st storeState
	for _, r := range res {
		c, err := s.SnapshotContent(ctx, tenantA, r.Snapshot.ID, contentstore.ContentOptions{Normalized: true, Features: true})
		if err != nil {
			t.Fatal(err)
		}
		fj, _ := core.CanonicalJSON(c.Processed)
		st.snaps, st.texts, st.features = append(st.snaps, c.Snapshot), append(st.texts, string(c.Normalized)), append(st.features, string(fj))
		if r.Diff == nil {
			continue
		}
		d, hunks, cs, err := s.StoredDiff(ctx, tenantA, r.Diff.ID)
		if err != nil {
			t.Fatal(err)
		}
		hj, _ := core.CanonicalJSON(hunks)
		cj, _ := core.CanonicalJSON(cs)
		st.diffs, st.hunks, st.changes = append(st.diffs, d), append(st.hunks, string(hj)), append(st.changes, string(cj))
	}
	for _, q := range rederiveQueries {
		r, err := s.Search(ctx, tenantA, programP, q, contentstore.SearchOptions{K: 1000, IncludeVendor: true})
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		// Hits as a set, without scores (BM25 statistics may still count
		// replaced documents until segments merge) and without diff ids
		// (a diff keeps its id through re-derivation; a fresh ingest under
		// the new normalizer derives another).
		var hs []string
		for _, h := range r.Hits {
			h.Score, h.DiffID = 0, ""
			b, _ := core.CanonicalJSON(h)
			hs = append(hs, string(b))
		}
		slices.Sort(hs)
		st.hits = append(st.hits, q+" "+strings.Join(hs, "\n"))
	}
	for _, text := range []string{"admin", "export", "fetch"} {
		if h, ok, err := s.FirstAppeared(ctx, tenantA, programP, text, contentstore.SearchOptions{IncludeVendor: true}); err != nil {
			t.Fatal(err)
		} else if ok {
			st.hits = append(st.hits, "first "+text+" "+h.SnapshotID+" "+h.Doc)
		}
	}
	q, err := s.Quota(ctx, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	st.captures = q.Usage.Captures
	return st
}

// rederiveAll calls Rederive with batch size n until nothing remains and
// returns the summed report and the number of calls.
func rederiveAll(t *testing.T, s *contentstore.Store, n int) (contentstore.RederiveReport, int) {
	t.Helper()
	var total contentstore.RederiveReport
	for calls := 1; ; calls++ {
		r, err := s.Rederive(context.Background(), tenantA, contentstore.RederiveOptions{BatchSize: n})
		if err != nil {
			t.Fatal(err)
		}
		total.Snapshots, total.Rewritten, total.Diffs, total.HunksVerbatim = total.Snapshots+r.Snapshots, total.Rewritten+r.Rewritten,
			total.Diffs+r.Diffs, total.HunksVerbatim+r.HunksVerbatim
		if !r.Remaining {
			return total, calls
		}
		if calls > 100 {
			t.Fatal("re-derive does not finish")
		}
	}
}

func sameStrings(t *testing.T, what string, a, b []string) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("%s: %d vs %d entries", what, len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("%s %d differs:\n%.600s\n---\n%.600s", what, i, a[i], b[i])
		}
	}
}

// RederiveIdempotent is CSTORE-INV-14: re-derive is resumable and
// idempotent. After a normalizer upgrade, re-deriving in batches of one
// (each call a fresh, interrupted-and-resumed job) and in one batch give
// the same state; every snapshot then carries the new stamp, and running
// the job again re-derives nothing and changes nothing. A stamp change
// with an unchanged pipeline (rows from before stamps) rewrites no record.
// Three jobs running at once reach the same state.
func RederiveIdempotent(t *testing.T, open func(t *testing.T) contentstore.Repo) {
	clock := testkit.NewFakeClock(t0.AddDate(0, 0, 30))
	reqs := spaCaptures(t)
	var states []storeState
	for _, batch := range []int{1, 64, 0} {
		s := harness(t, open(t), clock, contentstore.Limits{})
		res := ingestAll(t, s, reqs)
		up := withPipeline(t, s, UpgradedPipeline())
		if batch == 0 {
			var wg sync.WaitGroup
			for range 3 {
				wg.Go(func() {
					for {
						r, err := up.Rederive(context.Background(), tenantA, contentstore.RederiveOptions{BatchSize: 2})
						if err != nil {
							t.Error(err)
							return
						}
						if !r.Remaining {
							return
						}
					}
				})
			}
			wg.Wait()
			states = append(states, readState(t, up, res))
			continue
		}
		rep, calls := rederiveAll(t, up, batch)
		if rep.Snapshots != len(res) || rep.Rewritten == 0 || rep.Diffs == 0 || (batch == 1 && calls < len(res)) {
			t.Fatalf("batch %d: %+v in %d calls for %d snapshots", batch, rep, calls, len(res))
		}
		st := readState(t, up, res)
		for _, sn := range st.snaps {
			if sn.DeriveStamp != UpgradedStamp {
				t.Fatalf("%s still has stamp %q", sn.PublicID(), sn.DeriveStamp)
			}
		}
		again, err := up.Rederive(context.Background(), tenantA, contentstore.RederiveOptions{})
		if err != nil || again.Snapshots != 0 || again.Remaining {
			t.Fatalf("second run: %+v %v", again, err)
		}
		sameStrings(t, "state after a second run", stateLines(readState(t, up, res)), stateLines(st))
		states = append(states, st)

		if batch == 64 {
			// Same derivation, new stamp: only the stamps move.
			s3 := harness(t, open(t), clock, contentstore.Limits{})
			res3 := ingestAll(t, s3, reqs)
			same := withPipeline(t, s3, contentstore.Pipeline{Stamp: "test/renamed"})
			before := readState(t, same, res3)
			r, _ := rederiveAll(t, same, 64)
			if r.Snapshots != len(res3) || r.Rewritten != 0 || r.Diffs != 0 || r.HunksVerbatim != 0 {
				t.Fatalf("unchanged pipeline: %+v", r)
			}
			after := readState(t, same, res3)
			for i := range after.snaps {
				after.snaps[i].DeriveStamp = before.snaps[i].DeriveStamp
			}
			sameStrings(t, "state after an unchanged re-derive", stateLines(after), stateLines(before))
		}
	}
	sameStrings(t, "batch 1 vs batch 64", stateLines(states[0]), stateLines(states[1]))
	sameStrings(t, "one job vs three concurrent jobs", stateLines(states[1]), stateLines(states[2]))
}

// stateLines flattens a state for comparison (snapshot rows without the
// blob refs, whose encodings depend on the order the job ran in).
func stateLines(st storeState) []string {
	var out []string
	for i, sn := range st.snaps {
		sn.HeadersRef, sn.BodyManifest, sn.NormalizedManifest, sn.FeaturesRef, sn.StoredSize = "", "", "", "", 0
		b, _ := core.CanonicalJSON(sn)
		out = append(out, string(b), st.texts[i], st.features[i])
	}
	for i, d := range st.diffs {
		d.HunksRef, d.SemanticRef, d.StoredSize = "", "", 0
		b, _ := core.CanonicalJSON(d)
		out = append(out, string(b), st.hunks[i], st.changes[i])
	}
	return append(append(out, st.hits...), "captures "+strings.Repeat("|", int(st.captures)))
}

// spyRepo counts the writes a re-derive must never make.
type spyRepo struct {
	contentstore.Repo
	n map[string]int
}

func (r *spyRepo) InTx(ctx context.Context, tenant core.UUID, fn func(contentstore.Tx) error) error {
	return r.Repo.InTx(ctx, tenant, func(tx contentstore.Tx) error { return fn(&spyTx{Tx: tx, n: r.n}) })
}

type spyTx struct {
	contentstore.Tx
	n map[string]int
}

func (t *spyTx) InsertSnapshot(ctx context.Context, s contentstore.Snapshot) error {
	t.n["InsertSnapshot"]++
	return t.Tx.InsertSnapshot(ctx, s)
}

func (t *spyTx) InsertDiff(ctx context.Context, d contentstore.Diff) error {
	t.n["InsertDiff"]++
	return t.Tx.InsertDiff(ctx, d)
}

func (t *spyTx) NextDiffSeq(ctx context.Context, p core.UUID) (int64, error) {
	t.n["NextDiffSeq"]++
	return t.Tx.NextDiffSeq(ctx, p)
}

func (t *spyTx) CloseVersion(ctx context.Context, id core.UUID, at time.Time) error {
	t.n["CloseVersion"]++
	return t.Tx.CloseVersion(ctx, id, at)
}

func (t *spyTx) Touch(ctx context.Context, id core.UUID, at time.Time) error {
	t.n["Touch"]++
	return t.Tx.Touch(ctx, id, at)
}

func (t *spyTx) InsertFindings(ctx context.Context, fs []contentstore.Finding) error {
	t.n["InsertFindings"]++
	return t.Tx.InsertFindings(ctx, fs)
}

func (t *spyTx) AppendOutbox(ctx context.Context, e contentstore.OutboxEvent) error {
	t.n["AppendOutbox"]++
	return t.Tx.AppendOutbox(ctx, e)
}

func (t *spyTx) PutVolatility(ctx context.Context, rows []contentstore.VolatilityRow) error {
	t.n["PutVolatility"]++
	return t.Tx.PutVolatility(ctx, rows)
}

func (t *spyTx) DeleteSnapshots(ctx context.Context, ids []core.UUID) error {
	t.n["DeleteSnapshots"]++
	return t.Tx.DeleteSnapshots(ctx, ids)
}

// RederiveNoChange is CSTORE-INV-15: re-derivation is not a change. The
// job inserts no snapshot, diff, finding or outbox event, allocates no
// diff sequence number, closes or touches no version and learns no
// volatility; every snapshot and diff keeps its id, sides, validity,
// lineage and sequence number and records the new normalizer_version;
// no capture is counted. An ingest after the upgrade re-derives its
// predecessor first, so its change is computed between snapshots of the
// new derivation (a revert to page v2 is a target change whose hunks
// carry no normalizer difference).
func RederiveNoChange(t *testing.T, open func(t *testing.T) contentstore.Repo) {
	ctx := context.Background()
	clock := testkit.NewFakeClock(t0.AddDate(0, 0, 30))
	spy := &spyRepo{Repo: open(t), n: map[string]int{}}
	s := harness(t, spy, clock, contentstore.Limits{})
	res := ingestAll(t, s, spaCaptures(t))
	before := readState(t, s, res)
	up := withPipeline(t, s, UpgradedPipeline())
	clear(spy.n)
	rederiveAll(t, up, 3)
	if len(spy.n) != 0 {
		t.Fatalf("re-derive wrote %v", spy.n)
	}
	after := readState(t, up, res)
	if after.captures != before.captures || len(after.snaps) != len(before.snaps) || len(after.diffs) != len(before.diffs) {
		t.Fatalf("captures %d → %d, snapshots %d → %d, diffs %d → %d", before.captures, after.captures,
			len(before.snaps), len(after.snaps), len(before.diffs), len(after.diffs))
	}
	for i, a := range after.snaps {
		b := before.snaps[i]
		if a.ID != b.ID || !a.ValidFrom.Equal(b.ValidFrom) || !a.ValidTo.Equal(b.ValidTo) || !a.LastSeen.Equal(b.LastSeen) ||
			a.ParentID != b.ParentID || a.LineageParentID != b.LineageParentID || a.Signature != b.Signature || a.Degraded != b.Degraded {
			t.Fatalf("snapshot %d identity or validity moved", i)
		}
		if strings.Contains(b.NormalizerVersion, "@1") && (b.Kind == "html" || b.Kind == "js") &&
			a.NormalizerVersion != strings.Replace(b.NormalizerVersion, "@1", "@2", 1) {
			t.Fatalf("snapshot %d records %q", i, a.NormalizerVersion)
		}
	}
	for i, a := range after.diffs {
		b := before.diffs[i]
		if a.ID != b.ID || a.Seq != b.Seq || a.From != b.From || a.To != b.To || !a.At.Equal(b.At) {
			t.Fatalf("diff %d identity moved", i)
		}
	}
	if after.texts[1] == before.texts[1] {
		t.Fatal("the bundle's normalized text did not change")
	}

	// Ingest under the new pipeline without running the job first.
	s2 := harness(t, open(t), clock, contentstore.Limits{})
	res2 := ingestAll(t, s2, spaCaptures(t))
	up2 := withPipeline(t, s2, UpgradedPipeline())
	revert := contentstore.IngestRequest{Program: programP, Capture: page(t, "v2", t0.AddDate(0, 0, 21))}
	r, err := up2.Ingest(ctx, tenantA, revert)
	if err != nil || r.Diff == nil || r.ChangeSet == nil {
		t.Fatalf("ingest after the upgrade: %v", err)
	}
	if r.ChangeSet.Cause != "target" || r.Diff.NormalizerVersion != "html@2" {
		t.Fatalf("revert: cause %s, normalizer %s", r.ChangeSet.Cause, r.Diff.NormalizerVersion)
	}
	pred, err := up2.GetSnapshot(ctx, tenantA, res2[5].Snapshot.ID)
	if err != nil || pred.DeriveStamp != UpgradedStamp {
		t.Fatalf("predecessor not re-derived first: %q %v", pred.DeriveStamp, err)
	}
	_, hunks, _, err := up2.StoredDiff(ctx, tenantA, r.Diff.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range hunks {
		for _, h := range a.Hunks {
			for _, l := range h.Lines {
				// html@2 writes no lower-case script tag: one on either
				// side would be the old derivation.
				if strings.Contains(l, "<script") {
					t.Fatalf("revert diff mixes derivations: %q", l)
				}
			}
		}
	}
}

// RederiveEqualsFresh is CSTORE-INV-16: after re-derivation, the store
// equals one that ingested the same captures under the new pipeline from
// the start: every snapshot's normalized text, features record, kind,
// coverage, simhash and normalizer_version; every diff's levels, stats,
// hunks and change set; and the search and first-appeared answers of a
// query set.
func RederiveEqualsFresh(t *testing.T, open func(t *testing.T) contentstore.Repo) {
	clock := testkit.NewFakeClock(t0.AddDate(0, 0, 30))
	reqs := spaCaptures(t)
	old := harness(t, open(t), clock, contentstore.Limits{})
	res := ingestAll(t, old, reqs)
	up := withPipeline(t, old, UpgradedPipeline())
	rederiveAll(t, up, 4)
	got := readState(t, up, res)

	fresh := harness(t, open(t), clock, contentstore.Limits{})
	fresh = withPipeline(t, fresh, UpgradedPipeline())
	fres := ingestAll(t, fresh, reqs)
	want := readState(t, fresh, fres)
	for i := range got.diffs {
		// The diff id hashes the normalizer version at ingest (kept by
		// re-derivation); everything else must agree.
		got.diffs[i].ID, want.diffs[i].ID = core.Nil, core.Nil
		got.diffs[i].CreatedAt, want.diffs[i].CreatedAt = time.Time{}, time.Time{}
	}
	for i := range got.snaps {
		got.snaps[i].CreatedAt, want.snaps[i].CreatedAt = time.Time{}, time.Time{}
	}
	sameStrings(t, "re-derived vs fresh", stateLines(got), stateLines(want))
}

// rederiveRepo checks the Repo methods re-derivation uses: StaleSnapshots
// (filter, order, cursor, limit), LockSnapshot, UpdateDerived (derived
// columns only) and RewriteDiff (derived columns only).
func rederiveRepo(t *testing.T, r contentstore.Repo) {
	ctx := context.Background()
	var all []contentstore.Snapshot
	inTx(t, r, tenantA, func(tx contentstore.Tx) error {
		if err := tx.EnsureProgram(ctx, programP); err != nil {
			return err
		}
		for i := range 6 {
			s := snap(10+i, "https://h.example/"+string(rune('a'+i)), t0.Add(time.Duration(i%3)*time.Hour), false)
			if i%2 == 0 {
				s.DeriveStamp = "new"
			}
			all = append(all, s)
			if err := tx.InsertSnapshot(ctx, s); err != nil {
				return err
			}
		}
		return nil
	})
	var stale []contentstore.Snapshot
	for _, s := range all {
		if s.DeriveStamp != "new" {
			stale = append(stale, s)
		}
	}
	slices.SortFunc(stale, func(a, b contentstore.Snapshot) int {
		if c := a.ValidFrom.Compare(b.ValidFrom); c != 0 {
			return c
		}
		return bytes.Compare(a.ID[:], b.ID[:])
	})
	inTx(t, r, tenantA, func(tx contentstore.Tx) error {
		got, err := tx.StaleSnapshots(ctx, programP, "new", nil, 2)
		if err != nil || len(got) != 2 || got[0].ID != stale[0].ID || got[1].ID != stale[1].ID {
			t.Fatalf("first page %v %v", ids(got), err)
		}
		rest, err := tx.StaleSnapshots(ctx, programP, "new", &contentstore.Cursor{ValidFrom: got[1].ValidFrom, ID: got[1].ID}, 10)
		if err != nil || len(rest) != 1 || rest[0].ID != stale[2].ID {
			t.Fatalf("second page %v %v", ids(rest), err)
		}
		if other, _ := tx.StaleSnapshots(ctx, programQ, "new", nil, 10); len(other) != 0 {
			t.Fatalf("another program's rows: %v", ids(other))
		}
		s, ok, err := tx.LockSnapshot(ctx, stale[0].ID)
		if err != nil || !ok {
			t.Fatalf("lock %v %v", ok, err)
		}
		u := s
		u.NormalizedManifest, u.FeaturesRef, u.Kind, u.NormalizerVersion, u.Coverage, u.SimHash, u.DeriveStamp =
			strings.Repeat("e", 64), strings.Repeat("f", 64), "js", "js@2", "partial", 77, "new"
		u.URL, u.Status = "https://ignored.example/", 500 // not derived columns: unchanged
		if err := tx.UpdateDerived(ctx, u); err != nil {
			return err
		}
		got2, _, _ := tx.GetSnapshot(ctx, s.ID)
		want := s
		want.NormalizedManifest, want.FeaturesRef, want.Kind, want.NormalizerVersion, want.Coverage, want.SimHash, want.DeriveStamp =
			u.NormalizedManifest, u.FeaturesRef, u.Kind, u.NormalizerVersion, u.Coverage, u.SimHash, u.DeriveStamp
		a, _ := core.CanonicalJSON(got2)
		b, _ := core.CanonicalJSON(want)
		if !bytes.Equal(a, b) {
			t.Fatalf("UpdateDerived:\n got %s\nwant %s", a, b)
		}
		d := contentstore.Diff{ID: core.MustParseUUID("00000000-0000-8000-8000-0000000000d1"), Tenant: tenantA, Program: programP, Seq: 1,
			To: s.ID, Levels: []string{"hash"}, Algorithm: "size/none", NormalizerVersion: "html@1", Stats: contentstore.DiffStats{Cause: "target"},
			HunksRef: strings.Repeat("1", 64), SemanticRef: strings.Repeat("2", 64), At: t0, URL: s.URL, Profile: s.Profile, CreatedAt: t0}
		if err := tx.InsertDiff(ctx, d); err != nil {
			return err
		}
		n := d
		n.Levels, n.Algorithm, n.NormalizerVersion, n.Stats = []string{"hash", "text"}, "size/myers", "js@2", contentstore.DiffStats{Cause: "target", Inserted: 3}
		n.HunksRef, n.SemanticRef, n.StoredSize, n.IndexDocs = strings.Repeat("3", 64), strings.Repeat("4", 64), 9, 2
		n.Seq, n.URL = 99, "https://ignored.example/" // not derived columns: unchanged
		if err := tx.RewriteDiff(ctx, n); err != nil {
			return err
		}
		gd, _, _ := tx.GetDiff(ctx, d.ID)
		wd := n
		wd.Seq, wd.URL = d.Seq, d.URL
		a, _ = core.CanonicalJSON(gd)
		b, _ = core.CanonicalJSON(wd)
		if !bytes.Equal(a, b) {
			t.Fatalf("RewriteDiff:\n got %s\nwant %s", a, b)
		}
		return nil
	})
}

func ids(ss []contentstore.Snapshot) []string {
	var out []string
	for _, s := range ss {
		out = append(out, s.PublicID())
	}
	return out
}
