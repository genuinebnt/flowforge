package contentstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/genuinebnt/flowforge/chunkstore"
	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/testkit"
)

var bg = context.Background()

// ingestAll ingests the SPA fixture's versions and returns the results.
func (h *harness) ingestSPA(tenant core.UUID) []IngestResult {
	h.t.Helper()
	var out []IngestResult
	for _, v := range spaVersions(h.t) {
		out = append(out, h.ingest(tenant, v.C, v.Page))
	}
	return out
}

func (h *harness) snapshots(tenant core.UUID) []Snapshot {
	h.t.Helper()
	ss, err := h.store.ListSnapshots(bg, tenant, SnapshotFilter{Program: programP, Limit: 1000})
	if err != nil {
		h.t.Fatal(err)
	}
	return ss
}

func (h *harness) usage(tenant core.UUID) Usage {
	h.t.Helper()
	q, err := h.store.Quota(bg, tenant)
	if err != nil {
		h.t.Fatal(err)
	}
	return q.Usage
}

// CSTORE-INV-01: ingest is idempotent. The same capture ingested again
// (sequentially or concurrently) is one snapshot: no new row, diff, finding,
// outbox event, index document or quota count; the result names the stored
// snapshot.
func TestCStoreInv01_IngestIdempotent(t *testing.T) {
	h := newHarness(t)
	first := h.ingestSPA(tenantA)
	snaps, events, u := len(h.snapshots(tenantA)), len(h.mem.OutboxEvents()), h.usage(tenantA)
	_, diffs, findings, _, _ := h.mem.Dump(tenantA)
	idx, err := h.store.Search(bg, tenantA, programP, "side:added OR side:removed", SearchOptions{K: 1000, IncludeVendor: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range spaVersions(t) {
		r := h.ingest(tenantA, v.C, v.Page)
		if !r.Duplicate || r.Snapshot.ID != first[i].Snapshot.ID {
			t.Fatalf("re-ingest %d (%s): duplicate=%v id=%s, want the stored %s", i, v.C.URL, r.Duplicate, r.Snapshot.PublicID(), first[i].Snapshot.PublicID())
		}
	}
	_, diffs2, findings2, _, _ := h.mem.Dump(tenantA)
	idx2, err := h.store.Search(bg, tenantA, programP, "side:added OR side:removed", SearchOptions{K: 1000, IncludeVendor: true})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(h.snapshots(tenantA)); n != snaps || len(diffs2) != len(diffs) || len(findings2) != len(findings) ||
		len(h.mem.OutboxEvents()) != events || h.usage(tenantA) != u || idx2.Total != idx.Total {
		t.Fatalf("re-ingest changed state: snapshots %d→%d diffs %d→%d findings %d→%d events %d→%d usage %+v→%+v docs %d→%d",
			snaps, n, len(diffs), len(diffs2), len(findings), len(findings2), events, len(h.mem.OutboxEvents()), u, h.usage(tenantA), idx.Total, idx2.Total)
	}

	// Concurrent ingests of one capture: exactly one stores it.
	h2 := newHarness(t)
	c := spaVersions(t)[0].C
	var wg sync.WaitGroup
	var mu sync.Mutex
	stored, dups := 0, 0
	ids := map[core.UUID]bool{}
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := h2.store.Ingest(bg, tenantA, IngestRequest{Program: programP, Capture: c})
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			ids[r.Snapshot.ID] = true
			if r.Duplicate {
				dups++
			} else {
				stored++
			}
		}()
	}
	wg.Wait()
	if stored != 1 || dups != 7 || len(ids) != 1 || len(h2.snapshots(tenantA)) != 1 || h2.usage(tenantA).Captures != 1 {
		t.Fatalf("concurrent: stored %d duplicates %d ids %d snapshots %d captures %d, want 1/7/1/1/1",
			stored, dups, len(ids), len(h2.snapshots(tenantA)), h2.usage(tenantA).Captures)
	}
}

// CSTORE-INV-02: tenants are isolated. Identical content stored by two
// tenants is paid for by each (no cross-tenant dedupe: disjoint chunk
// namespaces, equal stored bytes); one tenant can neither read, diff, pin
// nor find (search, first appeared) the other's records; ids, keyed hashes
// and data keys differ.
func TestCStoreInv02_TenantIsolation(t *testing.T) {
	h := newHarness(t)
	ra := h.ingestSPA(tenantA)
	rb := h.ingestSPA(tenantB)
	alone := newHarness(t)
	alone.ingestSPA(tenantB)
	ma, _ := h.chunks.Metrics(bg, tenantA)
	mb, _ := h.chunks.Metrics(bg, tenantB)
	mAlone, _ := alone.chunks.Metrics(bg, tenantB)
	// Keyed hashes make the two tenants' features records differ by a few
	// bytes, so compare chunk counts: B dedupes exactly as much next to A
	// as alone, and A's identical bodies saved B nothing.
	if ma.CompressedBytes == 0 || mb.NewChunks != mAlone.NewChunks || mb.DedupChunks != mAlone.DedupChunks || mb.Chunks != mAlone.Chunks {
		t.Fatalf("each tenant pays for its own chunks (no dedupe against A): B next to A %+v, B alone %+v", mb, mAlone)
	}
	objsA, _ := h.backend.List(bg, "t/"+tenantA.String()+"/")
	objsB, _ := h.backend.List(bg, "t/"+tenantB.String()+"/")
	all, _ := h.backend.List(bg, "")
	if len(objsA) == 0 || len(objsB) == 0 || len(objsA)+len(objsB) != len(all) {
		t.Fatalf("chunk objects not namespaced per tenant: A %d B %d all %d", len(objsA), len(objsB), len(all))
	}
	for i := range ra {
		if ra[i].Snapshot.ID == rb[i].Snapshot.ID {
			t.Fatalf("snapshot ids collide across tenants: %s", ra[i].Snapshot.PublicID())
		}
		id := ra[i].Snapshot.ID
		if _, err := h.store.GetSnapshot(bg, tenantB, id); !errors.Is(err, ErrNotFound) {
			t.Fatalf("B reads A's snapshot: %v", err)
		}
		if _, err := h.store.SnapshotContent(bg, tenantB, id, ContentOptions{Body: true, Raw: true}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("B reads A's content: %v", err)
		}
		if _, err := h.store.Pin(bg, tenantB, id, "x"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("B pins A's snapshot: %v", err)
		}
		if ra[i].Diff != nil && !ra[i].Diff.From.IsZero() {
			if _, err := h.store.Diff(bg, tenantB, ra[i].Diff.From, id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("B diffs A's snapshots: %v", err)
			}
		}
	}
	// A marker only tenant B stores is invisible to A.
	marker := capture("https://shop.example/only-b", []byte("<html><body><p>zebracrossingmarker</p></body></html>"),
		[]content.Header{{Name: "Content-Type", Value: "text/html"}}, t0.AddDate(0, 0, 20))
	h.ingest(tenantB, marker, "")
	for _, tc := range []struct {
		tenant core.UUID
		want   int
	}{{tenantA, 0}, {tenantB, 1}} {
		res, err := h.store.Search(bg, tc.tenant, programP, "zebracrossingmarker", SearchOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_, found, err := h.store.FirstAppeared(bg, tc.tenant, programP, "zebracrossingmarker", SearchOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if (res.Total > 0) != (tc.want > 0) || found != (tc.want > 0) {
			t.Fatalf("tenant %s: search total %d, first appeared %v; want %d", tc.tenant, res.Total, found, tc.want)
		}
	}
	// Keyed hashes of the same secret differ (per-tenant hash keys); one
	// tenant's data key cannot open the other's ciphertext.
	_, _, fa, _, ka := h.mem.Dump(tenantA)
	_, _, fb, _, kb := h.mem.Dump(tenantB)
	if len(fa) == 0 || len(fa) != len(fb) {
		t.Fatalf("findings A %d B %d", len(fa), len(fb))
	}
	hashes := map[string]bool{}
	for _, f := range fa {
		hashes[f.KeyedHash] = true
	}
	for _, f := range fb {
		if hashes[f.KeyedHash] {
			t.Fatalf("keyed hash %s shared across tenants", f.KeyedHash)
		}
	}
	for _, k := range ka {
		for _, l := range kb {
			if bytes.Equal(k.Wrapped, l.Wrapped) {
				t.Fatal("wrapped data keys shared across tenants")
			}
		}
	}
	f := fa[0]
	if _, err := h.store.cfg.Keys.decrypt(bg, tenantB, f.KeyID, findingAAD(tenantB, f.Snapshot, f.ID), f.Ciphertext); err == nil {
		t.Fatal("tenant B's key opened tenant A's finding")
	}
	if _, err := h.store.cfg.Keys.decrypt(bg, tenantA, f.KeyID, findingAAD(tenantA, f.Snapshot, f.ID), f.Ciphertext); err != nil {
		t.Fatalf("tenant A cannot open its own finding: %v", err)
	}
}

// CSTORE-INV-03: retention keeps exactly the union of the newest KeepLast
// snapshots and those fetched within KeepFor per key, plus every pinned
// snapshot and each key's newest, never anything else; diffs follow their
// snapshots; every survivor still reads back byte-exactly (delta chains
// are re-keyframed before a base goes); chunk GC reclaims the deleted
// blobs within the tenant.
func TestCStoreInv03_RetentionKeepsPinnedAndNewest(t *testing.T) {
	r := testkit.Rng(t)
	// The pure rule, on random histories.
	for range testkit.Cases() {
		now := testkit.NewFakeClock(t0.AddDate(1, 0, 0))
		p := RetentionPolicy{KeepLast: 1 + core.IntN(r, 6), KeepFor: time.Duration(1+core.IntN(r, 30)) * 24 * time.Hour}
		var snaps []Snapshot
		at := t0
		for i := range core.IntN(r, 15) + 1 {
			at = at.Add(time.Duration(1+core.IntN(r, 60*24)) * time.Hour)
			s := Snapshot{ID: derivedUUID("t", fmt.Sprint(i)), FetchedAt: at, ValidFrom: at, ValidTo: at.Add(time.Hour), Pinned: core.IntN(r, 4) == 0}
			snaps = append(snaps, s)
		}
		if core.IntN(r, 3) > 0 {
			snaps[len(snaps)-1].ValidTo = time.Time{}
		}
		del := keep(snaps, p, now)
		gone := map[core.UUID]bool{}
		for _, d := range del {
			gone[d.ID] = true
		}
		sorted := slices.Clone(snaps)
		slices.SortFunc(sorted, snapshotOrder)
		for i, s := range sorted {
			want := i < p.KeepLast || now.Now().Sub(s.FetchedAt) <= p.KeepFor || s.Pinned || i == 0 || s.Open()
			if want == gone[s.ID] {
				t.Fatalf("rank %d pinned=%v age=%v policy %+v: deleted=%v", i, s.Pinned, now.Now().Sub(s.FetchedAt), p, gone[s.ID])
			}
		}
	}
	// The store, on random version histories of one page with pins.
	for iter := range max(6, testkit.Cases()/150) {
		h := newHarness(t, func(c *Config) {
			c.Retention = RetentionPolicy{KeepLast: 3 + core.IntN(r, 4), KeepFor: 10 * 24 * time.Hour, DisableOnIngest: true}
			c.KeyframeInterval = 2 + core.IntN(r, 5)
		})
		at := t0
		var all []IngestResult
		for i := range 6 + core.IntN(r, 10) {
			at = at.Add(time.Duration(1+core.IntN(r, 5*24)) * time.Hour)
			body := fmt.Sprintf("<html><body><p>version %d</p><a href=\"/p%d\">x</a><p>%s</p></body></html>", i, i, token(r, 200, alnum))
			res := h.ingest(tenantA, capture("https://keep.example/", []byte(body), []content.Header{{Name: "Content-Type", Value: "text/html"}}, at), "")
			all = append(all, res)
			if core.IntN(r, 5) == 0 {
				if _, err := h.store.Pin(bg, tenantA, res.Snapshot.ID, "reviewed"); err != nil {
					t.Fatal(err)
				}
			}
		}
		h.clock.SetWall(at.Add(time.Duration(core.IntN(r, 40*24)) * time.Hour))
		before := h.snapshots(tenantA)
		mBefore, _ := h.chunks.Metrics(bg, tenantA)
		if _, err := h.store.ApplyRetention(bg, tenantA); err != nil {
			t.Fatal(err)
		}
		after := h.snapshots(tenantA)
		gone := map[core.UUID]bool{}
		for _, d := range keep(before, h.store.cfg.Retention, h.clock) {
			gone[d.ID] = true
		}
		kept := map[core.UUID]bool{}
		for _, s := range after {
			kept[s.ID] = true
		}
		for i, s := range before {
			if gone[s.ID] == kept[s.ID] {
				t.Fatalf("iter %d: snapshot %d pinned=%v kept=%v, rule deletes=%v", iter, i, s.Pinned, kept[s.ID], gone[s.ID])
			}
			if (s.Pinned || i == 0) && !kept[s.ID] {
				t.Fatalf("iter %d: pinned or newest snapshot deleted", iter)
			}
		}
		_, diffs, _, _, _ := h.mem.Dump(tenantA)
		for _, d := range diffs {
			if !kept[d.To] || !d.From.IsZero() && !kept[d.From] {
				t.Fatalf("iter %d: diff %s outlived a snapshot", iter, d.PublicID())
			}
		}
		h.store.cache = newBlobCache(h.store.cfg.CacheBytes) // read from the chunk store, not the cache
		for _, res := range all {
			if !kept[res.Snapshot.ID] {
				continue
			}
			sc, err := h.store.SnapshotContent(bg, tenantA, res.Snapshot.ID, ContentOptions{Raw: true, Normalized: true, Features: true})
			if err != nil {
				t.Fatalf("iter %d: survivor unreadable after retention: %v", iter, err)
			}
			if sha256Hex(sc.Body) != res.Snapshot.BodySHA256 {
				t.Fatalf("iter %d: survivor body changed", iter)
			}
		}
		// chunkstore's GC grace keeps blobs put since the previous GC
		// began; the next GC reclaims the deleted snapshots' chunks.
		if _, err := h.store.GC(bg, tenantA); err != nil {
			t.Fatal(err)
		}
		mAfter, _ := h.chunks.Metrics(bg, tenantA)
		if len(gone) > 0 && mAfter.CompressedBytes >= mBefore.CompressedBytes {
			t.Fatalf("iter %d: GC reclaimed nothing (%d → %d bytes) after deleting %d snapshots", iter, mBefore.CompressedBytes, mAfter.CompressedBytes, len(gone))
		}
		if h.usage(tenantA).StoredBytes != int64(mAfter.CompressedBytes) {
			t.Fatalf("iter %d: stored-bytes usage %d != chunk bytes %d after GC", iter, h.usage(tenantA).StoredBytes, mAfter.CompressedBytes)
		}
	}
}

func canonical(t testing.TB, v any) []byte {
	t.Helper()
	b, err := core.CanonicalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// CSTORE-INV-04: diffs are deterministic. Two independent stores fed the
// same captures produce identical snapshot ids, diff ids, blob manifests
// and change sets; Diff(a, b) is byte-identical on every call; and the
// stored hunks and semantic changes of every diff equal a recomputation
// from the stored snapshots.
func TestCStoreInv04_DiffDeterminism(t *testing.T) {
	h1, h2 := newHarness(t), newHarness(t)
	r1, r2 := h1.ingestSPA(tenantA), h2.ingestSPA(tenantA)
	for i := range r1 {
		a, b := r1[i], r2[i]
		if a.Snapshot.ID != b.Snapshot.ID || a.Snapshot.BodyManifest != b.Snapshot.BodyManifest || a.Snapshot.FeaturesRef != b.Snapshot.FeaturesRef ||
			a.Snapshot.NormalizedManifest != b.Snapshot.NormalizedManifest || a.Snapshot.HeadersRef != b.Snapshot.HeadersRef {
			t.Fatalf("version %d: snapshots differ between stores", i)
		}
		if (a.Diff == nil) != (b.Diff == nil) || a.Diff != nil && (a.Diff.ID != b.Diff.ID || a.Diff.HunksRef != b.Diff.HunksRef || a.Diff.SemanticRef != b.Diff.SemanticRef) {
			t.Fatalf("version %d: diffs differ between stores", i)
		}
		if !bytes.Equal(canonical(t, a.ChangeSet), canonical(t, b.ChangeSet)) {
			t.Fatalf("version %d: change sets differ", i)
		}
	}
	for i, res := range r1 {
		d := res.Diff
		if d == nil {
			continue
		}
		var view DiffView
		var err error
		if d.From.IsZero() {
			view, err = h1.store.BaselineDiff(bg, tenantA, d.To)
		} else {
			view, err = h1.store.Diff(bg, tenantA, d.From, d.To)
		}
		if err != nil {
			t.Fatal(err)
		}
		again, _ := h1.store.Diff(bg, tenantA, d.From, d.To)
		if !d.From.IsZero() && !bytes.Equal(canonical(t, view.ChangeSet), canonical(t, again.ChangeSet)) {
			t.Fatalf("version %d: Diff is not deterministic", i)
		}
		_, stored, sem, err := h1.store.StoredDiff(bg, tenantA, d.ID)
		if err != nil {
			t.Fatal(err)
		}
		recomputed := []ArtifactHunks{}
		for _, a := range view.ChangeSet.Artifacts {
			if len(a.Hunks) > 0 {
				recomputed = append(recomputed, ArtifactHunks{DocID: a.DocID, Kind: a.Kind, Hunks: a.Hunks, Vendor: vendored(a)})
			}
		}
		if !bytes.Equal(canonical(t, stored), canonical(t, recomputed)) {
			t.Fatalf("version %d (%s): stored hunks differ from the recomputation", i, res.Snapshot.URL)
		}
		if !bytes.Equal(canonical(t, sem.Changes), canonical(t, view.ChangeSet.Changes)) || sem.Outcome != view.ChangeSet.Outcome {
			t.Fatalf("version %d: stored semantic changes differ from the recomputation", i)
		}
		if !bytes.Equal(canonical(t, sem.Changes), canonical(t, res.ChangeSet.Changes)) {
			t.Fatalf("version %d: stored changes differ from the ingest's change set", i)
		}
	}
}

// indexDump returns every document of a program's index, keyed by id.
func indexDump(t testing.TB, h *harness, tenant core.UUID) map[uint64][]byte {
	t.Helper()
	pi, err := h.index.get(tenant, programP)
	if err != nil {
		t.Fatal(err)
	}
	_, diffs, _, _, _ := h.mem.Dump(tenant)
	out := map[uint64][]byte{}
	for _, d := range diffs {
		for i := range d.IndexDocs {
			doc, ok, err := pi.ix.Get(docID(d.Seq, i))
			if err != nil || !ok {
				t.Fatalf("doc %d/%d of diff %s missing: %v", d.Seq, i, d.PublicID(), err)
			}
			out[doc.ID] = canonical(t, doc)
		}
	}
	return out
}

var invQueries = []string{`kind:endpoint_added`, `admin`, `/admin\/export/`, `side:removed`, `"api v2"`, `fetch -side:removed`,
	`kind:hunk side:added since:2026-11-01`, `vendor:1`, `feature:header`, `class:semantic_endpoint`}

func searchAll(t testing.TB, h *harness, tenant core.UUID) []byte {
	t.Helper()
	var all []SearchResult
	for _, q := range invQueries {
		res, err := h.store.Search(bg, tenant, programP, q, SearchOptions{K: 1000})
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		all = append(all, res)
	}
	return canonical(t, all)
}

// CSTORE-INV-05: the incremental index equals a rebuild from the stored
// diffs: the same documents under the same ids, and the same results for
// a query set; batched indexing ends in the same index; re-indexing above
// the watermark changes nothing.
func TestCStoreInv05_RebuildEqualsIncremental(t *testing.T) {
	inline := newHarness(t)
	inline.ingestSPA(tenantA)
	docs, results := indexDump(t, inline, tenantA), searchAll(t, inline, tenantA)
	if len(docs) == 0 {
		t.Fatal("no documents indexed")
	}
	rep, err := inline.store.IndexPending(bg, tenantA, programP)
	if err != nil || rep.Diffs != 0 {
		t.Fatalf("re-index above the watermark: %+v %v", rep, err)
	}
	if _, err := inline.store.Rebuild(bg, tenantA, programP); err != nil {
		t.Fatal(err)
	}
	if !mapsEqual(docs, indexDump(t, inline, tenantA)) || !bytes.Equal(results, searchAll(t, inline, tenantA)) {
		t.Fatal("rebuilt index differs from the incremental one")
	}
	batched := newHarness(t, func(c *Config) { c.Index.Mode = IndexBatched })
	for i, v := range spaVersions(t) {
		batched.ingest(tenantA, v.C, v.Page)
		if i%3 == 2 {
			if _, err := batched.store.IndexPending(bg, tenantA, programP); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := batched.store.IndexPending(bg, tenantA, programP); err != nil {
		t.Fatal(err)
	}
	if !mapsEqual(docs, indexDump(t, batched, tenantA)) || !bytes.Equal(results, searchAll(t, batched, tenantA)) {
		t.Fatal("batched index differs from the inline one")
	}
	// A lost index (fresh memory directory) is rebuilt from the watermark.
	batched.index.mu.Lock()
	batched.index.open, batched.index.mem = nil, nil
	batched.index.mu.Unlock()
	if _, err := batched.store.IndexPending(bg, tenantA, programP); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(results, searchAll(t, batched, tenantA)) {
		t.Fatal("index rebuilt after loss differs")
	}
}

func mapsEqual(a, b map[uint64][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !bytes.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

func quotaOf(err error) string {
	var ce *core.Error
	if errors.As(err, &ce) && ce.Reason == ReasonQuotaExceeded {
		return ce.Meta["quota"]
	}
	return ""
}

// pageAt is a distinct small page per i.
func pageAt(i int, at time.Time) content.Capture {
	return capture(fmt.Sprintf("https://quota.example/p%d", i), []byte(fmt.Sprintf("<html><body><p>page %d</p><a href=\"/x%d\">x</a></body></html>", i, i)),
		[]content.Header{{Name: "Content-Type", Value: "text/html"}}, at)
}

// CSTORE-INV-06: quotas are exact. With a captures/day limit N exactly N
// captures are admitted per UTC day (also under concurrency), the next is
// QUOTA_EXCEEDED with a retry at midnight and stores nothing, duplicates
// are not counted; a stored-bytes limit admits a capture iff usage plus
// the bytes it adds stays within the limit; an index-bytes limit refuses
// captures once the index holds that much.
func TestCStoreInv06_QuotaExact(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.DefaultLimits = Limits{CapturesPerDay: 3} })
	day := h.clock.Now()
	for i := range 3 {
		h.ingest(tenantA, pageAt(i, day.Add(time.Duration(i)*time.Minute)), "")
	}
	events := len(h.mem.OutboxEvents())
	_, err := h.store.Ingest(bg, tenantA, IngestRequest{Program: programP, Capture: pageAt(3, day.Add(time.Hour))})
	var ce *core.Error
	if quotaOf(err) != QuotaCapturesPerDay || !errors.As(err, &ce) || ce.RetryAfter != untilTomorrow(h.clock.Now()) || ce.Kind != core.KindResourceExhausted {
		t.Fatalf("4th capture: %v", err)
	}
	if len(h.snapshots(tenantA)) != 3 || len(h.mem.OutboxEvents()) != events || h.usage(tenantA).Captures != 3 {
		t.Fatal("a refused capture stored something")
	}
	if r := h.ingest(tenantA, pageAt(1, day.Add(time.Minute)), ""); !r.Duplicate {
		t.Fatal("duplicate of an admitted capture refused or re-stored")
	}
	h.clock.Advance(untilTomorrow(h.clock.Now()))
	h.ingest(tenantA, pageAt(3, day.Add(25*time.Hour)), "")
	if u := h.usage(tenantA); u.Captures != 1 || u.Day != today(h.clock.Now()) {
		t.Fatalf("next day usage %+v", u)
	}

	// Concurrency: exactly the limit succeeds.
	hc := newHarness(t, func(c *Config) { c.DefaultLimits = Limits{CapturesPerDay: 5} })
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, refused := 0, 0
	for i := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := hc.store.Ingest(bg, tenantA, IngestRequest{Program: programP, Capture: pageAt(i, day.Add(time.Duration(i)*time.Second))})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case quotaOf(err) == QuotaCapturesPerDay:
				refused++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if ok != 5 || refused != 11 || hc.usage(tenantA).Captures != 5 {
		t.Fatalf("concurrent: %d admitted, %d refused, usage %d; want 5, 11, 5", ok, refused, hc.usage(tenantA).Captures)
	}

	// Stored bytes: learn the exact usage after each capture, then set the
	// limit at it and one byte below.
	probe := newHarness(t)
	var usage []int64
	for i := range 3 {
		probe.ingest(tenantA, pageAt(i, day.Add(time.Duration(i)*time.Minute)), "")
		usage = append(usage, probe.usage(tenantA).StoredBytes)
	}
	for _, tc := range []struct {
		limit    int64
		admitted int
	}{{usage[1], 2}, {usage[1] - 1, 1}, {usage[2], 3}} {
		hq := newHarness(t, func(c *Config) { c.DefaultLimits = Limits{StoredBytes: tc.limit} })
		n := 0
		for i := range 3 {
			_, err := hq.store.Ingest(bg, tenantA, IngestRequest{Program: programP, Capture: pageAt(i, day.Add(time.Duration(i)*time.Minute))})
			if err == nil {
				n++
				continue
			}
			if quotaOf(err) != QuotaStoredBytes {
				t.Fatal(err)
			}
			break
		}
		if n != tc.admitted || hq.usage(tenantA).StoredBytes > tc.limit {
			t.Fatalf("stored-bytes limit %d (usages %v): %d admitted, usage %d; want %d", tc.limit, usage, n, hq.usage(tenantA).StoredBytes, tc.admitted)
		}
	}

	// Index bytes: the first capture fills the index; the next is refused.
	hi := newHarness(t, func(c *Config) { c.DefaultLimits = Limits{IndexBytes: 1} })
	hi.ingest(tenantA, pageAt(0, day), "")
	if _, err := hi.store.Ingest(bg, tenantA, IngestRequest{Program: programP, Capture: pageAt(1, day.Add(time.Minute))}); quotaOf(err) != QuotaIndexBytes {
		t.Fatalf("index quota: %v", err)
	}
	if err := hi.store.SetLimits(bg, tenantA, Limits{}); err != nil {
		t.Fatal(err)
	}
	hi.ingest(tenantA, pageAt(1, day.Add(time.Minute)), "")
}

// scanner collects every place a stored byte can live.
type scanner struct {
	t      testing.TB
	places map[string][]byte
}

func (s *scanner) add(name string, b []byte) { s.places[name] = append(s.places[name], b...) }

func (s *scanner) find(secret string) []string {
	var out []string
	for name, b := range s.places {
		if bytes.Contains(b, []byte(secret)) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// storedBytes gathers rows, raw chunk objects, decompressed pack records,
// decoded snapshot blobs and diff records, index documents and outbox
// events of a tenant.
func storedBytes(t testing.TB, h *harness, tenant core.UUID) *scanner {
	t.Helper()
	sc := &scanner{t: t, places: map[string][]byte{}}
	snaps, diffs, findings, vol, keys := h.mem.Dump(tenant)
	sc.add("rows", canonical(t, []any{snaps, diffs, findings, vol, keys}))
	names, _ := h.backend.List(bg, "")
	dec, _ := zstd.NewReader(nil)
	defer dec.Close()
	for _, n := range names {
		b, _ := h.backend.Get(bg, n)
		sc.add("object:"+n, b)
		if p, err := chunkstore.DecodePack(b); err == nil {
			for _, r := range p.Records {
				pl := r.Payload(b)
				if r.Codec == chunkstore.CodecZstd {
					pl, _ = dec.DecodeAll(pl, nil)
				}
				sc.add("record:"+n, pl)
			}
		}
	}
	tc, _ := h.chunks.open(bg, tenant)
	h.store.cache = newBlobCache(h.store.cfg.CacheBytes)
	for _, s := range snaps {
		p, err := h.store.snapshotPlain(bg, tc, s)
		if err != nil {
			t.Fatal(err)
		}
		sc.add("blobs:"+s.PublicID(), bytes.Join([][]byte{p.Record, p.Body, p.Normalized, p.Features}, nil))
	}
	for _, d := range diffs {
		to, _ := h.store.GetSnapshot(bg, tenant, d.To)
		hr, sr, err := h.store.loadDiff(bg, tc, d, to)
		if err != nil {
			t.Fatal(err)
		}
		sc.add("diff:"+d.PublicID(), canonical(t, []any{hr, sr}))
	}
	for _, d := range indexDump(t, h, tenant) {
		sc.add("index", d)
	}
	for _, md := range h.index.mem {
		files, _ := md.List()
		for _, f := range files {
			b, _ := md.ReadFile(f)
			sc.add("index-file:"+f, b)
		}
	}
	for _, e := range h.mem.OutboxEvents() {
		sc.add("outbox", e.Payload)
	}
	return sc
}

// CSTORE-INV-07: secrets are stored only encrypted. For a secret of every
// named rule planted in a body and in a response header, the finding
// decrypts to the secret, a raw read restores the exact body and headers,
// and no row, chunk object, decompressed record, decoded blob, diff
// record, index document or file, or outbox event contains it; a tampered
// ciphertext or another tenant's key fails to open it.
func TestCStoreInv07_EncryptionRoundTripNoPlaintext(t *testing.T) {
	r := testkit.Rng(t)
	h := newHarness(t)
	type planted struct {
		rule, secret string
		res          IngestResult
		body         []byte
		headers      []content.Header
	}
	var all []planted
	for i, rule := range secretRules {
		plant, secret := genSecret(r, rule)
		esc := strings.NewReplacer(`"`, `\"`, "\n", `\n`).Replace(plant)
		body := []byte(fmt.Sprintf("<html><body><p>build %d</p><script>var cfg = \"%s\";</script><p>%s</p></body></html>", i, esc, strings.ReplaceAll(plant, "<", "&lt;")))
		hs := []content.Header{{Name: "Content-Type", Value: "text/html"}, {Name: "X-Debug-Config", Value: strings.ReplaceAll(plant, "\n", " ")}}
		c := capture(fmt.Sprintf("https://leak.example/%s", rule), body, hs, t0.Add(time.Duration(i)*time.Minute))
		res := h.ingest(tenantA, c, "")
		all = append(all, planted{rule, secret, res, body, hs})
		// A second version, so the secret also passes through a diff.
		c2 := c
		c2.Body = append(slices.Clone(body), []byte("<p>v2</p>")...)
		c2.FetchedAt = c.FetchedAt.Add(time.Hour)
		h.ingest(tenantA, c2, "")
	}
	sc := storedBytes(t, h, tenantA)
	if len(sc.places) < 10 {
		t.Fatalf("scanned only %d places", len(sc.places))
	}
	for _, p := range all {
		if where := sc.find(p.secret); len(where) > 0 {
			t.Errorf("%s secret stored in the clear in %v", p.rule, where)
		}
		got, err := h.store.SnapshotContent(bg, tenantA, p.res.Snapshot.ID, ContentOptions{Raw: true})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Body, p.body) || !slices.Equal(got.Capture.Headers, p.headers) {
			t.Errorf("%s: raw read does not restore body and headers", p.rule)
		}
		red, err := h.store.SnapshotContent(bg, tenantA, p.res.Snapshot.ID, ContentOptions{Body: true})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(red.Body, []byte(p.secret)) || bytes.Contains(canonical(t, red.Capture.Headers), []byte(p.secret)) {
			t.Errorf("%s: redacted read returns the secret", p.rule)
		}
		fs := red.Findings
		var opened bool
		_, _, rows, _, _ := h.mem.Dump(tenantA)
		for _, f := range rows {
			if f.Snapshot != p.res.Snapshot.ID {
				continue
			}
			pt, err := h.store.cfg.Keys.decrypt(bg, tenantA, f.KeyID, findingAAD(tenantA, f.Snapshot, f.ID), f.Ciphertext)
			if err != nil {
				t.Fatal(err)
			}
			var fp findingPayload
			if err := json.Unmarshal(pt, &fp); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(fp.Secret), p.secret) {
				opened = true
			}
			bad := slices.Clone(f.Ciphertext)
			bad[len(bad)-1] ^= 1
			if _, err := h.store.cfg.Keys.decrypt(bg, tenantA, f.KeyID, findingAAD(tenantA, f.Snapshot, f.ID), bad); err == nil {
				t.Fatal("a tampered finding decrypted")
			}
			if _, err := h.store.cfg.Keys.decrypt(bg, tenantB, f.KeyID, findingAAD(tenantB, f.Snapshot, f.ID), f.Ciphertext); err == nil {
				t.Fatal("another tenant's key decrypted a finding")
			}
		}
		if len(fs) == 0 || !opened {
			t.Errorf("%s: no finding decrypts to the secret (%d findings)", p.rule, len(fs))
		}
	}
	covered := map[string]bool{}
	for _, r := range secretRules {
		covered[r] = true
	}
	for _, r := range content.SecretRuleIDs() {
		if !covered[r[0]] && r[0] != "generic_high_entropy" {
			t.Errorf("rule %s has no generator in this test", r[0])
		}
	}
}

// CSTORE-INV-08: the SPA conformance site's versions (every page, bundle
// and chunk deploy) are stored, as raw snapshots (redacted bodies and
// capture records, the bytes needed to reproduce each version), in less
// than 25 % of their raw size with the default codec. The derived records
// (normalized texts, features, diffs) are reported alongside.
func TestCStoreInv08_SPAStorageUnderQuarter(t *testing.T) {
	for _, codec := range []BlobCodec{CodecDelta, CodecChunk} {
		h := newHarness(t, func(c *Config) { c.Codec = codec })
		res := h.ingestSPA(tenantA)
		tc, _ := h.chunks.open(bg, tenantA)
		var raw, versions int64
		stat := func(ref string) int64 {
			id, err := chunkstore.ParseManifestID(ref)
			if err != nil {
				t.Fatal(err)
			}
			info, err := tc.s.Stat(bg, id)
			if err != nil {
				t.Fatal(err)
			}
			return int64(info.StoredBytes)
		}
		for _, r := range res {
			raw += r.Snapshot.RawSize
			versions += stat(r.Snapshot.BodyManifest) + stat(r.Snapshot.HeadersRef)
		}
		m, _ := h.chunks.Metrics(bg, tenantA)
		t.Logf("codec %s: raw %d, versions %d (%.1f%%), everything %d (%.1f%%)", codec, raw, versions,
			100*float64(versions)/float64(raw), m.CompressedBytes, 100*float64(m.CompressedBytes)/float64(raw))
		if codec == CodecDelta && 4*versions >= raw {
			t.Fatalf("versions take %d bytes of %d raw (%.1f%%), want < 25%%", versions, raw, 100*float64(versions)/float64(raw))
		}
	}
}

// CSTORE-INV-09: validity intervals. A key's versions have disjoint
// [valid_from, valid_to) intervals in fetch order with exactly one open
// version (the newest non-degraded); an unchanged re-fetch (only volatile
// headers differ) adds no row and moves last_seen; a degraded capture is
// stored closed and empty and never becomes the open version; an as-of
// lookup at any time returns the version whose interval contains it.
func TestCStoreInv09_ValidityIntervals(t *testing.T) {
	h := newHarness(t)
	u := "https://valid.example/"
	hs := func(date string) []content.Header {
		return []content.Header{{Name: "Content-Type", Value: "text/html"}, {Name: "Date", Value: date}, {Name: "X-Request-Id", Value: date}}
	}
	body := func(i int) []byte {
		return []byte(fmt.Sprintf("<html><head><title>Shop</title></head><body><p>v%d</p><a href=\"/a\">a</a><a href=\"/b\">b</a><a href=\"/c%d\">c</a></body></html>", i, i))
	}
	at := t0
	v1 := h.ingest(tenantA, capture(u, body(1), hs("one"), at), "")
	re := h.ingest(tenantA, capture(u, body(1), hs("two"), at.Add(time.Hour)), "")
	if !re.Unchanged || re.Snapshot.ID != v1.Snapshot.ID || !re.Snapshot.LastSeen.Equal(at.Add(time.Hour)) {
		t.Fatalf("re-fetch: %+v", re)
	}
	if again := h.ingest(tenantA, capture(u, body(1), hs("two"), at.Add(time.Hour)), ""); !again.Duplicate {
		t.Fatal("re-send of a re-fetch not a duplicate")
	}
	v2 := h.ingest(tenantA, capture(u, body(2), hs("three"), at.Add(2*time.Hour)), "")
	login := capture(u, []byte(`<html><body><form action="/login" method="post"><input name="user"><input type="password" name="pw"></form></body></html>`),
		[]content.Header{{Name: "Content-Type", Value: "text/html"}}, at.Add(3*time.Hour))
	login.RedirectChain = []content.Hop{{URL: u, Status: 302, Headers: []content.Header{{Name: "Location", Value: "https://valid.example/login?next=/"}}}}
	login.Status = 200
	deg := h.ingest(tenantA, login, "")
	if !deg.Snapshot.Degraded || deg.Snapshot.Open() || !deg.Snapshot.ValidTo.Equal(deg.Snapshot.ValidFrom) || deg.Diff != nil {
		t.Fatalf("degraded capture: degraded=%v open=%v diff=%v", deg.Snapshot.Degraded, deg.Snapshot.Open(), deg.Diff != nil)
	}
	v3 := h.ingest(tenantA, capture(u, body(3), hs("four"), at.Add(4*time.Hour)), "")
	if v3.Diff == nil || v3.Diff.From != v2.Snapshot.ID {
		t.Fatal("the version after a degraded capture must diff against the last good one")
	}
	ss, _ := h.store.ListSnapshots(bg, tenantA, SnapshotFilter{Program: programP, URL: u, Profile: "default"})
	if len(ss) != 4 {
		t.Fatalf("%d snapshots, want 4 (v1, v2, degraded, v3)", len(ss))
	}
	open := 0
	var good []Snapshot
	for _, s := range ss {
		if s.Open() {
			open++
		}
		if !s.Degraded {
			good = append(good, s)
		}
	}
	slices.Reverse(good)
	if open != 1 || !good[len(good)-1].Open() {
		t.Fatalf("%d open versions", open)
	}
	for i := 1; i < len(good); i++ {
		if !good[i-1].ValidTo.Equal(good[i].ValidFrom) {
			t.Fatalf("intervals not contiguous: %v / %v", good[i-1].ValidTo, good[i].ValidFrom)
		}
	}
	k := Key{Program: programP, URL: u, Profile: "default"}
	for _, tc := range []struct {
		at   time.Time
		want core.UUID
	}{{at, v1.Snapshot.ID}, {at.Add(90 * time.Minute), v1.Snapshot.ID}, {at.Add(2 * time.Hour), v2.Snapshot.ID},
		{at.Add(3 * time.Hour), v2.Snapshot.ID}, {at.Add(100 * time.Hour), v3.Snapshot.ID}} {
		s, err := h.store.AsOf(bg, tenantA, k, tc.at)
		if err != nil || s.ID != tc.want {
			t.Fatalf("as of %v: %s %v, want %s", tc.at, s.PublicID(), err, core.PublicID(PrefixSnapshot, tc.want))
		}
	}
	if _, err := h.store.AsOf(bg, tenantA, k, at.Add(-time.Second)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("as of before the first capture: %v", err)
	}
	if _, err := h.store.Ingest(bg, tenantA, IngestRequest{Program: programP, Capture: capture(u, body(9), hs("x"), at.Add(time.Minute))}); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("an older, unseen capture: %v", err)
	}
}

// CSTORE-INV-10: search defaults (V2 §7, §12.14). Hunk sides are separate
// documents (a removed line matches side:removed, not side:added);
// vendored bundles are excluded unless asked for; base64 and gzip blobs in
// content are searchable decoded; "first appeared" returns the earliest
// snapshot that added a string, a baseline included.
func TestCStoreInv10_SearchDefaults(t *testing.T) {
	h := newHarness(t)
	res := h.ingestSPA(tenantA)
	vendorJS := func(v string, d int) content.Capture {
		return capture("https://shop.example/static/js/vendor-1a2b3c4d.js", []byte("/*! lib */\nfunction quokkaHelper"+v+"(){return 1}\n"), jsHeaders, t0.AddDate(0, 0, d))
	}
	h.ingest(tenantA, vendorJS("One", 1), "")
	h.ingest(tenantA, vendorJS("Two", 2), "")
	for _, tc := range []struct {
		q       string
		include bool
		want    bool
	}{{"quokkahelpertwo", false, false}, {"quokkahelpertwo", true, true}, {"quokkahelpertwo vendor:1", false, true}} {
		r, err := h.store.Search(bg, tenantA, programP, tc.q, SearchOptions{IncludeVendor: tc.include})
		if err != nil {
			t.Fatal(err)
		}
		if (r.Total > 0) != tc.want {
			t.Fatalf("%q include=%v: total %d", tc.q, tc.include, r.Total)
		}
	}
	// v3 of the page removed X-Frame-Options; the header line is on the
	// removed side only.
	for _, tc := range []struct {
		q    string
		want bool
	}{{`"x-frame-options" side:removed`, true}, {`"x-frame-options" side:added kind:hunk baseline:0`, false}} {
		r, err := h.store.Search(bg, tenantA, programP, tc.q, SearchOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if (r.Total > 0) != tc.want {
			t.Fatalf("%q: total %d, want %v", tc.q, r.Total, tc.want)
		}
	}
	// First appeared: in the baseline of the page, and in v3.
	for _, tc := range []struct {
		text string
		want IngestResult
	}{{"/products", res[0]}, {"/account/delete", res[5]}, {"/api/v1/admin/export", res[6]}} {
		hit, found, err := h.store.FirstAppeared(bg, tenantA, programP, tc.text, SearchOptions{})
		if err != nil || !found || hit.SnapshotID != tc.want.Snapshot.PublicID() {
			t.Fatalf("first appeared %q: %+v found=%v %v, want %s", tc.text, hit, found, err, tc.want.Snapshot.PublicID())
		}
	}
	if _, found, _ := h.store.FirstAppeared(bg, tenantA, programP, "never-in-any-version-xyz", SearchOptions{}); found {
		t.Fatal("first appeared found an absent string")
	}
	// A base64 blob of text is searchable by its decoded words.
	blob := "cGF5bG9hZCBpbmNsdWRlcyBwbGF0eXB1c21hcmtlciBhbmQgbW9yZSB0ZXh0"
	h.ingest(tenantA, capture("https://shop.example/data.txt", []byte("data="+blob+"\n"), []content.Header{{Name: "Content-Type", Value: "text/plain"}}, t0.AddDate(0, 0, 3)), "")
	if r, err := h.store.Search(bg, tenantA, programP, "platypusmarker", SearchOptions{}); err != nil || r.Total == 0 {
		t.Fatalf("decoded base64 blob not searchable: %v %+v", err, r)
	}
	if _, err := h.store.Search(bg, tenantA, programP, "kind:(", SearchOptions{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad query: %v", err)
	}
}

// CSTORE-INV-11: the SPA end to end. Renamed bundles are linked to their
// predecessors (by lineage key, then by SimHash) and an unrelated chunk is
// not; v1 → v2 of the page is masked-only (no index documents); v2 → v3
// carries the planted changes, findable by a field query and a regex;
// events carry a decodable observation and a valid content_changed patch,
// and baselines are not alertable while the planted change is.
func TestCStoreInv11_SPAEndToEnd(t *testing.T) {
	h := newHarness(t)
	res := h.ingestSPA(tenantA)
	lin := func(i int) string {
		if res[i].Lineage == nil {
			return ""
		}
		return res[i].Lineage.URL
	}
	if !strings.HasSuffix(lin(4), "main.3f9a1c2b.js") || !strings.HasSuffix(lin(6), "main.77e0d41a.js") ||
		!strings.HasSuffix(lin(7), "index-a1b2c3d4.js") || lin(8) != "" {
		t.Fatalf("lineage: %q %q %q %q", lin(4), lin(6), lin(7), lin(8))
	}
	if res[4].Snapshot.LineageParentID != res[1].Snapshot.ID {
		t.Fatal("lineage parent not recorded")
	}
	if res[3].ChangeSet.Outcome != content.DiffMaskedOnly || res[3].Diff.IndexDocs != 0 {
		t.Fatalf("page v1→v2: %s with %d docs", res[3].ChangeSet.Outcome, res[3].Diff.IndexDocs)
	}
	if res[5].ChangeSet.Outcome != content.DiffChanged || res[5].ChangeSet.Class != oam.ClassSemanticEndpoint {
		t.Fatalf("page v2→v3: %s/%s", res[5].ChangeSet.Outcome, res[5].ChangeSet.Class)
	}
	for _, q := range []string{`kind:form_added`, `/admin\/export/`, `kind:endpoint_added admin`} {
		r, err := h.store.Search(bg, tenantA, programP, q, SearchOptions{})
		if err != nil || r.Total == 0 {
			t.Fatalf("%s: %v %+v", q, err, r)
		}
	}
	events := h.mem.OutboxEvents()
	if len(events) != len(res) {
		t.Fatalf("%d events for %d captures", len(events), len(res))
	}
	for i, e := range events {
		var o outboxJSON
		if err := json.Unmarshal(e.Payload, &o); err != nil {
			t.Fatal(err)
		}
		var p EventPayload
		if err := json.Unmarshal(o.Payload, &p); err != nil {
			t.Fatal(err)
		}
		obs, err := oam.DecodeObservation([]byte(p.Observation), oam.Strict)
		if err != nil {
			t.Fatalf("event %d: observation: %v", i, err)
		}
		if obs.Authority == nil || p.SnapshotID != res[i].Snapshot.PublicID() || o.EventID != e.EventID.String() {
			t.Fatalf("event %d: authority %v snapshot %s", i, obs.Authority, p.SnapshotID)
		}
		if p.ContentChange != nil {
			if _, err := p.ContentChange.Normalize(); err != nil {
				t.Fatalf("event %d: patch: %v", i, err)
			}
		}
		baseline := res[i].Diff != nil && res[i].Diff.From.IsZero()
		if baseline && p.Alertable {
			t.Fatalf("event %d: a baseline is alertable", i)
		}
	}
	var p5 EventPayload
	var o5 outboxJSON
	_ = json.Unmarshal(events[5].Payload, &o5)
	_ = json.Unmarshal(o5.Payload, &p5)
	if !p5.Alertable || o5.Type != EventChanged || p5.ContentChange == nil || p5.ContentChange.SnapshotFrom != res[3].Snapshot.PublicID() {
		t.Fatalf("v3 event: %+v", p5)
	}
}

// CSTORE-INV-12: learned volatility. A non-feature attribute that changes
// on every capture of an otherwise stable page is frozen into a learned
// rule after the policy's observations (once, as version 1), later
// captures use the learned normalizer version and diff as masked-only,
// and feature paths are never counted.
func TestCStoreInv12_VolatilityLearned(t *testing.T) {
	r := testkit.Rng(t)
	h := newHarness(t)
	u := "https://vol.example/"
	var last IngestResult
	for i := range 14 {
		body := fmt.Sprintf(`<html><body><div title="%s">hello</div><a href="/about">about</a></body></html>`, token(r, 12, alnum))
		last = h.ingest(tenantA, capture(u, []byte(body), []content.Header{{Name: "Content-Type", Value: "text/html"}}, t0.Add(time.Duration(i)*time.Hour)), "")
	}
	_, _, _, rows, _ := h.mem.Dump(tenantA)
	var frozen []string
	for _, row := range rows {
		if content.IsFeaturePath(row.Path) {
			t.Fatalf("feature path counted: %s", row.Path)
		}
		if row.FrozenVersion > 0 {
			frozen = append(frozen, row.Path)
			if row.FrozenVersion != 1 {
				t.Fatalf("%s frozen as version %d", row.Path, row.FrozenVersion)
			}
		}
	}
	if !slices.Equal(frozen, []string{"html:attr:div.title"}) {
		t.Fatalf("frozen %v", frozen)
	}
	if !strings.Contains(last.Snapshot.NormalizerVersion, ".learned1") || last.ChangeSet.Outcome != content.DiffMaskedOnly {
		t.Fatalf("after learning: normalizer %s outcome %s", last.Snapshot.NormalizerVersion, last.ChangeSet.Outcome)
	}
}

// CSTORE-INV-13: observations replay. The observation published for an
// unchanged re-fetch (rebuilt from the stored processed capture with its
// decrypted findings) is byte-identical to processing the same capture
// afresh.
func TestCStoreInv13_ObservationReplay(t *testing.T) {
	h := newHarness(t)
	v3 := spaVersions(t)[5].C
	h.ingest(tenantA, v3, "")
	again := v3
	again.FetchedAt = v3.FetchedAt.Add(time.Hour)
	again.Headers = append(slices.Clone(v3.Headers), content.Header{Name: "Age", Value: "42"})
	re := h.ingest(tenantA, again, "")
	if !re.Unchanged {
		t.Fatal("re-fetch not recognized as unchanged")
	}
	fresh := newHarness(t)
	fresh.store.cfg.Keys = h.store.cfg.Keys // same tenant keys, so keyed hashes agree
	hasher, _ := fresh.store.cfg.Keys.hasher(bg, tenantA)
	p, err := content.Process(&again, content.ProcessOptions{Hasher: hasher})
	if err != nil {
		t.Fatal(err)
	}
	o, err := content.Observe(p, content.ObserveOptions{Hasher: hasher, SnapshotID: re.Snapshot.PublicID()})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := content.ObservationJSON(o)
	b, _ := content.ObservationJSON(re.Observation)
	if !bytes.Equal(a, b) {
		t.Fatalf("replayed observation differs:\n%s\n%s", a, b)
	}
}
