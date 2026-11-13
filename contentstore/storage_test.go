package contentstore

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/chunkstore"
	"github.com/genuinebnt/flowforge/content"
)

// TestCStoreSnapshotStorage: SnapshotStorage reports each snapshot's
// raw records (capture record + redacted body) and derived records
// (normalized text + features) exactly as the chunk store's manifest
// stats give them, so callers outside the package (the V2 lab) measure
// CSTORE-INV-08's quantity; StorageMetrics is the tenant's whole store.
func TestCStoreSnapshotStorage(t *testing.T) {
	h := newHarness(t)
	res := h.ingestSPA(tenantA)
	tc, _ := h.chunks.open(bg, tenantA)
	stat := func(ref string) int64 {
		if ref == "" {
			return 0
		}
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
	var raw, derived int64
	for _, r := range res {
		got, err := h.store.SnapshotStorage(bg, tenantA, r.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		wantRaw := stat(r.Snapshot.HeadersRef) + stat(r.Snapshot.BodyManifest)
		wantDerived := stat(r.Snapshot.NormalizedManifest) + stat(r.Snapshot.FeaturesRef)
		if got.Raw != wantRaw || got.Derived != wantDerived || got.Raw == 0 || got.Derived == 0 {
			t.Fatalf("%s: storage %+v, want raw %d derived %d", r.Snapshot.URL, got, wantRaw, wantDerived)
		}
		raw, derived = raw+got.Raw, derived+got.Derived
	}
	m, err := h.store.StorageMetrics(bg, tenantA)
	if err != nil {
		t.Fatal(err)
	}
	// Diff records are in the store too, so the whole exceeds the
	// snapshots' records.
	if int64(m.CompressedBytes) <= raw+derived {
		t.Fatalf("store holds %d bytes, snapshots' records %d", m.CompressedBytes, raw+derived)
	}
}

// TestCStoreRegress_LineagePrefersPageScripts: renamed bundles of three
// deploys keep their old URLs as open versions (nothing re-fetches them).
// The third bundle's predecessor is the one the page's previous version
// loaded, not an older generation with the same lineage key that SimHash
// might rank closer: its diff shows only the third deploy's new endpoint.
func TestCStoreRegress_LineagePrefersPageScripts(t *testing.T) {
	h := newHarness(t)
	const site = "https://shop.example"
	js := []content.Header{{Name: "Content-Type", Value: "application/javascript"}}
	html := []content.Header{{Name: "Content-Type", Value: "text/html"}}
	page := func(bundle string) []byte {
		return []byte(`<!DOCTYPE html><html><head><title>Shop</title><script defer src="/static/js/` + bundle + `"></script></head><body><a href="/a">a</a></body></html>`)
	}
	third := strings.Replace(string(spaFile(t, "main.5b6c7d8e.js")), `t.exportAll=function(){return fetch("/api/v1/admin/export",{method:"POST"})}`,
		`t.exportAll=function(){return fetch("/api/v1/admin/export",{method:"POST"})};t.dropReport=function(e){return fetch("/api/v2/reports/"+e,{method:"DELETE"})}`, 1)
	bundles := []struct{ name, body string }{
		{"main.3f9a1c2b.js", string(spaFile(t, "main.3f9a1c2b.js"))},
		{"main.5b6c7d8e.js", string(spaFile(t, "main.5b6c7d8e.js"))},
		{"main.c4d5e6f7.js", third},
	}
	var got []IngestResult
	for i, b := range bundles {
		at := t0.AddDate(0, 0, 7*i)
		h.ingest(tenantA, capture(site+"/", page(b.name), html, at), "")
		got = append(got, h.ingest(tenantA, capture(site+"/static/js/"+b.name, []byte(b.body), js, at), site+"/"))
	}
	if got[2].Lineage == nil || got[2].Lineage.URL != site+"/static/js/main.5b6c7d8e.js" || got[2].Snapshot.LineageParentID != got[1].Snapshot.ID {
		t.Fatalf("third bundle's lineage %+v, want main.5b6c7d8e.js", got[2].Lineage)
	}
	var changes []string
	for _, c := range got[2].ChangeSet.Changes {
		changes = append(changes, c.Op+" "+string(c.Feature.Kind)+"|"+c.Feature.Key)
	}
	if want := []string{"added endpoint|ep:DELETE " + site + "/api/v2/reports/{expr}"}; !slices.Equal(changes, want) {
		t.Fatalf("third bundle's changes %v, want %v", changes, want)
	}
}

// TestCStoreEventExcerpt: the content event of a level-3 change carries
// the change set's levels and a bounded unified excerpt (the level-3
// artifacts' hunks first) whose secret lines are redacted; baselines and
// non-changes carry none.
func TestCStoreEventExcerpt(t *testing.T) {
	h := newHarness(t)
	res := h.ingestSPA(tenantA)
	payloads := map[string]EventPayload{}
	for _, e := range h.mem.OutboxEvents() {
		var env struct {
			Payload EventPayload `json:"payload"`
		}
		if err := json.Unmarshal(e.Payload, &env); err != nil {
			t.Fatal(err)
		}
		payloads[env.Payload.SnapshotID] = env.Payload
	}
	v3 := payloads[res[5].Snapshot.PublicID()] // page v3: the planted changes
	lines := strings.Split(strings.TrimSuffix(v3.Excerpt, "\n"), "\n")
	if !slices.Equal(v3.Levels, []string{"hash", "text", "semantic"}) || len(lines) == 0 || len(lines) > maxExcerptLines ||
		len(v3.Excerpt) > maxExcerptBytes || lines[0] != "--- "+spaPage || !strings.HasPrefix(lines[1], "@@ ") {
		t.Fatalf("v3 excerpt (%d lines, %d bytes):\n%s", len(lines), len(v3.Excerpt), v3.Excerpt)
	}
	if v3.Excerpt != Excerpt(*res[5].ChangeSet) || !strings.Contains(v3.Excerpt, "[REDACTED:aws_access_key_id:") ||
		strings.Contains(v3.Excerpt, "EXAMPLEKY") {
		t.Fatalf("v3 excerpt not the redacted change set excerpt:\n%s", v3.Excerpt)
	}
	for _, i := range []int{0, 3, 4} { // baseline, masked-only page v2, reordered bundle
		if p := payloads[res[i].Snapshot.PublicID()]; p.Excerpt != "" {
			t.Fatalf("%s (%s) carries an excerpt:\n%s", p.URL, p.Outcome, p.Excerpt)
		}
	}
}
