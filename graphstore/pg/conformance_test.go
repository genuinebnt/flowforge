package pg_test

import (
	"testing"

	"github.com/genuinebnt/flowforge/graphstore/storetest"
	"github.com/genuinebnt/flowforge/testkit"
)

// Store conformance (G-SPEC-07) on Postgres, with Mem as the oracle: the
// same conformance/graphstore cases as graphstore/conformance_test.go.

func graphCase(t *testing.T, id string) storetest.GraphCase {
	t.Helper()
	dsn(t)
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
	storetest.GraphConformance(t, pgFactory, graphCase(t, "001"), memFactory)
}
func TestGraph002_AtomicReject(t *testing.T) {
	storetest.GraphConformance(t, pgFactory, graphCase(t, "002"), memFactory)
}
func TestGraph003_CoveringRunAsOf(t *testing.T) {
	storetest.GraphConformance(t, pgFactory, graphCase(t, "003"), memFactory)
}
func TestGraph004_AuthorityAsOf(t *testing.T) {
	storetest.GraphConformance(t, pgFactory, graphCase(t, "004"), memFactory)
}
func TestGraph005_ContentChanged(t *testing.T) {
	storetest.GraphConformance(t, pgFactory, graphCase(t, "005"), memFactory)
}
func TestGraph006_TagWithdrawAsOf(t *testing.T) {
	storetest.GraphConformance(t, pgFactory, graphCase(t, "006"), memFactory)
}
