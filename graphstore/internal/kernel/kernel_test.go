package kernel

import (
	"cmp"
	"maps"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore/storetest"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/testkit"
)

// store simulates the Postgres tables: rows and tag rows by record id.
type store struct {
	entities map[string]EntityRow
	edges    map[string]EdgeRow
	tags     map[string][]StoredTag
	scope    oam.ScopeFunc
}

func (s *store) load(t *testing.T, tenant, prog string, entIDs, edgeIDs []string) *State {
	st := NewState()
	for _, id := range entIDs {
		if row, ok := s.entities[id]; ok {
			e, err := LoadEntity(tenant, prog, row, s.tags[id])
			if err != nil {
				t.Fatal(err)
			}
			st.Entities[id] = e
		}
	}
	for _, id := range edgeIDs {
		if row, ok := s.edges[id]; ok {
			e, err := LoadEdge(tenant, prog, row, s.tags[id])
			if err != nil {
				t.Fatal(err)
			}
			st.Edges[id] = e
		}
	}
	return st
}

func (s *store) save(t *testing.T, st *State) {
	for id, e := range st.Entities {
		if !e.Dirty {
			continue
		}
		row, err := e.Row()
		if err != nil {
			t.Fatal(err)
		}
		tags, err := e.StoredTags()
		if err != nil {
			t.Fatal(err)
		}
		s.entities[id], s.tags[id] = row, tags
	}
	for id, e := range st.Edges {
		if !e.Dirty {
			continue
		}
		row, err := e.Row()
		if err != nil {
			t.Fatal(err)
		}
		tags, err := e.StoredTags()
		if err != nil {
			t.Fatal(err)
		}
		s.edges[id], s.tags[id] = row, tags
	}
}

func (s *store) views(t *testing.T, tenant, prog string) ([]oam.Entity, []oam.Edge) {
	st := s.load(t, tenant, prog, slices.Sorted(maps.Keys(s.entities)), slices.Sorted(maps.Keys(s.edges)))
	ents := []oam.Entity{}
	for _, id := range slices.Sorted(maps.Keys(st.Entities)) {
		ents = append(ents, st.Entities[id].View())
	}
	edges := []oam.Edge{}
	for _, id := range slices.Sorted(maps.Keys(st.Edges)) {
		edges = append(edges, st.Edges[id].View())
	}
	return ents, edges
}

func canon(t *testing.T, v any) string {
	b, err := core.CanonicalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func flatEvents(evs []oam.ChangeEvent) []Event {
	var out []Event
	for _, ev := range evs {
		out = append(out, Event{ID: ev.ID, Type: ev.Type, EntityID: ev.EntityID, EdgeID: ev.EdgeID, RunID: ev.RunID, Patch: ev.Patch, At: ev.At})
	}
	return out
}

// TestGraphKernel_MatchesOAM checks that the kernel, with every record
// loaded from and saved to the storage encoding around each operation as
// the Postgres store does, reproduces oam.Graph exactly: results, change
// events (ids included) and final state.
func TestGraphKernel_MatchesOAM(t *testing.T) {
	r := testkit.Rng(t)
	tenant, prog := storetest.TenantID("k"), storetest.ProgramID("k")
	scope := func(_, _ string, a oam.Asset) oam.ScopeStatus {
		if s, ok := a.(oam.Service); ok && s.ServiceType == "https" {
			return oam.ScopeInScope
		}
		return oam.ScopeUnknown
	}
	cases := testkit.Cases() / 5
	for c := range cases {
		g := oam.NewGraph(oam.Config{Scope: scope, StaleAfterRuns: 2})
		s := &store{entities: map[string]EntityRow{}, edges: map[string]EdgeRow{}, tags: map[string][]StoredTag{}}
		applied := map[string]bool{}
		n := core.IntN(r, 25)
		for i := range n {
			if core.IntN(r, 5) == 0 {
				cr := storetest.RandomCoveringRun(r, i)
				want, err := g.CompleteRun(oam.CoveringRun{TenantID: tenant, ProgramID: prog, RunID: cr.Run, Tool: cr.Tool, CompletedAt: cr.At})
				if err != nil {
					t.Fatal(err)
				}
				if want.Duplicate {
					continue
				}
				var ids, eids []string
				for id, rows := range s.tags {
					for _, tg := range rows {
						if tg.PType == PTypeRun && tg.Name == cr.Tool {
							if _, ok := s.entities[id]; ok {
								ids = append(ids, id)
							} else {
								eids = append(eids, id)
							}
						}
					}
				}
				st := s.load(t, tenant, prog, ids, eids)
				ne, nd, evs := CoverRun(st, tenant, prog, cr.Tool, cr.Run, nil, 2, cr.At)
				s.save(t, st)
				if ne != want.EntitiesMarked || nd != want.EdgesMarked {
					t.Fatalf("case %d: cover marked %d/%d, oam %d/%d", c, ne, nd, want.EntitiesMarked, want.EdgesMarked)
				}
				if a, b := canon(t, evs), canon(t, flatEvents(want.Changes)); a != b {
					t.Fatalf("case %d: cover events\n%s\n%s", c, a, b)
				}
				continue
			}
			o := storetest.RandomObservation(r, core.IntN(r, n+1), tenant, prog)
			want, werr := g.Apply(o)
			p, err := Prepare(o)
			if err != nil {
				if werr == nil {
					t.Fatalf("case %d: prepare failed, oam accepted: %v", c, err)
				}
				continue
			}
			if applied[o.ObservationID] {
				continue
			}
			st := s.load(t, tenant, prog, p.EntityIDs(), p.EdgeIDs())
			got, err := Apply(st, p, scope)
			if (err != nil) != (werr != nil) {
				t.Fatalf("case %d: kernel error %v, oam error %v", c, err, werr)
			}
			if err != nil {
				continue
			}
			applied[o.ObservationID] = true
			s.save(t, st)
			if got.EntitiesCreated != want.EntitiesCreated || got.EntitiesUpdated != want.EntitiesUpdated ||
				got.EdgesCreated != want.EdgesCreated || got.EdgesUpdated != want.EdgesUpdated {
				t.Fatalf("case %d: counts %+v, oam %+v", c, got, want)
			}
			if a, b := canon(t, got.Events), canon(t, flatEvents(want.Changes)); a != b {
				t.Fatalf("case %d: events\n%s\n%s", c, a, b)
			}
		}
		ents, edges := s.views(t, tenant, prog)
		if a, b := canon(t, ents), canon(t, g.Entities()); a != b {
			t.Fatalf("case %d: entities\nkernel %s\noam    %s", c, a, b)
		}
		if a, b := canon(t, edges), canon(t, g.Edges()); a != b {
			t.Fatalf("case %d: edges\nkernel %s\noam    %s", c, a, b)
		}
	}
}

// TestGraphKernel_StorageRoundTrip checks that a record survives the
// storage encoding unchanged, hidden tags included.
func TestGraphKernel_StorageRoundTrip(t *testing.T) {
	r := testkit.Rng(t)
	tenant, prog := storetest.TenantID("k"), storetest.ProgramID("k")
	for range 50 {
		st := NewState()
		for i := range 10 {
			p, err := Prepare(storetest.RandomObservation(r, i, tenant, prog))
			if err != nil {
				continue
			}
			_, _ = Apply(st, p, nil)
		}
		for _, e := range st.Entities {
			row, _ := e.Row()
			tags, _ := e.StoredTags()
			back, err := LoadEntity(tenant, prog, row, tags)
			if err != nil {
				t.Fatal(err)
			}
			back.Dirty = e.Dirty
			if a, b := canon(t, back.View()), canon(t, e.View()); a != b {
				t.Fatalf("round trip\n%s\n%s", a, b)
			}
			if !maps.EqualFunc(back.Content.Stamps, e.Content.Stamps, func(a, b oam.Stamp) bool { return a.Compare(b) == 0 }) {
				t.Fatal("stamps lost")
			}
			if cmp.Compare(back.First.Stamp.Observation, e.First.Stamp.Observation) != 0 || back.Last.Run != e.Last.Run {
				t.Fatal("sightings lost")
			}
		}
	}
}
