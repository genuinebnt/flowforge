// Package storetest is the contract suite of the contentstore Repo port:
// every adapter (contentstore.MemRepo, contentstore/pg) runs Run, so the
// fake and Postgres agree on transactions, tenant scoping, the open-version
// rule, cascades, ordering and the whole store flow on top (ingest,
// idempotence, diffs, encryption, retention, quotas, search). Test support
// only: imported from _test.go files.
package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/chunkstore"
	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/contentstore"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

var (
	t0       = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	tenantA  = core.MustParseUUID("00000000-0000-7000-8000-00000000a001")
	tenantB  = core.MustParseUUID("00000000-0000-7000-8000-00000000b002")
	programP = core.MustParseUUID("00000000-0000-7000-8000-00000000c003")
	programQ = core.MustParseUUID("00000000-0000-7000-8000-00000000c004")
)

// Run runs the contract. open returns a fresh, empty repository per call.
func Run(t *testing.T, open func(t *testing.T) contentstore.Repo) {
	t.Run("OpenVersionUnique", func(t *testing.T) { openVersionUnique(t, open(t)) })
	t.Run("RollbackOnError", func(t *testing.T) { rollback(t, open(t)) })
	t.Run("TenantScoping", func(t *testing.T) { tenantScoping(t, open(t)) })
	t.Run("DeleteCascades", func(t *testing.T) { deleteCascades(t, open(t)) })
	t.Run("OrderingAndAsOf", func(t *testing.T) { ordering(t, open(t)) })
	t.Run("SnapshotRoundTrip", func(t *testing.T) { roundTrip(t, open(t)) })
	t.Run("StoreFlow", func(t *testing.T) { storeFlow(t, open(t)) })
	t.Run("ConcurrentQuotaAndIdempotence", func(t *testing.T) { concurrency(t, open) })
}

func snap(i int, url string, at time.Time, open bool) contentstore.Snapshot {
	s := contentstore.Snapshot{ID: core.MustParseUUID(fmt.Sprintf("00000000-0000-8000-8000-%012d", i)), Tenant: tenantA, Program: programP,
		URL: url, URLTemplate: url, Host: "h.example", Profile: "default", FetchedAt: at, LastSeen: at, ValidFrom: at, Status: 200,
		Kind: "html", Signature: fmt.Sprint("sig", i), HeadersRef: fmt.Sprintf("%064x", 4*i), BodyManifest: fmt.Sprintf("%064x", 4*i+1),
		NormalizedManifest: fmt.Sprintf("%064x", 4*i+2), FeaturesRef: fmt.Sprintf("%064x", 4*i+3), BodySHA256: "00", NormalizerVersion: "html@1",
		Outcome: "complete", Coverage: "complete", Codec: "chunk", CreatedAt: at}
	if !open {
		s.ValidTo = at.Add(time.Hour)
	}
	return s
}

func inTx(t *testing.T, r contentstore.Repo, tenant core.UUID, fn func(contentstore.Tx) error) {
	t.Helper()
	if err := r.InTx(context.Background(), tenant, fn); err != nil {
		t.Fatal(err)
	}
}

func openVersionUnique(t *testing.T, r contentstore.Repo) {
	ctx := context.Background()
	a, b := snap(1, "https://h.example/", t0, true), snap(2, "https://h.example/", t0.Add(time.Hour), true)
	inTx(t, r, tenantA, func(tx contentstore.Tx) error { return tx.InsertSnapshot(ctx, a) })
	err := r.InTx(ctx, tenantA, func(tx contentstore.Tx) error { return tx.InsertSnapshot(ctx, b) })
	if !errors.Is(err, contentstore.ErrConflict) {
		t.Fatalf("second open version: %v, want a conflict", err)
	}
	inTx(t, r, tenantA, func(tx contentstore.Tx) error {
		if err := tx.CloseVersion(ctx, a.ID, b.ValidFrom); err != nil {
			return err
		}
		return tx.InsertSnapshot(ctx, b)
	})
	inTx(t, r, tenantA, func(tx contentstore.Tx) error {
		o, ok, err := tx.OpenVersion(ctx, b.Key())
		if err != nil || !ok || o.ID != b.ID {
			t.Fatalf("open version %v %v %v", o.ID, ok, err)
		}
		if err := tx.CloseVersion(ctx, a.ID, b.ValidFrom); !errors.Is(err, contentstore.ErrConflict) {
			t.Fatalf("closing a closed version: %v", err)
		}
		return nil
	})
}

func rollback(t *testing.T, r contentstore.Repo) {
	ctx := context.Background()
	boom := errors.New("boom")
	err := r.InTx(ctx, tenantA, func(tx contentstore.Tx) error {
		if err := tx.EnsureProgram(ctx, programP); err != nil {
			return err
		}
		if err := tx.InsertSnapshot(ctx, snap(1, "https://h.example/", t0, true)); err != nil {
			return err
		}
		if err := tx.PutUsage(ctx, contentstore.Usage{Day: "2026-11-08", Captures: 9}); err != nil {
			return err
		}
		if err := tx.AppendOutbox(ctx, contentstore.OutboxEvent{EventID: core.MustParseUUID("00000000-0000-8000-8000-0000000000e1"),
			Topic: "content.changes", Payload: []byte(`{"event_id":"x"}`)}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx error %v, want the callback's", err)
	}
	inTx(t, r, tenantA, func(tx contentstore.Tx) error {
		if _, ok, _ := tx.GetSnapshot(ctx, snap(1, "", t0, true).ID); ok {
			t.Fatal("rolled-back snapshot visible")
		}
		if u, _ := tx.Usage(ctx); u.Captures != 0 {
			t.Fatalf("rolled-back usage %+v", u)
		}
		ps, _ := tx.Programs(ctx)
		if len(ps) != 0 {
			t.Fatalf("rolled-back program %v", ps)
		}
		return nil
	})
}

func tenantScoping(t *testing.T, r contentstore.Repo) {
	ctx := context.Background()
	s := snap(1, "https://h.example/", t0, true)
	inTx(t, r, tenantA, func(tx contentstore.Tx) error {
		if err := tx.EnsureProgram(ctx, programP); err != nil {
			return err
		}
		if err := tx.InsertSnapshot(ctx, s); err != nil {
			return err
		}
		seq, err := tx.NextDiffSeq(ctx, programP)
		if err != nil {
			return err
		}
		if err := tx.InsertDiff(ctx, contentstore.Diff{ID: core.MustParseUUID("00000000-0000-8000-8000-0000000000d1"), Program: programP,
			Seq: seq, To: s.ID, Levels: []string{"hash"}, Algorithm: "a", NormalizerVersion: "html@1", HunksRef: "aa", SemanticRef: "bb",
			At: t0, URL: s.URL, Profile: "default", CreatedAt: t0}); err != nil {
			return err
		}
		if err := tx.InsertFindings(ctx, []contentstore.Finding{{ID: core.MustParseUUID("00000000-0000-8000-8000-0000000000f1"),
			Program: programP, Snapshot: s.ID, RuleID: "jwt", Preview: "eyJh...xx", KeyedHash: "kh", Ciphertext: []byte{1, 2}, KeyID: "enc/1", CreatedAt: t0}}); err != nil {
			return err
		}
		if err := tx.PutUsage(ctx, contentstore.Usage{Day: "2026-11-08", Captures: 3, StoredBytes: 10}); err != nil {
			return err
		}
		if err := tx.PutLimits(ctx, contentstore.Limits{CapturesPerDay: 5}); err != nil {
			return err
		}
		if err := tx.PutIndexState(ctx, contentstore.IndexState{Program: programP, Watermark: 1, Docs: 2, Bytes: 3}); err != nil {
			return err
		}
		_, err = tx.InsertDataKey(ctx, contentstore.DataKey{Tenant: tenantA, Purpose: "enc", Version: 1, KEKID: "k", Wrapped: []byte{9}, CreatedAt: t0})
		return err
	})
	inTx(t, r, tenantB, func(tx contentstore.Tx) error {
		if _, ok, _ := tx.GetSnapshot(ctx, s.ID); ok {
			t.Error("B reads A's snapshot")
		}
		if _, ok, _ := tx.OpenVersion(ctx, s.Key()); ok {
			t.Error("B sees A's open version")
		}
		if ss, _ := tx.ListSnapshots(ctx, contentstore.SnapshotFilter{Program: programP}); len(ss) != 0 {
			t.Error("B lists A's snapshots")
		}
		if ds, _ := tx.DiffsAfter(ctx, programP, 0, 10); len(ds) != 0 {
			t.Error("B lists A's diffs")
		}
		if fs, _ := tx.Findings(ctx, s.ID); len(fs) != 0 {
			t.Error("B reads A's findings")
		}
		if u, _ := tx.Usage(ctx); u.Captures != 0 {
			t.Error("B reads A's usage")
		}
		if _, ok, _ := tx.Limits(ctx); ok {
			t.Error("B reads A's limits")
		}
		if st, _ := tx.IndexState(ctx, programP); st.Watermark != 0 {
			t.Error("B reads A's index state")
		}
		if ks, _ := tx.DataKeys(ctx); len(ks) != 0 {
			t.Error("B reads A's data keys")
		}
		if live, _, _ := tx.Manifests(ctx); len(live) != 0 {
			t.Error("B sees A's manifests")
		}
		if ps, _ := tx.Programs(ctx); len(ps) != 0 {
			t.Error("B sees A's programs")
		}
		// B's own writes do not touch A's rows.
		return tx.DeleteSnapshots(ctx, []core.UUID{s.ID})
	})
	inTx(t, r, tenantA, func(tx contentstore.Tx) error {
		if _, ok, _ := tx.GetSnapshot(ctx, s.ID); !ok {
			t.Error("B deleted A's snapshot")
		}
		return nil
	})
	ts, err := r.Tenants(ctx)
	if err != nil || len(ts) != 1 || ts[0] != tenantA {
		t.Fatalf("tenants %v %v", ts, err)
	}
}

func deleteCascades(t *testing.T, r contentstore.Repo) {
	ctx := context.Background()
	a, b, c := snap(1, "https://h.example/", t0, false), snap(2, "https://h.example/", t0.Add(time.Hour), false), snap(3, "https://h.example/", t0.Add(2*time.Hour), true)
	a.ValidTo, b.ValidTo = b.ValidFrom, c.ValidFrom
	b.ParentID, c.ParentID, c.LineageParentID = a.ID, b.ID, a.ID
	b.Codec, b.DeltaBase, b.DeltaDepth = "delta", a.ID, 1
	d1 := contentstore.Diff{ID: core.MustParseUUID("00000000-0000-8000-8000-0000000000d1"), Program: programP, From: a.ID, To: b.ID,
		Levels: []string{"hash"}, Algorithm: "a", NormalizerVersion: "v", HunksRef: "11", SemanticRef: "12", At: b.FetchedAt, URL: b.URL, Profile: "default", CreatedAt: t0}
	d2 := d1
	d2.ID, d2.From, d2.To, d2.HunksRef, d2.SemanticRef = core.MustParseUUID("00000000-0000-8000-8000-0000000000d2"), b.ID, c.ID, "21", "22"
	inTx(t, r, tenantA, func(tx contentstore.Tx) error {
		if err := tx.EnsureProgram(ctx, programP); err != nil {
			return err
		}
		for _, s := range []contentstore.Snapshot{a, b, c} {
			if err := tx.InsertSnapshot(ctx, s); err != nil {
				return err
			}
		}
		for _, d := range []*contentstore.Diff{&d1, &d2} {
			seq, err := tx.NextDiffSeq(ctx, programP)
			if err != nil {
				return err
			}
			d.Seq = seq
			if err := tx.InsertDiff(ctx, *d); err != nil {
				return err
			}
		}
		if d1.Seq != 1 || d2.Seq != 2 {
			t.Fatalf("diff seqs %d %d", d1.Seq, d2.Seq)
		}
		if err := tx.SetPinned(ctx, c.ID, true, "alerted"); err != nil {
			return err
		}
		return tx.InsertFindings(ctx, []contentstore.Finding{{ID: core.MustParseUUID("00000000-0000-8000-8000-0000000000f1"), Program: programP,
			Snapshot: a.ID, RuleID: "jwt", Preview: "p", KeyedHash: "h", Ciphertext: []byte{1}, KeyID: "enc/1", CreatedAt: t0}})
	})
	inTx(t, r, tenantA, func(tx contentstore.Tx) error {
		deps, err := tx.DeltaDependents(ctx, []core.UUID{a.ID})
		if err != nil || len(deps) != 1 || deps[0].ID != b.ID {
			t.Fatalf("delta dependents %v %v", deps, err)
		}
		if err := tx.RewriteBlobs(ctx, b.ID, contentstore.BlobRefs{HeadersRef: "a1", BodyManifest: "a2", NormalizedManifest: "a3",
			FeaturesRef: "a4", Codec: "delta"}); err != nil {
			return err
		}
		touching, err := tx.DiffsTouching(ctx, []core.UUID{a.ID})
		if err != nil || len(touching) != 1 || touching[0].ID != d1.ID {
			t.Fatalf("diffs touching a: %v %v", touching, err)
		}
		into, ok, err := tx.DiffInto(ctx, c.ID)
		if err != nil || !ok || into.ID != d2.ID {
			t.Fatalf("diff into c: %v %v %v", into.ID, ok, err)
		}
		live, pinned, err := tx.Manifests(ctx)
		if err != nil {
			return err
		}
		if len(live) != 14 || len(pinned) != 4 {
			t.Fatalf("manifests: %d live %d pinned, want 14 and 4", len(live), len(pinned))
		}
		return tx.DeleteSnapshots(ctx, []core.UUID{a.ID})
	})
	inTx(t, r, tenantA, func(tx contentstore.Tx) error {
		if _, ok, _ := tx.GetDiff(ctx, d1.ID); ok {
			t.Error("diff from a deleted snapshot survived")
		}
		if _, ok, _ := tx.GetDiff(ctx, d2.ID); !ok {
			t.Error("unrelated diff deleted")
		}
		if fs, _ := tx.Findings(ctx, a.ID); len(fs) != 0 {
			t.Error("findings of a deleted snapshot survived")
		}
		gb, _, _ := tx.GetSnapshot(ctx, b.ID)
		gc, _, _ := tx.GetSnapshot(ctx, c.ID)
		if !gb.ParentID.IsZero() || !gc.LineageParentID.IsZero() || gc.ParentID != b.ID || !gc.Pinned || gc.PinReason != "alerted" {
			t.Errorf("links after delete: b.parent %v c.lineage %v c.parent %v pinned %v", gb.ParentID, gc.LineageParentID, gc.ParentID, gc.Pinned)
		}
		if gb.BodyManifest != "a2" || !gb.DeltaBase.IsZero() || gb.DeltaDepth != 0 {
			t.Errorf("rewrite: %+v", gb)
		}
		return nil
	})
}

func ordering(t *testing.T, r contentstore.Repo) {
	ctx := context.Background()
	var all []contentstore.Snapshot
	inTx(t, r, tenantA, func(tx contentstore.Tx) error {
		for i := range 7 {
			at := t0.Add(time.Duration(i) * time.Hour)
			s := snap(i+1, fmt.Sprintf("https://h.example/%d", i%3), at, true)
			if i >= 3 {
				// Close the previous version of the key first.
				prev := all[i-3]
				if err := tx.CloseVersion(ctx, prev.ID, at); err != nil {
					return err
				}
			}
			if err := tx.InsertSnapshot(ctx, s); err != nil {
				return err
			}
			all = append(all, s)
		}
		return nil
	})
	inTx(t, r, tenantA, func(tx contentstore.Tx) error {
		page1, err := tx.ListSnapshots(ctx, contentstore.SnapshotFilter{Program: programP, Limit: 4})
		if err != nil {
			return err
		}
		last := page1[len(page1)-1]
		page2, err := tx.ListSnapshots(ctx, contentstore.SnapshotFilter{Program: programP, Limit: 4, Before: &contentstore.Cursor{ValidFrom: last.ValidFrom, ID: last.ID}})
		if err != nil {
			return err
		}
		var got []core.UUID
		for _, s := range append(page1, page2...) {
			got = append(got, s.ID)
		}
		for i := range got {
			if got[i] != all[len(all)-1-i].ID {
				t.Fatalf("order: position %d", i)
			}
		}
		open, _ := tx.ListSnapshots(ctx, contentstore.SnapshotFilter{Program: programP, OpenOnly: true})
		if len(open) != 3 {
			t.Fatalf("%d open versions, want 3", len(open))
		}
		keyed, _ := tx.ListSnapshots(ctx, contentstore.SnapshotFilter{Program: programP, URL: "https://h.example/0", Profile: "default"})
		ks, _ := tx.KeySnapshots(ctx, all[0].Key())
		if len(keyed) != 3 || len(ks) != 3 {
			t.Fatalf("key filter %d, key snapshots %d", len(keyed), len(ks))
		}
		keys, _ := tx.Keys(ctx, programP)
		if len(keys) != 3 {
			t.Fatalf("%d keys", len(keys))
		}
		for _, tc := range []struct {
			at   time.Time
			want core.UUID
			ok   bool
		}{{t0, all[0].ID, true}, {t0.Add(2*time.Hour + time.Minute), all[0].ID, true}, {t0.Add(3 * time.Hour), all[3].ID, true},
			{t0.Add(100 * time.Hour), all[6].ID, true}, {t0.Add(-time.Second), core.Nil, false}} {
			s, ok, err := tx.SnapshotAsOf(ctx, all[0].Key(), tc.at)
			if err != nil || ok != tc.ok || ok && s.ID != tc.want {
				t.Fatalf("as of %v: %v %v %v", tc.at, s.ID, ok, err)
			}
		}
		hosts, _ := tx.HostOpenVersions(ctx, programP, "h.example", "default", "https://h.example/0", 10)
		if len(hosts) != 2 {
			t.Fatalf("%d host versions, want 2", len(hosts))
		}
		byURL, _ := tx.OpenVersionsByURL(ctx, programP, "default", []string{"https://h.example/1", "https://h.example/none"})
		if len(byURL) != 1 || byURL[0].ID != all[4].ID {
			t.Fatalf("by url %v", byURL)
		}
		return nil
	})
}

func roundTrip(t *testing.T, r contentstore.Repo) {
	ctx := context.Background()
	s := snap(1, "https://h.example/x", t0.Add(1500*time.Millisecond), false)
	s.EntityID, s.RunID, s.EffectPath, s.Mime, s.RawSize, s.StoredSize = "ent_X", "run_Y", "0/1", "text/html", 99, 42
	s.SimHash, s.Degraded, s.ValidTo = 1<<63|5, true, s.ValidFrom
	s.BodySeal, s.HeadersSeal, s.KeyID, s.Codec, s.Pinned, s.PinReason = []byte{1, 2, 3}, []byte{4}, "enc/1", "delta", true, "r"
	inTx(t, r, tenantA, func(tx contentstore.Tx) error { return tx.InsertSnapshot(ctx, s) })
	inTx(t, r, tenantA, func(tx contentstore.Tx) error {
		got, ok, err := tx.GetSnapshot(ctx, s.ID)
		if err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
		a, _ := core.CanonicalJSON(got)
		b, _ := core.CanonicalJSON(s)
		if !bytes.Equal(a, b) {
			t.Fatalf("snapshot round trip:\n got %s\nwant %s", a, b)
		}
		return nil
	})
	// Volatility, limits, index state and data keys round trip.
	rows := []contentstore.VolatilityRow{{Program: programQ, URLTemplate: "u", Profile: "p", Path: "html:attr:div.title", Changes: 3, Stable: 1, Streak: 2, FrozenVersion: 1}}
	inTx(t, r, tenantA, func(tx contentstore.Tx) error {
		if err := tx.PutVolatility(ctx, rows); err != nil {
			return err
		}
		rows[0].Changes = 4
		return tx.PutVolatility(ctx, rows)
	})
	inTx(t, r, tenantA, func(tx contentstore.Tx) error {
		got, _ := tx.Volatility(ctx, programQ, "u", "p")
		if len(got) != 1 || got[0] != rows[0] {
			t.Fatalf("volatility %+v", got)
		}
		k := contentstore.DataKey{Tenant: tenantA, Purpose: "enc", Version: 1, KEKID: "local:x", Wrapped: []byte{7, 7}, CreatedAt: t0}
		if ok, err := tx.InsertDataKey(ctx, k); err != nil || !ok {
			t.Fatalf("data key insert %v %v", ok, err)
		}
		if ok, err := tx.InsertDataKey(ctx, k); err != nil || ok {
			t.Fatalf("duplicate data key inserted: %v %v", ok, err)
		}
		ks, _ := tx.DataKeys(ctx)
		if len(ks) != 1 || !bytes.Equal(ks[0].Wrapped, k.Wrapped) || !ks[0].CreatedAt.Equal(t0) {
			t.Fatalf("data keys %+v", ks)
		}
		return nil
	})
}

// harness builds a Store over r with memory chunks and index.
func harness(t *testing.T, r contentstore.Repo, clock *testkit.FakeClock, limits contentstore.Limits) *contentstore.Store {
	t.Helper()
	kek := &contentstore.LocalKEK{KeyID: "suite", Rng: core.NewSplitMix64(3)}
	for i := range kek.Key {
		kek.Key[i] = byte(i)
	}
	s, err := contentstore.New(contentstore.Config{Repo: r, Chunks: contentstore.NewChunkStores(chunkstore.NewMemBackend(), chunkstore.Config{}),
		Keys: &contentstore.Keyring{Repo: r, KEK: kek, Rng: core.NewSplitMix64(5), Clock: clock}, Index: &contentstore.Indexes{},
		Clock: clock, DefaultLimits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func spa(t *testing.T, name string) []byte { return testkit.ReadFile(t, "content/sites/spa", name) }

func spaHeaders(t *testing.T, v string) []content.Header {
	var hs []content.Header
	for _, l := range strings.Split(strings.TrimSpace(string(spa(t, v+".headers.txt"))), "\n") {
		k, val, _ := strings.Cut(l, ":")
		hs = append(hs, content.Header{Name: strings.TrimSpace(k), Value: strings.TrimSpace(val)})
	}
	return hs
}

func page(t *testing.T, v string, at time.Time) content.Capture {
	return content.Capture{URL: "https://shop.example/", Profile: content.DefaultProfile(), Status: 200, Headers: spaHeaders(t, v),
		Body: spa(t, v+".index.html"), FetchedAt: at, Outcome: content.OutcomeComplete}
}

// storeFlow drives the store over the adapter: ingest, idempotence,
// stored rows equal the results, diffs, raw reads, search, pinning,
// retention and GC.
func storeFlow(t *testing.T, r contentstore.Repo) {
	ctx := context.Background()
	clock := testkit.NewFakeClock(t0.AddDate(0, 0, 30))
	s := harness(t, r, clock, contentstore.Limits{})
	var res []contentstore.IngestResult
	for i, v := range []string{"v1", "v2", "v3"} {
		c := page(t, v, t0.AddDate(0, 0, 7*i))
		got, err := s.Ingest(ctx, tenantA, contentstore.IngestRequest{Program: programP, Capture: c, RunID: "run_1"})
		if err != nil {
			t.Fatal(err)
		}
		res = append(res, got)
		again, err := s.Ingest(ctx, tenantA, contentstore.IngestRequest{Program: programP, Capture: c})
		if err != nil || !again.Duplicate || again.Snapshot.ID != got.Snapshot.ID {
			t.Fatalf("re-ingest %s: %+v %v", v, again.Snapshot.ID, err)
		}
	}
	for _, x := range res {
		got, err := s.GetSnapshot(ctx, tenantA, x.Snapshot.ID)
		if err != nil {
			t.Fatal(err)
		}
		a, _ := core.CanonicalJSON(got)
		b, _ := core.CanonicalJSON(x.Snapshot)
		if !bytes.Equal(a, b) {
			t.Fatalf("stored snapshot differs from the ingest result:\n%s\n%s", a, b)
		}
	}
	v, err := s.Diff(ctx, tenantA, res[1].Snapshot.ID, res[2].Snapshot.ID)
	if err != nil || v.Stored == nil || v.ChangeSet.Outcome != content.DiffChanged {
		t.Fatalf("diff v2→v3: %v %+v", err, v.Stored)
	}
	raw, err := s.SnapshotContent(ctx, tenantA, res[2].Snapshot.ID, contentstore.ContentOptions{Raw: true})
	if err != nil || !bytes.Equal(raw.Body, spa(t, "v3.index.html")) {
		t.Fatalf("raw read: %v", err)
	}
	red, err := s.SnapshotContent(ctx, tenantA, res[2].Snapshot.ID, contentstore.ContentOptions{Body: true})
	if err != nil || bytes.Contains(red.Body, []byte("aws_secret_access_key:\"wJal")) || len(red.Findings) == 0 {
		t.Fatalf("redacted read: %v (%d findings)", err, len(red.Findings))
	}
	hits, err := s.Search(ctx, tenantA, programP, "kind:form_added", contentstore.SearchOptions{})
	if err != nil || hits.Total == 0 {
		t.Fatalf("search: %v %+v", err, hits)
	}
	if _, err := s.Pin(ctx, tenantA, res[0].Snapshot.ID, "reviewed"); err != nil {
		t.Fatal(err)
	}
	// Retention with KeepLast 1 and the clock 30 days on keeps the pinned
	// v1 and the newest v3, and deletes v2 with both its diffs.
	s2, err := contentstore.New(func() contentstore.Config {
		c := s.Config()
		c.Retention = contentstore.RetentionPolicy{KeepLast: 1, KeepFor: 24 * time.Hour}
		return c
	}())
	if err != nil {
		t.Fatal(err)
	}
	rep, err := s2.ApplyRetention(ctx, tenantA)
	if err != nil || rep.Snapshots != 1 || rep.Diffs != 2 {
		t.Fatalf("retention: %+v %v", rep, err)
	}
	if _, err := s2.GetSnapshot(ctx, tenantA, res[1].Snapshot.ID); !errors.Is(err, contentstore.ErrNotFound) {
		t.Fatalf("v2 after retention: %v", err)
	}
	for _, keep := range []contentstore.IngestResult{res[0], res[2]} {
		c, err := s2.SnapshotContent(ctx, tenantA, keep.Snapshot.ID, contentstore.ContentOptions{Raw: true, Features: true})
		if err != nil || len(c.Body) == 0 {
			t.Fatalf("survivor unreadable: %v", err)
		}
	}
	if _, err := s2.GC(ctx, tenantA); err != nil {
		t.Fatal(err)
	}
	q, err := s2.Quota(ctx, tenantA)
	if err != nil || q.Usage.StoredBytes == 0 || q.Usage.IndexBytes == 0 {
		t.Fatalf("quota status %+v %v", q, err)
	}
}

// concurrency: concurrent ingests of one capture store it once; with a
// captures-per-day limit exactly the limit is admitted.
func concurrency(t *testing.T, open func(t *testing.T) contentstore.Repo) {
	ctx := context.Background()
	clock := testkit.NewFakeClock(t0.Add(time.Hour))
	s := harness(t, open(t), clock, contentstore.Limits{CapturesPerDay: 4})
	c := page(t, "v1", t0)
	var wg sync.WaitGroup
	var mu sync.Mutex
	stored, admitted, refused := 0, 0, 0
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cc := c
			if i%2 == 1 {
				cc.URL = fmt.Sprintf("https://shop.example/p%d", i)
			}
			r, err := s.Ingest(ctx, tenantA, contentstore.IngestRequest{Program: programP, Capture: cc})
			mu.Lock()
			defer mu.Unlock()
			var ce *core.Error
			switch {
			case err == nil && i%2 == 0 && !r.Duplicate:
				stored++
			case err == nil && i%2 == 1:
				admitted++
			case err == nil:
			case errors.As(err, &ce) && ce.Reason == contentstore.ReasonQuotaExceeded:
				refused++
			default:
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	q, _ := s.Quota(ctx, tenantA)
	if stored > 1 || int64(stored+admitted) != q.Usage.Captures || q.Usage.Captures != 4 || stored+admitted+refused > 12 {
		t.Fatalf("stored %d (same capture), admitted %d, refused %d, counted %d; want at most 1 store and exactly 4 counted",
			stored, admitted, refused, q.Usage.Captures)
	}
}
