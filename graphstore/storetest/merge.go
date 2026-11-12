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

// MergeFile is the schema of conformance/oam/merge/*.json (spec 19
// graph/ingest): observation steps and covering runs with the expected
// entities, edges, confidences and change events.
type MergeFile struct {
	Description    string                     `json:"description"`
	StaleAfterRuns int                        `json:"stale_after_runs"`
	Scope          map[string]oam.ScopeStatus `json:"scope"`
	OrderDependent bool                       `json:"order_dependent"`
	Steps          []struct {
		Observation json.RawMessage `json:"observation"`
		CompleteRun *struct {
			TenantID    string    `json:"tenant_id"`
			ProgramID   string    `json:"program_id"`
			RunID       string    `json:"run_id"`
			Tool        string    `json:"tool"`
			AssetTypes  []string  `json:"asset_types"`
			CompletedAt time.Time `json:"completed_at"`
		} `json:"complete_run"`
		Error string `json:"error"`
	} `json:"steps"`
	Expect struct {
		Entities []struct {
			Key          string          `json:"key"`
			AssetType    string          `json:"asset_type"`
			Content      map[string]any  `json:"content"`
			Confidence   *float64        `json:"confidence"`
			CreatedAt    *time.Time      `json:"created_at"`
			LastSeen     *time.Time      `json:"last_seen"`
			State        oam.State       `json:"state"`
			ScopeStatus  oam.ScopeStatus `json:"scope_status"`
			FirstSeenRun string          `json:"first_seen_run"`
			LastSeenRun  string          `json:"last_seen_run"`
			Sources      map[string]int  `json:"sources"`
			Tags         []ExpTag        `json:"tags"`
			MissedRuns   map[string]int  `json:"missed_runs"`
		} `json:"entities"`
		Edges []struct {
			ExpEdgeRef
			RelationType string         `json:"relation_type"`
			Content      map[string]any `json:"content"`
			Confidence   *float64       `json:"confidence"`
			State        oam.State      `json:"state"`
			Sources      map[string]int `json:"sources"`
		} `json:"edges"`
		Changes []struct {
			Type   oam.ChangeType `json:"type"`
			Entity string         `json:"entity"`
			Edge   *ExpEdgeRef    `json:"edge"`
			Patch  []oam.PatchOp  `json:"patch"`
		} `json:"changes"`
	} `json:"expect"`
}

// ExpEdgeRef names an edge by its endpoint keys.
type ExpEdgeRef struct {
	From          string `json:"from"`
	Label         string `json:"label"`
	To            string `json:"to"`
	Discriminator string `json:"discriminator"`
}

// ExpTag is an expected non-source tag.
type ExpTag struct {
	Type    string         `json:"type"`
	Name    string         `json:"name"`
	Content map[string]any `json:"content"`
}

// MergeCase is one decoded conformance file.
type MergeCase struct {
	Name string
	Data MergeFile
}

var sentinels = map[string]error{
	"invalid_observation": oam.ErrInvalidObservation,
	"label_endpoints":     oam.ErrLabelEndpoints,
	"unknown_entity":      oam.ErrUnknownEntity,
	"unresolved_ref":      oam.ErrUnresolvedRef,
	"batch_too_large":     oam.ErrBatchTooLarge,
	"invalid_asset":       oam.ErrInvalidAsset,
	"unknown_field":       oam.ErrUnknownField,
}

// mergeRun replays a conformance file against a store.
type mergeRun struct {
	t       *testing.T
	e       *env
	ids     map[string]string // conformance-space entity id -> store id
	applied []graphstore.IngestRequest
}

func scopeOf(c MergeFile) oam.ScopeFunc {
	if c.Scope == nil {
		return nil
	}
	return func(_, _ string, a oam.Asset) oam.ScopeStatus {
		k, _ := a.Key()
		return c.Scope[k]
	}
}

// MergeConformance runs every conformance/oam/merge case against stores
// from f: steps are ingested (tenant, program and run names mapped to
// canonical ids, embedded entity ids rewritten), and the entities, edges,
// confidences, tags and change events are compared with the expectation.
// Unless a case is order dependent, the reversed and duplicated
// observation list must converge to the same state. With an oracle, every
// final state is also compared byte for byte with the oracle's.
func MergeConformance(t *testing.T, f Factory, cases []MergeCase, oracle Factory) {
	if len(cases) == 0 {
		t.Fatal("no merge conformance cases")
	}
	for _, mc := range cases {
		t.Run(mc.Name, func(t *testing.T) {
			c := mc.Data
			r := replay(t, f, c)
			checkMerge(t, r, c)
			got := Canon(t, r.e.export())
			if oracle != nil {
				o := replay(t, oracle, c)
				if want := Canon(t, o.e.export()); got != want {
					t.Fatalf("state differs from the oracle:\n got %s\nwant %s", got, want)
				}
			}
			if c.OrderDependent {
				return
			}
			e2 := newEnv(t, f, Options{Scope: scopeOf(c), StaleAfterRuns: c.StaleAfterRuns})
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
		})
	}
}

func replay(t *testing.T, f Factory, c MergeFile) *mergeRun {
	t.Helper()
	r := &mergeRun{t: t, e: newEnv(t, f, Options{Scope: scopeOf(c), StaleAfterRuns: c.StaleAfterRuns}), ids: map[string]string{}}
	for i, s := range c.Steps {
		if s.CompleteRun != nil {
			cr := s.CompleteRun
			r.use(cr.TenantID, cr.ProgramID)
			r.e.clock.Set(cr.CompletedAt)
			if _, err := r.e.s.SetStale(r.e.ctx, r.e.c, graphstore.SetStaleRequest{ProgramID: r.e.prog, RunID: RunID(cr.RunID),
				ToolName: cr.Tool, AssetTypes: cr.AssetTypes}); err != nil {
				t.Fatalf("step %d: set stale: %v", i, err)
			}
			continue
		}
		err := r.ingest(s.Observation)
		if s.Error != "" {
			want, ok := sentinels[s.Error]
			if !ok {
				t.Fatalf("step %d: unknown error name %q", i, s.Error)
			}
			if !errors.Is(err, want) {
				t.Fatalf("step %d: error %v, want %s", i, err, s.Error)
			}
			continue
		}
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	return r
}

func (r *mergeRun) use(tenant, program string) {
	r.e.tenant, r.e.prog = TenantID(tenant), ProgramID(program)
	r.e.c = graphstore.Caller{TenantID: r.e.tenant}
}

// ingest decodes (strictly, as the gateway does), maps names, rewrites
// conformance-space entity ids and ingests one step.
func (r *mergeRun) ingest(raw json.RawMessage) error {
	o, err := oam.DecodeObservation(raw, oam.Strict)
	if err != nil {
		return err
	}
	fileTenant, fileProgram := o.TenantID, o.ProgramID
	r.use(o.TenantID, o.ProgramID)
	run := RunID(o.RunID)
	o.TenantID, o.ProgramID, o.RunID = "", "", ""
	mapID := func(s string) string {
		if id, ok := r.ids[s]; ok {
			return id
		}
		return s
	}
	for i := range o.Edges {
		o.Edges[i].From, o.Edges[i].To = mapID(o.Edges[i].From), mapID(o.Edges[i].To)
	}
	for i := range o.Tags {
		o.Tags[i].Target = mapID(o.Tags[i].Target)
	}
	req := graphstore.IngestRequest{TenantID: r.e.tenant, ProgramID: r.e.prog, RunID: run, Observation: o}
	res, err := r.e.s.IngestObservation(r.e.ctx, req)
	if err != nil {
		var ce *core.Error
		if !errors.As(err, &ce) || ce.Kind != core.KindInvalidArgument {
			r.t.Fatalf("rejected observation must be INVALID_ARGUMENT *core.Error, got %T %v", err, err)
		}
		return err
	}
	r.applied = append(r.applied, req)
	for _, id := range res.RefToEntityID {
		ent := r.e.entity(id)
		r.ids[oam.EntityID(fileTenant, fileProgram, ent.AssetType, ent.Key)] = id
	}
	return nil
}

func checkMerge(t *testing.T, r *mergeRun, c MergeFile) {
	t.Helper()
	ex := r.e.export()
	byKey := map[string]graphstore.Entity{}
	byID := map[string]graphstore.Entity{}
	for _, e := range ex.Entities {
		byKey[e.Key], byID[e.ID] = e, e
	}
	if len(ex.Entities) != len(c.Expect.Entities) {
		t.Errorf("entities: got %d, want %d", len(ex.Entities), len(c.Expect.Entities))
	}
	for _, x := range c.Expect.Entities {
		e, ok := byKey[x.Key]
		if !ok {
			t.Errorf("entity %s missing", x.Key)
			continue
		}
		where := "entity " + x.Key
		optEq(t, where+" asset_type", e.AssetType, x.AssetType)
		if x.Content != nil {
			eq(t, where+" content", Canon(t, e.Content), Canon(t, x.Content))
		}
		if x.Confidence != nil {
			eq(t, where+" confidence", e.Confidence, *x.Confidence)
		}
		if x.CreatedAt != nil {
			eq(t, where+" created_at", e.CreatedAt.UTC().Format(time.RFC3339Nano), x.CreatedAt.UTC().Format(time.RFC3339Nano))
		}
		if x.LastSeen != nil {
			eq(t, where+" last_seen", e.LastSeen.UTC().Format(time.RFC3339Nano), x.LastSeen.UTC().Format(time.RFC3339Nano))
		}
		optEq(t, where+" state", string(e.State), string(x.State))
		optEq(t, where+" scope_status", string(e.ScopeStatus), string(x.ScopeStatus))
		if x.FirstSeenRun != "" {
			eq(t, where+" first_seen_run", e.FirstSeenRun, RunID(x.FirstSeenRun))
		}
		if x.LastSeenRun != "" {
			eq(t, where+" last_seen_run", e.LastSeenRun, RunID(x.LastSeenRun))
		}
		if x.Sources != nil {
			eq(t, where+" sources", Canon(t, sourcesOf(e.Tags)), Canon(t, x.Sources))
		}
		if x.Tags != nil {
			eq(t, where+" tags", Canon(t, otherTags(e.Tags)), Canon(t, x.Tags))
		}
		if x.MissedRuns != nil {
			eq(t, where+" missed_runs", Canon(t, e.MissedRuns), Canon(t, x.MissedRuns))
		}
	}
	if len(ex.Edges) != len(c.Expect.Edges) {
		t.Errorf("edges: got %d, want %d", len(ex.Edges), len(c.Expect.Edges))
	}
	edgeName := func(e graphstore.Edge) string {
		return byID[e.FromEntityID].Key + " " + e.Label + " " + byID[e.ToEntityID].Key
	}
	for _, x := range c.Expect.Edges {
		where := fmt.Sprintf("edge %s -%s-> %s", x.From, x.Label, x.To)
		var e graphstore.Edge
		found := false
		for _, ed := range ex.Edges {
			if edgeName(ed) == x.From+" "+x.Label+" "+x.To && (x.Discriminator == "" || x.Discriminator == ed.Discriminator) {
				e, found = ed, true
				break
			}
		}
		if !found {
			t.Errorf("%s missing", where)
			continue
		}
		optEq(t, where+" relation_type", e.RelationType, x.RelationType)
		if x.Content != nil {
			eq(t, where+" content", Canon(t, e.Content), Canon(t, x.Content))
		}
		if x.Confidence != nil {
			eq(t, where+" confidence", e.Confidence, *x.Confidence)
		}
		optEq(t, where+" state", string(e.State), string(x.State))
		if x.Sources != nil {
			eq(t, where+" sources", Canon(t, sourcesOf(e.Tags)), Canon(t, x.Sources))
		}
	}
	if c.Expect.Changes == nil {
		return
	}
	edges := map[string]graphstore.Edge{}
	for _, e := range ex.Edges {
		edges[e.ID] = e
	}
	type flat struct {
		Type  oam.ChangeType `json:"type"`
		What  string         `json:"what"`
		Patch []oam.PatchOp  `json:"patch,omitempty"`
	}
	var have, want []flat
	var prev int64
	for _, ev := range r.e.changes(0) {
		if ev.Seq <= prev {
			t.Errorf("change seqs not increasing: %d after %d", ev.Seq, prev)
		}
		prev = ev.Seq
		f := flat{Type: ev.Type, Patch: ev.Patch, What: byID[ev.EntityID].Key}
		if ev.EdgeID != "" {
			f.What = edgeName(edges[ev.EdgeID])
		}
		have = append(have, f)
	}
	for _, x := range c.Expect.Changes {
		f := flat{Type: x.Type, What: x.Entity, Patch: x.Patch}
		if x.Edge != nil {
			f.What = x.Edge.From + " " + x.Edge.Label + " " + x.Edge.To
		}
		want = append(want, f)
	}
	eq(t, "changes", Canon(t, have), Canon(t, want))
}

func sourcesOf(tags []graphstore.Tag) map[string]int {
	out := map[string]int{}
	for _, tg := range tags {
		if tg.PropertyType == oam.PropertySource {
			p, err := oam.JoinProperty(tg.PropertyType, tg.Content, oam.Lenient)
			if err == nil {
				out[tg.Name] = p.(oam.SourceProperty).Confidence
			}
		}
	}
	return out
}

func otherTags(tags []graphstore.Tag) []ExpTag {
	out := []ExpTag{}
	for _, tg := range tags {
		if tg.PropertyType != oam.PropertySource {
			out = append(out, ExpTag{Type: tg.PropertyType, Name: tg.Name, Content: tg.Content})
		}
	}
	return out
}

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

func optEq(t *testing.T, what, got, want string) {
	t.Helper()
	if want != "" {
		eq(t, what, got, want)
	}
}
