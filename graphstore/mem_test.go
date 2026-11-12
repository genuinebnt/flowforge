package graphstore_test

import (
	"testing"

	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/graphstore/storetest"
	"github.com/genuinebnt/flowforge/testkit"
)

func memFactory(_ testing.TB, o storetest.Options) graphstore.Store {
	return graphstore.NewMem(o.Config())
}

func mergeCases(t *testing.T) []storetest.MergeCase {
	var out []storetest.MergeCase
	for _, c := range testkit.LoadCases[storetest.MergeFile](t, "oam/merge") {
		out = append(out, storetest.MergeCase{Name: c.Name, Data: c.Data})
	}
	return out
}

func TestGraphIngest_MergeConformance(t *testing.T) {
	storetest.MergeConformance(t, memFactory, mergeCases(t), nil)
}
func TestGraphIngest_Idempotent(t *testing.T) { storetest.Idempotent(t, memFactory) }
func TestGraphIngest_Atomic(t *testing.T)     { storetest.Atomic(t, memFactory) }
func TestGraphProp_Convergence(t *testing.T) {
	storetest.Convergence(t, memFactory, testkit.Rng(t), testkit.Cases()/10, nil)
}
func TestGraphNeighbors(t *testing.T)              { storetest.Neighbors(t, memFactory) }
func TestGraphPath(t *testing.T)                   { storetest.Path(t, memFactory) }
func TestGraphBlastRadius_Components(t *testing.T) { storetest.BlastAndComponents(t, memFactory) }
func TestGraphFindEntities(t *testing.T)           { storetest.FindEntities(t, memFactory) }
func TestGraphSearch(t *testing.T)                 { storetest.Search(t, memFactory) }
func TestGraphDiff(t *testing.T)                   { storetest.Diff(t, memFactory) }
func TestGraphStaleness(t *testing.T)              { storetest.Staleness(t, memFactory) }
func TestGraphTenantIsolation(t *testing.T)        { storetest.TenantIsolation(t, memFactory) }
func TestGraphWatchChanges_Resume(t *testing.T)    { storetest.WatchResume(t, memFactory) }
func TestGraphSnapshot_Deterministic(t *testing.T) { storetest.Snapshot(t, memFactory, memFactory) }
func TestGraphAddTag(t *testing.T)                 { storetest.AddTag(t, memFactory) }

func BenchmarkSys01_Ingest(b *testing.B) {
	storetest.BenchIngest(b, graphstore.NewMem(graphstore.Config{}), 1)
}
