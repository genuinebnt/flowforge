package contentstore

import (
	"bytes"
	"testing"

	"github.com/genuinebnt/flowforge/content"
)

// upgradedHTML is a pipeline whose html normalizer writes script start
// tags in upper case (a stand-in for an html normalizer upgrade).
func upgradedHTML() Pipeline {
	return Pipeline{Stamp: "test/html-v2", Process: func(c *content.Capture, o content.ProcessOptions) (*content.Processed, error) {
		p, err := content.Process(c, o)
		if err != nil {
			return nil, err
		}
		for i := range p.Artifacts {
			if a := &p.Artifacts[i]; a.Kind == content.KindHTML {
				a.Normalized.Text = bytes.ReplaceAll(a.Normalized.Text, []byte("<script"), []byte("<SCRIPT"))
				a.Normalized.Version = "html@99"
			}
		}
		return p, nil
	}}
}

// rederiveBase ingests the SPA pages v1 and v2 (v2 a delta frame against
// v1), then re-derives only v1 under upgradedHTML, which re-encodes v2
// against v1's new blobs. It returns the upgraded store and the rows of
// v1 and v2 as they were before.
func rederiveBase(t *testing.T) (*harness, *Store, Snapshot, Snapshot) {
	h := newHarness(t)
	vs := spaVersions(t)
	h.ingest(tenantA, vs[0].C, "")
	r2 := h.ingest(tenantA, vs[3].C, "")
	v2, err := h.store.GetSnapshot(bg, tenantA, r2.Snapshot.ID)
	if err != nil || v2.DeltaBase.IsZero() {
		t.Fatalf("fixture: v2 is not a delta frame (%v)", err)
	}
	v1, err := h.store.GetSnapshot(bg, tenantA, v2.DeltaBase)
	if err != nil {
		t.Fatal(err)
	}
	cfg := h.store.Config()
	cfg.Pipeline = upgradedHTML()
	up, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, rep, err := up.rederiveOnce(bg, tenantA, v1.ID); err != nil || rep.Rewritten != 1 {
		t.Fatalf("re-derive v1: %+v %v", rep, err)
	}
	return h, up, v1, v2
}

// TestRegress_PlainReadAcrossBaseRederive: a snapshot row read before its
// delta base was re-derived (which re-encodes the snapshot against the
// base's new blobs) still decodes: snapshotPlain reads the base's current
// row, so the old frame failed with RECORD_CHECKSUM; it now re-reads the
// row and decodes the current one. Concurrent re-derive jobs hit this
// (CSTORE-INV-14's three-job case).
func TestRegress_PlainReadAcrossBaseRederive(t *testing.T) {
	h, up, _, v2 := rederiveBase(t)
	tc, err := h.chunks.open(bg, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	up.cache = newBlobCache(up.cfg.CacheBytes) // decode, do not hit an entry the re-derive cached
	got, err := up.snapshotPlain(bg, tc, v2)
	if err != nil {
		t.Fatalf("stale row: %v", err)
	}
	cur, err := up.GetSnapshot(bg, tenantA, v2.ID)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := up.snapshotPlain(bg, tc, cur)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Normalized, fresh.Normalized) || !bytes.Equal(got.Features, fresh.Features) || !bytes.Equal(got.Body, fresh.Body) {
		t.Fatal("stale row decodes to other plain blobs than the current row")
	}
}

// TestRegress_BlobCacheKeyedByRefs: the plain-blob cache never serves a
// snapshot's old blobs for its re-derived row. A reader that loaded the
// old row before a re-derive committed can put its entry after the
// re-derive dropped the id; the cache was keyed by id alone, so the new
// row then decoded to the old blobs (and a delta dependent re-encoded
// against them would reference a dictionary the store no longer has).
func TestRegress_BlobCacheKeyedByRefs(t *testing.T) {
	h, up, v1, _ := rederiveBase(t)
	tc, err := h.chunks.open(bg, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	old, err := up.snapshotPlain(bg, tc, v1) // the late reader: old row, entry put after the drop
	if err != nil {
		t.Fatal(err)
	}
	cur, err := up.GetSnapshot(bg, tenantA, v1.ID)
	if err != nil {
		t.Fatal(err)
	}
	got, err := up.snapshotPlain(bg, tc, cur)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got.Normalized, old.Normalized) || !bytes.Contains(got.Normalized, []byte("<SCRIPT")) {
		t.Fatal("re-derived row decoded to the old normalized text from the cache")
	}
}
