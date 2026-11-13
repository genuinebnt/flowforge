package graphstore_test

import (
	"testing"

	"github.com/genuinebnt/flowforge/graphstore/storetest"
	"github.com/genuinebnt/flowforge/testkit"
)

// Store conformance (G-SPEC-07): conformance/graphstore/gs_NNN_*.json,
// language neutral, run here against Mem and in graphstore/pg against
// Postgres with Mem as the oracle. Each required id of modules.toml has
// one test.

func graphCase(t *testing.T, id string) storetest.GraphCase {
	t.Helper()
	for _, c := range testkit.LoadCases[map[string]any](t, "graphstore") {
		if c.Data["id"] != "gs_"+id {
			continue
		}
		gc, err := storetest.DecodeGraphCase(c.Name, testkit.ReadFile(t, "graphstore", c.Name))
		if err != nil {
			t.Fatal(err)
		}
		return gc
	}
	t.Fatalf("conformance/graphstore: no case gs_%s", id)
	return storetest.GraphCase{}
}

func TestGraph001_ChangeLog(t *testing.T) {
	storetest.GraphConformance(t, memFactory, graphCase(t, "001"), nil)
}
func TestGraph002_AtomicReject(t *testing.T) {
	storetest.GraphConformance(t, memFactory, graphCase(t, "002"), nil)
}
func TestGraph003_CoveringRunAsOf(t *testing.T) {
	storetest.GraphConformance(t, memFactory, graphCase(t, "003"), nil)
}
func TestGraph004_AuthorityAsOf(t *testing.T) {
	storetest.GraphConformance(t, memFactory, graphCase(t, "004"), nil)
}
func TestGraph005_ContentChanged(t *testing.T) {
	storetest.GraphConformance(t, memFactory, graphCase(t, "005"), nil)
}
func TestGraph006_TagWithdrawAsOf(t *testing.T) {
	storetest.GraphConformance(t, memFactory, graphCase(t, "006"), nil)
}
