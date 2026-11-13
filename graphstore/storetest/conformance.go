package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/oam"
)

// GraphFile is the schema of conformance/graphstore/*.json (spec Part II
// 28.8, G-SPEC-07). It extends the conformance/oam/merge schema
// (MergeFile: steps, scope, stale_after_runs, expect) with store-level
// operations and as-of reads:
//
//   - a step may be {"content_change": {...}} (RecordContentChange);
//     with "repeat": true it must return the event of the identical
//     earlier request and append nothing;
//   - "as_of" lists reads evaluated, after all steps, at the change seq
//     reached after the first after_step steps: entities by key (exists
//     false means NOT_FOUND) and neighbourhoods by key.
//
// Seqs are store-local, so the data never names one; it names steps.
type GraphFile struct {
	ID    string      `json:"id"`
	Steps []GraphStep `json:"steps"`
	AsOf  []AsOfRead  `json:"as_of"`
}

// GraphStep carries the step kinds GraphFile adds to MergeFile.
type GraphStep struct {
	ContentChange *struct {
		Entity string                 `json:"entity"` // key of an entity an earlier step created
		RunID  string                 `json:"run_id"`
		At     time.Time              `json:"at"`
		Patch  oam.ContentChangePatch `json:"patch"`
		Repeat bool                   `json:"repeat"`
		Error  string                 `json:"error"` // "invalid_argument" or "not_found"
	} `json:"content_change"`
}

// AsOfRead is one as-of checkpoint.
type AsOfRead struct {
	AfterStep int `json:"after_step"`
	Entities  []struct {
		Key         string          `json:"key"`
		Exists      bool            `json:"exists"`
		Content     map[string]any  `json:"content"`
		State       oam.State       `json:"state"`
		ScopeStatus oam.ScopeStatus `json:"scope_status"`
		Tags        []ExpTag        `json:"tags"`
	} `json:"entities"`
	Neighbors []struct {
		Key       string   `json:"key"`
		Direction string   `json:"direction"` // out, in or both
		Depth     int      `json:"depth"`
		Labels    []string `json:"labels"`
		Entities  []string `json:"entities"` // "<key> <state>", in (distance, id) order is not required: compared as sets
		Edges     []string `json:"edges"`    // "<from key> <label> <to key> <state>"
	} `json:"neighbors"`
}

// GraphCase is one decoded conformance/graphstore file.
type GraphCase struct {
	Name  string
	Merge MergeFile
	Graph GraphFile
}

// DecodeGraphCase decodes one conformance/graphstore file into both
// views.
func DecodeGraphCase(name string, b []byte) (GraphCase, error) {
	c := GraphCase{Name: name}
	if err := json.Unmarshal(b, &c.Merge); err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c.Graph); err != nil {
		return c, err
	}
	if len(c.Graph.Steps) != len(c.Merge.Steps) {
		return c, fmt.Errorf("%s: step lists disagree", name)
	}
	return c, nil
}

var kinds = map[string]core.Kind{"invalid_argument": core.KindInvalidArgument, "not_found": core.KindNotFound}

// GraphConformance replays one conformance/graphstore case against a
// store from f: the merge expectation (entities, edges, tags, the full
// change log) is checked exactly as MergeConformance does, then every
// as_of read. Unless the case is order dependent (which every case with
// covering runs or content changes must be), the reversed and duplicated
// observation list must converge to the same state. With an oracle, the
// final canonical state must also equal the oracle's byte for byte.
func GraphConformance(t *testing.T, f Factory, c GraphCase, oracle Factory) {
	t.Helper()
	m, g := c.Merge, c.Graph
	if g.ID == "" {
		t.Fatalf("%s: no id", c.Name)
	}
	r := replayGraph(t, f, c)
	checkMerge(t, r.mergeRun, m)
	ex := r.e.export()
	byKey := map[string]graphstore.Entity{}
	byID := map[string]graphstore.Entity{}
	for _, e := range ex.Entities {
		byKey[e.Key], byID[e.ID] = e, e
	}
	edgeKeys := map[string]string{}
	for _, e := range ex.Edges {
		edgeKeys[e.ID] = byID[e.FromEntityID].Key + " " + e.Label + " " + byID[e.ToEntityID].Key
	}
	for ai, a := range g.AsOf {
		if a.AfterStep < 1 || a.AfterStep > len(r.seqs) {
			t.Fatalf("as_of[%d]: after_step %d out of range", ai, a.AfterStep)
		}
		seq := r.seqs[a.AfterStep-1]
		if seq == 0 {
			t.Fatalf("as_of[%d]: no change event yet after step %d", ai, a.AfterStep)
		}
		for _, x := range a.Entities {
			where := fmt.Sprintf("as_of[%d] (after step %d) entity %s", ai, a.AfterStep, x.Key)
			cur, ok := byKey[x.Key]
			if !ok {
				t.Fatalf("%s: no such entity in the final state", where)
			}
			got, err := r.e.s.GetEntityAsOf(r.e.ctx, r.e.c, graphstore.GetEntityAsOfRequest{EntityID: cur.ID, Seq: seq, IncludeTags: true})
			if !x.Exists {
				if !isNotFound(err) {
					t.Errorf("%s: want NOT_FOUND, got %v", where, err)
				}
				continue
			}
			if err != nil {
				t.Errorf("%s: %v", where, err)
				continue
			}
			if x.Content != nil {
				eq(t, where+" content", Canon(t, got.Content), Canon(t, x.Content))
			}
			optEq(t, where+" state", string(got.State), string(x.State))
			optEq(t, where+" scope_status", string(got.ScopeStatus), string(x.ScopeStatus))
			if x.Tags != nil {
				eq(t, where+" tags", Canon(t, otherTags(got.Tags)), Canon(t, x.Tags))
			}
		}
		for _, x := range a.Neighbors {
			where := fmt.Sprintf("as_of[%d] (after step %d) neighbors of %s", ai, a.AfterStep, x.Key)
			cur, ok := byKey[x.Key]
			if !ok {
				t.Fatalf("%s: no such entity in the final state", where)
			}
			dir := map[string]graphstore.Direction{"out": graphstore.DirectionOut, "in": graphstore.DirectionIn, "both": graphstore.DirectionBoth}[x.Direction]
			sg, err := r.e.s.NeighborsAsOf(r.e.ctx, r.e.c, graphstore.NeighborsAsOfRequest{Seq: seq,
				NeighborsRequest: graphstore.NeighborsRequest{EntityID: cur.ID, Direction: dir, Depth: x.Depth, Labels: x.Labels}})
			if err != nil {
				t.Errorf("%s: %v", where, err)
				continue
			}
			ents, edges := []string{}, []string{}
			for _, e := range sg.Entities {
				ents = append(ents, e.Key+" "+string(e.State))
			}
			for _, e := range sg.Edges {
				edges = append(edges, edgeKeys[e.ID]+" "+string(e.State))
			}
			slices.Sort(ents)
			slices.Sort(edges)
			want := func(s []string) []string {
				s = slices.Clone(s)
				if s == nil {
					s = []string{}
				}
				slices.Sort(s)
				return s
			}
			eq(t, where+" entities", Canon(t, ents), Canon(t, want(x.Entities)))
			eq(t, where+" edges", Canon(t, edges), Canon(t, want(x.Edges)))
		}
	}
	got := Canon(t, ex)
	if oracle != nil {
		o := replayGraph(t, oracle, c)
		if want := Canon(t, o.e.export()); got != want {
			t.Fatalf("state differs from the oracle:\n got %s\nwant %s", got, want)
		}
	}
	if m.OrderDependent {
		return
	}
	for i, s := range m.Steps {
		if s.CompleteRun != nil || g.Steps[i].ContentChange != nil {
			t.Fatalf("step %d: a case with covering runs or content changes must be order_dependent", i)
		}
	}
	e2 := newEnv(t, f, Options{Scope: scopeOf(m), StaleAfterRuns: m.StaleAfterRuns})
	rev := slices.Clone(r.applied)
	slices.Reverse(rev)
	for _, req := range append(rev, r.applied...) {
		if _, err := e2.s.IngestObservation(e2.ctx, req); err != nil {
			t.Fatalf("replay %s: %v", req.Observation.ObservationID, err)
		}
	}
	e2.tenant, e2.prog, e2.c = r.e.tenant, r.e.prog, r.e.c
	if again := Canon(t, e2.export()); again != got {
		t.Fatalf("state depends on order:\n%s\n%s", got, again)
	}
}

type graphRun struct {
	*mergeRun
	seqs []int64 // last change seq after each step
}

func replayGraph(t *testing.T, f Factory, c GraphCase) *graphRun {
	t.Helper()
	m := c.Merge
	r := &graphRun{mergeRun: &mergeRun{t: t, e: newEnv(t, f, Options{Scope: scopeOf(m), StaleAfterRuns: m.StaleAfterRuns}), ids: map[string]string{}}}
	r.prescan(m)
	events := map[string]graphstore.ChangeEvent{} // canonical request -> first event
	var last int64
	for i, s := range m.Steps {
		switch cc := c.Graph.Steps[i].ContentChange; {
		case cc != nil:
			var id string
			for _, e := range r.e.export().Entities {
				if e.Key == cc.Entity {
					id = e.ID
				}
			}
			if id == "" && cc.Error != "not_found" {
				t.Fatalf("step %d: content change on unknown entity %s", i, cc.Entity)
			}
			req := graphstore.ContentChange{EntityID: id, RunID: RunID(cc.RunID), At: cc.At, Patch: cc.Patch}
			ev, err := r.e.s.RecordContentChange(r.e.ctx, r.e.c, req)
			if cc.Error != "" {
				wantKind(t, fmt.Sprintf("step %d", i), err, kinds[cc.Error])
				break
			}
			if err != nil {
				t.Fatalf("step %d: content change: %v", i, err)
			}
			k := Canon(t, req.Patch) + req.EntityID + req.RunID + req.At.UTC().Format(time.RFC3339Nano)
			first, seen := events[k]
			if seen != cc.Repeat {
				t.Fatalf("step %d: repeat %v, but an identical request was seen: %v", i, cc.Repeat, seen)
			}
			if seen && (first.ID != ev.ID || first.Seq != ev.Seq) {
				t.Fatalf("step %d: repeated content change gave event %s/%d, want %s/%d", i, ev.ID, ev.Seq, first.ID, first.Seq)
			}
			if !seen {
				events[k] = ev
			}
		case s.CompleteRun != nil:
			cr := s.CompleteRun
			r.use(cr.TenantID, cr.ProgramID)
			r.e.clock.Set(cr.CompletedAt)
			if _, err := r.e.s.SetStale(r.e.ctx, r.e.c, graphstore.SetStaleRequest{ProgramID: r.e.prog, RunID: RunID(cr.RunID),
				ToolName: cr.Tool, AssetTypes: cr.AssetTypes}); err != nil {
				t.Fatalf("step %d: set stale: %v", i, err)
			}
		default:
			pending, err := r.ingest(s.Observation)
			if err == nil && pending != s.Pending {
				t.Fatalf("step %d: pending %v, want %v", i, pending, s.Pending)
			}
			if s.Error != "" {
				want, ok := sentinels[s.Error]
				if !ok {
					t.Fatalf("step %d: unknown error name %q", i, s.Error)
				}
				if !errors.Is(err, want) {
					t.Fatalf("step %d: error %v, want %s", i, err, s.Error)
				}
			} else if err != nil {
				t.Fatalf("step %d: %v", i, err)
			}
		}
		if evs := r.e.changes(last); len(evs) > 0 {
			last = evs[len(evs)-1].Seq
		}
		r.seqs = append(r.seqs, last)
	}
	return r
}
