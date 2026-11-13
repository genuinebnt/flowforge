package oam

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Entity is a snapshot of one graph node (spec 5.3).
type Entity struct {
	ID           string
	TenantID     string
	ProgramID    string
	AssetType    string
	Key          string
	Content      map[string]any
	ScopeStatus  ScopeStatus
	Confidence   float64
	State        State
	CreatedAt    time.Time
	LastSeen     time.Time
	FirstSeenRun string
	LastSeenRun  string
	MissedRuns   map[string]int // per tool, consecutive covering runs missed
	Tags         []Tag          // SourceProperty tags included, sorted by (type, name)
}

// Asset rebuilds the typed asset from the stored content.
func (e Entity) Asset() (Asset, error) { return JoinAsset(e.AssetType, e.Content, Lenient) }

// Edge is a snapshot of one graph edge (spec 5.3).
type Edge struct {
	ID            string
	TenantID      string
	ProgramID     string
	RelationType  string
	Label         string
	Discriminator string
	Content       map[string]any // relation fields other than type and label
	FromEntity    string
	ToEntity      string
	Confidence    float64
	State         State
	CreatedAt     time.Time
	LastSeen      time.Time
	MissedRuns    map[string]int
	Tags          []Tag
}

// Relation rebuilds the typed relation from the stored content.
func (e Edge) Relation() (Relation, error) {
	return JoinRelation(e.RelationType, e.Label, e.Content, Lenient)
}

// Tag is a property attached to an entity or edge.
type Tag struct {
	PropertyType string
	Name         string
	Content      map[string]any // property fields other than type
	CreatedAt    time.Time
	LastSeen     time.Time
}

// Property rebuilds the typed property from the stored content.
func (t Tag) Property() (Property, error) { return JoinProperty(t.PropertyType, t.Content, Lenient) }

func withType(typ string, content map[string]any, extra ...any) map[string]any {
	m := make(map[string]any, len(content)+1+len(extra)/2)
	maps.Copy(m, content)
	m["type"] = typ
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	return m
}

func timeJSON(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// MarshalJSON encodes the spec 5.3 entity shape plus missed_runs and tags.
func (e Entity) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID           string         `json:"id"`
		TenantID     string         `json:"tenant_id,omitempty"`
		ProgramID    string         `json:"program_id,omitempty"`
		Asset        map[string]any `json:"asset"`
		Key          string         `json:"key"`
		CreatedAt    string         `json:"created_at"`
		LastSeen     string         `json:"last_seen"`
		ScopeStatus  ScopeStatus    `json:"scope_status"`
		Confidence   float64        `json:"confidence"`
		State        State          `json:"state"`
		FirstSeenRun string         `json:"first_seen_run,omitempty"`
		LastSeenRun  string         `json:"last_seen_run,omitempty"`
		MissedRuns   map[string]int `json:"missed_runs,omitempty"`
		Tags         []Tag          `json:"tags,omitempty"`
	}{e.ID, e.TenantID, e.ProgramID, withType(e.AssetType, e.Content), e.Key,
		timeJSON(e.CreatedAt), timeJSON(e.LastSeen), e.ScopeStatus, e.Confidence, e.State,
		e.FirstSeenRun, e.LastSeenRun, e.MissedRuns, e.Tags})
}

// MarshalJSON encodes the spec 5.3 edge shape plus discriminator,
// state, missed_runs and tags.
func (e Edge) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ID            string         `json:"id"`
		TenantID      string         `json:"tenant_id,omitempty"`
		ProgramID     string         `json:"program_id,omitempty"`
		Relation      map[string]any `json:"relation"`
		Discriminator string         `json:"discriminator"`
		FromEntity    string         `json:"from_entity"`
		ToEntity      string         `json:"to_entity"`
		CreatedAt     string         `json:"created_at"`
		LastSeen      string         `json:"last_seen"`
		Confidence    float64        `json:"confidence"`
		State         State          `json:"state"`
		MissedRuns    map[string]int `json:"missed_runs,omitempty"`
		Tags          []Tag          `json:"tags,omitempty"`
	}{e.ID, e.TenantID, e.ProgramID, withType(e.RelationType, e.Content, "label", e.Label), e.Discriminator,
		e.FromEntity, e.ToEntity, timeJSON(e.CreatedAt), timeJSON(e.LastSeen), e.Confidence, e.State,
		e.MissedRuns, e.Tags})
}

// MarshalJSON encodes {"property": {...}, "created_at", "last_seen"}.
func (t Tag) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Property  map[string]any `json:"property"`
		CreatedAt string         `json:"created_at"`
		LastSeen  string         `json:"last_seen"`
	}{withType(t.PropertyType, t.Content), timeJSON(t.CreatedAt), timeJSON(t.LastSeen)})
}

func (r *record) tagViews() []Tag {
	out := make([]Tag, 0, len(r.sources)+len(r.tags))
	for name, s := range r.sources {
		out = append(out, Tag{
			PropertyType: PropertySource, Name: name,
			Content:   map[string]any{"name": name, "confidence": json.Number(strconv.Itoa(s.confidence))},
			CreatedAt: s.created, LastSeen: s.lastSeen,
		})
	}
	for k, t := range r.tags {
		if !t.visible() {
			continue
		}
		out = append(out, Tag{PropertyType: k.ptype, Name: k.name, Content: cloneObject(t.content.Values), CreatedAt: t.created, LastSeen: t.lastSeen})
	}
	slices.SortFunc(out, func(a, b Tag) int {
		return cmp.Or(cmp.Compare(a.PropertyType, b.PropertyType), cmp.Compare(a.Name, b.Name))
	})
	return out
}

func missedView(m map[string]int) map[string]int {
	if len(m) == 0 {
		return nil
	}
	return maps.Clone(m)
}

func (e *entityState) view() Entity {
	return Entity{
		ID: e.id, TenantID: e.tenant, ProgramID: e.program, AssetType: e.assetType, Key: e.key,
		Content: cloneObject(e.content.Values), ScopeStatus: e.scope, Confidence: e.confidence(), State: e.state,
		CreatedAt: e.first.stamp.At, LastSeen: e.last.stamp.At, FirstSeenRun: e.first.run, LastSeenRun: e.last.run,
		MissedRuns: missedView(e.missed), Tags: e.tagViews(),
	}
}

func (e *edgeState) view() Edge {
	return Edge{
		ID: e.id, TenantID: e.tenant, ProgramID: e.program, RelationType: e.relType, Label: e.label,
		Discriminator: e.disc, Content: cloneObject(e.content.Values), FromEntity: e.from, ToEntity: e.to,
		Confidence: e.confidence(), State: e.state, CreatedAt: e.first.stamp.At, LastSeen: e.last.stamp.At,
		MissedRuns: missedView(e.missed), Tags: e.tagViews(),
	}
}

// Entity returns the entity with the given id.
func (g *Graph) Entity(id string) (Entity, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entities[id]
	if !ok {
		return Entity{}, false
	}
	return e.view(), true
}

// Lookup returns the entity a matching asset would merge into.
func (g *Graph) Lookup(tenantID, programID string, a Asset) (Entity, bool, error) {
	_, typ, key, _, err := prepareAsset(a)
	if err != nil {
		return Entity{}, false, err
	}
	e, ok := g.Entity(EntityID(tenantID, programID, typ, key))
	return e, ok, nil
}

// Edge returns the edge with the given id.
func (g *Graph) Edge(id string) (Edge, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.edges[id]
	if !ok {
		return Edge{}, false
	}
	return e.view(), true
}

// Entities returns every entity, sorted by id.
func (g *Graph) Entities() []Entity {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.entityViews()
}

// Edges returns every edge, sorted by id.
func (g *Graph) Edges() []Edge {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.edgeViews()
}

func (g *Graph) entityViews() []Entity {
	out := make([]Entity, 0, len(g.entities))
	for _, id := range slices.Sorted(maps.Keys(g.entities)) {
		out = append(out, g.entities[id].view())
	}
	return out
}

func (g *Graph) edgeViews() []Edge {
	out := make([]Edge, 0, len(g.edges))
	for _, id := range slices.Sorted(maps.Keys(g.edges)) {
		out = append(out, g.edges[id].view())
	}
	return out
}

// Changes returns every change event emitted so far, in emission order.
func (g *Graph) Changes() []ChangeEvent {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.changes)
}

// CanonicalBytes encodes the graph state (entities and edges with their
// tags, sorted by their derived ids) as canonical JSON. Equal states give
// byte-identical output, which is what the convergence property compares
// (spec 23.5). Excluded, because they are history or bookkeeping rather
// than state: the change-event log, the set of applied observation ids and
// the per-field write stamps.
func (g *Graph) CanonicalBytes() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return core.CanonicalJSON(struct {
		Entities []Entity `json:"entities"`
		Edges    []Edge   `json:"edges"`
	}{g.entityViews(), g.edgeViews()})
}

// cloneObject deep-copies a JSON object so callers cannot alias graph state.
func cloneObject(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneObject(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = cloneValue(e)
		}
		return out
	}
	return v
}
