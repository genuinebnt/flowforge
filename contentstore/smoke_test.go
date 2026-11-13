package contentstore

import (
	"context"
	"testing"
)

func TestCStoreSmoke(t *testing.T) {
	h := newHarness(t)
	for _, v := range spaVersions(t) {
		r := h.ingest(tenantA, v.C, v.Page)
		cs := "-"
		if r.ChangeSet != nil {
			cs = r.ChangeSet.Outcome + "/" + r.ChangeSet.Class
		}
		lin := ""
		if r.Lineage != nil {
			lin = r.Lineage.URL + " by " + r.Lineage.By
		}
		var docs int
		if r.Diff != nil {
			docs = r.Diff.IndexDocs
		}
		t.Logf("%s %s stored=%d raw=%d %s docs=%d lineage=%s", v.C.URL, r.Snapshot.Kind, r.Snapshot.StoredSize, r.Snapshot.RawSize, cs, docs, lin)
	}
	m, _ := h.chunks.Metrics(context.Background(), tenantA)
	t.Logf("metrics %+v", m)
	res, err := h.store.Search(context.Background(), tenantA, programP, "kind:endpoint_added", SearchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, hit := range res.Hits {
		t.Logf("hit %s %s %s %q", hit.Kind, hit.Side, hit.URL, hit.Text)
	}
}
