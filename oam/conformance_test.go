package oam_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/testkit"
)

// keysFile is the schema of conformance/oam/keys/*.json.
type keysFile struct {
	Description string `json:"description"`
	Cases       []struct {
		Name      string            `json:"name"`
		Spellings []json.RawMessage `json:"spellings"`
		Key       string            `json:"key"`
		Error     bool              `json:"error"`
	} `json:"cases"`
}

// TestOAM001_DedupKeys checks spec 25.3 oam_001: the same asset written
// in different spellings yields one key, for every asset type.
func TestOAM001_DedupKeys(t *testing.T) {
	for _, f := range testkit.LoadCases[keysFile](t, "oam/keys") {
		for _, c := range f.Data.Cases {
			t.Run(f.Name+"/"+c.Name, func(t *testing.T) {
				if len(c.Spellings) == 0 {
					t.Fatal("case has no spellings")
				}
				for _, raw := range c.Spellings {
					a, err := oam.DecodeAsset(raw, oam.Strict)
					var key string
					if err == nil {
						key, err = a.Key()
					}
					if c.Error {
						if err == nil {
							t.Fatalf("%s: got key %q, want an error", raw, key)
						}
						if !errors.Is(err, oam.ErrInvalidAsset) && !errors.Is(err, oam.ErrMalformed) {
							t.Fatalf("%s: error %v is not ErrInvalidAsset", raw, err)
						}
						continue
					}
					if err != nil {
						t.Fatalf("%s: %v", raw, err)
					}
					if key != c.Key {
						t.Fatalf("%s: key %q, want %q", raw, key, c.Key)
					}
					// Normalization is idempotent and keeps the key.
					n, err := a.Normalize()
					if err != nil {
						t.Fatalf("normalize: %v", err)
					}
					n2, err := n.Normalize()
					if err != nil {
						t.Fatalf("normalize twice: %v", err)
					}
					if k, _ := n.Key(); k != c.Key {
						t.Fatalf("normalized key %q, want %q", k, c.Key)
					}
					if mustCanon(t, n) != mustCanon(t, n2) {
						t.Fatalf("normalize not idempotent: %s vs %s", mustCanon(t, n), mustCanon(t, n2))
					}
				}
			})
		}
	}
}

// mergeFile is the schema of conformance/oam/merge/*.json.
type mergeFile struct {
	Description    string                     `json:"description"`
	StaleAfterRuns int                        `json:"stale_after_runs"`
	Scope          map[string]oam.ScopeStatus `json:"scope"` // key -> status; others unknown
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
		Error string `json:"error"` // expected sentinel, see sentinels
		// Pending: the observation references ids not ingested yet and must
		// be buffered (Result.Pending), not applied and not rejected.
		Pending bool `json:"pending"`
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
			Tags         []expTag        `json:"tags"`
			MissedRuns   map[string]int  `json:"missed_runs"`
		} `json:"entities"`
		Edges []struct {
			expEdgeRef
			RelationType string         `json:"relation_type"`
			Content      map[string]any `json:"content"`
			Confidence   *float64       `json:"confidence"`
			State        oam.State      `json:"state"`
			Sources      map[string]int `json:"sources"`
		} `json:"edges"`
		Changes []struct {
			Type   oam.ChangeType `json:"type"`
			Entity string         `json:"entity"`
			Edge   *expEdgeRef    `json:"edge"`
			Patch  []oam.PatchOp  `json:"patch"`
		} `json:"changes"`
	} `json:"expect"`
}

type expEdgeRef struct {
	From          string `json:"from"`
	Label         string `json:"label"`
	To            string `json:"to"`
	Discriminator string `json:"discriminator"`
}

type expTag struct {
	Type    string         `json:"type"`
	Name    string         `json:"name"`
	Content map[string]any `json:"content"`
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

// TestOAMConformance_Merge runs conformance/oam/merge: observation lists
// with the expected entities, edges, confidences and change events (spec
// 5.7, 5.8, 23.5). Unless a file is marked order_dependent, it also checks
// that the reversed and duplicated list converges to the same state.
func TestOAMConformance_Merge(t *testing.T) {
	for _, f := range testkit.LoadCases[mergeFile](t, "oam/merge") {
		t.Run(f.Name, func(t *testing.T) {
			c := f.Data
			g, obs := runMergeSteps(t, c)
			checkEntities(t, g, c)
			checkEdges(t, g, c)
			checkChanges(t, g, c)
			if c.OrderDependent || len(obs) != len(c.Steps) {
				return
			}
			g2 := newConformanceGraph(c)
			rev := slices.Clone(obs)
			slices.Reverse(rev)
			for _, o := range append(rev, obs...) {
				if _, err := g2.Apply(o); err != nil {
					t.Fatalf("replay %s: %v", o.ObservationID, err)
				}
			}
			if a, b := canon(t, g), canon(t, g2); a != b {
				t.Fatalf("state depends on order:\n%s\n%s", a, b)
			}
		})
	}
}

func newConformanceGraph(c mergeFile) *oam.Graph {
	var scope oam.ScopeFunc
	if c.Scope != nil {
		scope = func(_, _ string, a oam.Asset) oam.ScopeStatus {
			k, _ := a.Key()
			return c.Scope[k]
		}
	}
	return oam.NewGraph(oam.Config{StaleAfterRuns: c.StaleAfterRuns, Scope: scope})
}

// runMergeSteps applies the steps and returns the successfully applied
// observations.
func runMergeSteps(t *testing.T, c mergeFile) (*oam.Graph, []oam.Observation) {
	t.Helper()
	g := newConformanceGraph(c)
	var applied []oam.Observation
	for i, s := range c.Steps {
		if s.CompleteRun != nil {
			r := s.CompleteRun
			if _, err := g.CompleteRun(oam.CoveringRun{TenantID: r.TenantID, ProgramID: r.ProgramID, RunID: r.RunID,
				Tool: r.Tool, AssetTypes: r.AssetTypes, CompletedAt: r.CompletedAt}); err != nil {
				t.Fatalf("step %d: complete run: %v", i, err)
			}
			continue
		}
		o, err := oam.DecodeObservation(s.Observation, oam.Strict)
		var res oam.Result
		if err == nil {
			res, err = g.Apply(o)
		}
		if err == nil && res.Pending != s.Pending {
			t.Fatalf("step %d: pending %v, want %v (missing %v)", i, res.Pending, s.Pending, res.Missing)
		}
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
		applied = append(applied, o)
	}
	return g, applied
}

func checkEntities(t *testing.T, g *oam.Graph, c mergeFile) {
	t.Helper()
	got := map[string]oam.Entity{}
	for _, e := range g.Entities() {
		got[e.Key] = e
	}
	if len(got) != len(c.Expect.Entities) {
		t.Errorf("entities: got %d %v, want %d", len(got), keysOf(got), len(c.Expect.Entities))
	}
	for _, x := range c.Expect.Entities {
		e, ok := got[x.Key]
		if !ok {
			t.Errorf("entity %s missing", x.Key)
			continue
		}
		where := "entity " + x.Key
		optEq(t, where+" asset_type", e.AssetType, x.AssetType)
		if x.Content != nil {
			eq(t, where+" content", mustCanon(t, e.Content), mustCanon(t, x.Content))
		}
		if x.Confidence != nil {
			eq(t, where+" confidence", e.Confidence, *x.Confidence)
		}
		if x.CreatedAt != nil {
			eq(t, where+" created_at", e.CreatedAt.UTC(), x.CreatedAt.UTC())
		}
		if x.LastSeen != nil {
			eq(t, where+" last_seen", e.LastSeen.UTC(), x.LastSeen.UTC())
		}
		optEq(t, where+" state", string(e.State), string(x.State))
		optEq(t, where+" scope_status", string(e.ScopeStatus), string(x.ScopeStatus))
		optEq(t, where+" first_seen_run", e.FirstSeenRun, x.FirstSeenRun)
		optEq(t, where+" last_seen_run", e.LastSeenRun, x.LastSeenRun)
		if x.Sources != nil {
			eq(t, where+" sources", mustCanon(t, sourcesOf(e.Tags)), mustCanon(t, x.Sources))
		}
		if x.Tags != nil {
			eq(t, where+" tags", mustCanon(t, otherTags(e.Tags)), mustCanon(t, x.Tags))
		}
		if x.MissedRuns != nil {
			eq(t, where+" missed_runs", mustCanon(t, e.MissedRuns), mustCanon(t, x.MissedRuns))
		}
	}
}

func checkEdges(t *testing.T, g *oam.Graph, c mergeFile) {
	t.Helper()
	edges := g.Edges()
	if len(edges) != len(c.Expect.Edges) {
		t.Errorf("edges: got %d, want %d", len(edges), len(c.Expect.Edges))
	}
	for _, x := range c.Expect.Edges {
		e, ok := findEdge(g, edges, x.expEdgeRef)
		where := fmt.Sprintf("edge %s -%s-> %s", x.From, x.Label, x.To)
		if !ok {
			t.Errorf("%s missing", where)
			continue
		}
		optEq(t, where+" relation_type", e.RelationType, x.RelationType)
		if x.Content != nil {
			eq(t, where+" content", mustCanon(t, e.Content), mustCanon(t, x.Content))
		}
		if x.Confidence != nil {
			eq(t, where+" confidence", e.Confidence, *x.Confidence)
		}
		optEq(t, where+" state", string(e.State), string(x.State))
		if x.Sources != nil {
			eq(t, where+" sources", mustCanon(t, sourcesOf(e.Tags)), mustCanon(t, x.Sources))
		}
	}
}

func checkChanges(t *testing.T, g *oam.Graph, c mergeFile) {
	t.Helper()
	if c.Expect.Changes == nil {
		return
	}
	got := g.Changes()
	type flat struct {
		Type  oam.ChangeType `json:"type"`
		What  string         `json:"what"`
		Patch []oam.PatchOp  `json:"patch,omitempty"`
	}
	var have, want []flat
	for _, ev := range got {
		f := flat{Type: ev.Type, Patch: ev.Patch}
		if ev.EntityID != "" {
			e, _ := g.Entity(ev.EntityID)
			f.What = e.Key
		} else {
			e, _ := g.Edge(ev.EdgeID)
			from, _ := g.Entity(e.FromEntity)
			to, _ := g.Entity(e.ToEntity)
			f.What = from.Key + " " + e.Label + " " + to.Key
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
	eq(t, "changes", mustCanon(t, have), mustCanon(t, want))
}

func findEdge(g *oam.Graph, edges []oam.Edge, r expEdgeRef) (oam.Edge, bool) {
	for _, e := range edges {
		from, _ := g.Entity(e.FromEntity)
		to, _ := g.Entity(e.ToEntity)
		if from.Key == r.From && to.Key == r.To && e.Label == r.Label && (r.Discriminator == "" || r.Discriminator == e.Discriminator) {
			return e, true
		}
	}
	return oam.Edge{}, false
}

func sourcesOf(tags []oam.Tag) map[string]int {
	out := map[string]int{}
	for _, tg := range tags {
		if tg.PropertyType == oam.PropertySource {
			p, _ := tg.Property()
			out[tg.Name] = p.(oam.SourceProperty).Confidence
		}
	}
	return out
}

func otherTags(tags []oam.Tag) []expTag {
	out := []expTag{}
	for _, tg := range tags {
		if tg.PropertyType != oam.PropertySource {
			out = append(out, expTag{Type: tg.PropertyType, Name: tg.Name, Content: tg.Content})
		}
	}
	return out
}

func keysOf(m map[string]oam.Entity) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
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

func mustCanon(t testing.TB, v any) string {
	t.Helper()
	b, err := core.CanonicalJSON(v)
	if err != nil {
		t.Fatalf("canonical JSON: %v", err)
	}
	return string(b)
}

func canon(t testing.TB, g *oam.Graph) string {
	t.Helper()
	b, err := g.CanonicalBytes()
	if err != nil {
		t.Fatalf("canonical bytes: %v", err)
	}
	return string(b)
}
