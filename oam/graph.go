package oam

import (
	"fmt"
	"sync"
	"time"
)

// DefaultStaleAfterRuns is the number of consecutive covering runs that
// must miss an entity before it becomes stale (spec 5.7).
const DefaultStaleAfterRuns = 3

// ScopeStatus is an entity's scope classification (spec 4, 5.3).
type ScopeStatus string

// Scope statuses.
const (
	ScopeInScope    ScopeStatus = "in_scope"
	ScopeOutOfScope ScopeStatus = "out_of_scope"
	ScopeUnknown    ScopeStatus = "unknown"
)

// State is an entity's or edge's lifecycle state.
type State string

// Lifecycle states. Nothing is ever hard-deleted by ingest.
const (
	StateActive State = "active"
	StateStale  State = "stale"
)

// ScopeFunc is the scope port: the section 4 matcher in passive mode (no
// DNS resolution). It must be a pure function of its arguments. Any value
// other than ScopeInScope or ScopeOutOfScope is treated as ScopeUnknown;
// the graph never promotes unknown to in-scope by inference.
type ScopeFunc func(tenantID, programID string, a Asset) ScopeStatus

// Config configures a Graph.
type Config struct {
	// StaleAfterRuns defaults to DefaultStaleAfterRuns when zero.
	StaleAfterRuns int
	// Scope classifies entities; nil leaves every entity unknown.
	Scope ScopeFunc
}

// Graph is the in-memory reference implementation of graph ingest (spec
// 5.7, 23.5): it applies observations with exactly the merge semantics the
// Postgres graph service implements, so its state is the oracle for the
// service's tests. All times come from the observations (observed_at) or
// from covering-run reports, never from a clock. A Graph is safe for
// concurrent use.
type Graph struct {
	mu         sync.Mutex
	staleAfter int
	scope      ScopeFunc
	entities   map[string]*entityState
	edges      map[string]*edgeState
	applied    map[string]struct{} // tenant + NUL + observation id
	runs       map[string]struct{} // completed covering runs
	changes    []ChangeEvent
}

// NewGraph returns an empty graph.
func NewGraph(cfg Config) *Graph {
	n := cfg.StaleAfterRuns
	if n <= 0 {
		n = DefaultStaleAfterRuns
	}
	return &Graph{
		staleAfter: n, scope: cfg.Scope,
		entities: map[string]*entityState{}, edges: map[string]*edgeState{},
		applied: map[string]struct{}{}, runs: map[string]struct{}{},
	}
}

// Result reports the effect of one Apply (IngestResponse in spec 11.6).
type Result struct {
	// Duplicate is true when the observation id was already applied; such
	// a call has no effect (spec 6.1).
	Duplicate       bool
	EntitiesCreated int
	EntitiesUpdated int
	EdgesCreated    int
	EdgesUpdated    int
	RefToEntityID   map[string]string
	Changes         []ChangeEvent
}

type sighting struct {
	stamp Stamp
	run   string
}

type sourceState struct {
	confidence        int // percent
	created, lastSeen time.Time
}

type tagKey struct{ ptype, name string }

type tagState struct {
	content           Content
	created, lastSeen time.Time
}

// record is the state shared by entities and edges.
type record struct {
	id, tenant, program string
	content             Content
	state               State
	first, last         sighting
	sources             map[string]*sourceState
	tags                map[tagKey]*tagState
	lastRun             map[string]sighting // per tool: latest sighting
	missed              map[string]int      // per tool: consecutive covering runs missed
}

type entityState struct {
	record
	assetType, key string
	scope          ScopeStatus
}

type edgeState struct {
	record
	relType, label, disc string
	from, to             string
}

func newRecord(id, tenant, program string, p *prepared, run string, content map[string]any) record {
	s := sighting{p.stamp, run}
	r := record{
		id: id, tenant: tenant, program: program,
		content: NewContent(content, p.stamp), state: StateActive,
		first: s, last: s,
		sources: map[string]*sourceState{}, tags: map[tagKey]*tagState{},
		lastRun: map[string]sighting{p.stamp.Source: s}, missed: map[string]int{},
	}
	r.addSource(p.stamp.Source, p.percent, p.stamp.At)
	return r
}

// see records a sighting and reports whether it reactivated a stale record.
func (r *record) see(p *prepared, run string) bool {
	s := sighting{p.stamp, run}
	if s.stamp.Compare(r.first.stamp) < 0 {
		r.first = s
	}
	if s.stamp.Compare(r.last.stamp) > 0 {
		r.last = s
	}
	tool := p.stamp.Source
	r.addSource(tool, p.percent, p.stamp.At)
	if lr, ok := r.lastRun[tool]; !ok || s.stamp.Compare(lr.stamp) > 0 {
		r.lastRun[tool] = s
	}
	if _, ok := r.missed[tool]; ok {
		r.missed[tool] = 0
	}
	if r.state == StateStale {
		r.state = StateActive
		return true
	}
	return false
}

func (r *record) addSource(name string, percent int, at time.Time) {
	s, ok := r.sources[name]
	if !ok {
		r.sources[name] = &sourceState{confidence: percent, created: at, lastSeen: at}
		return
	}
	s.confidence = max(s.confidence, percent)
	s.created, s.lastSeen = MinTime(s.created, at), MaxTime(s.lastSeen, at)
}

func (r *record) tag(t preparedTag, stamp Stamp) {
	if t.source != nil {
		r.addSource(t.source.Name, t.source.Confidence, stamp.At)
		return
	}
	k := tagKey{t.ptype, t.name}
	ts, ok := r.tags[k]
	if !ok {
		r.tags[k] = &tagState{content: NewContent(t.content, stamp), created: stamp.At, lastSeen: stamp.At}
		return
	}
	ts.content.Merge(t.content, stamp)
	ts.created, ts.lastSeen = MinTime(ts.created, stamp.At), MaxTime(ts.lastSeen, stamp.At)
}

func (r *record) confidence() float64 {
	s := make(Sources, len(r.sources))
	for name, st := range r.sources {
		s[name] = float64(st.confidence) / 100
	}
	return s.Confidence()
}

// Apply ingests one observation atomically: either every entity, edge and
// tag is applied or, on error, nothing is. Applying an observation id a
// second time has no effect and returns Result{Duplicate: true}.
func (g *Graph) Apply(o Observation) (Result, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	dk := o.TenantID + "\x00" + o.ObservationID
	if _, ok := g.applied[dk]; ok {
		return Result{Duplicate: true}, nil
	}
	p, err := o.prepare()
	if err != nil {
		return Result{}, err
	}
	ids, err := g.resolve(o, p)
	if err != nil {
		return Result{}, err
	}
	res := g.commit(o, p, ids)
	g.applied[dk] = struct{}{}
	return res, nil
}

// resolve computes local entity ids and checks every reference to an
// existing entity or edge, plus label rules that need stored asset types.
func (g *Graph) resolve(o Observation, p *prepared) ([]string, error) {
	ids := make([]string, len(p.entities))
	for i, e := range p.entities {
		ids[i] = EntityID(o.TenantID, o.ProgramID, e.assetType, e.key)
		if ex, ok := g.entities[ids[i]]; ok && (ex.key != e.key || ex.assetType != e.assetType) {
			return nil, fmt.Errorf("oam: entity id collision for %s", e.key)
		}
	}
	typeOf := func(path string, ep endpoint) (string, error) {
		if ep.local >= 0 {
			return p.entities[ep.local].assetType, nil
		}
		e, ok := g.entities[ep.id]
		if !ok || e.tenant != o.TenantID || e.program != o.ProgramID {
			return "", invalidAt(path, fmt.Errorf("%w: %s", ErrUnknownEntity, ep.id))
		}
		return e.assetType, nil
	}
	for i, e := range p.edges {
		path := fmt.Sprintf("edges[%d]", i)
		ft, err := typeOf(path+".from", e.from)
		if err != nil {
			return nil, err
		}
		tt, err := typeOf(path+".to", e.to)
		if err != nil {
			return nil, err
		}
		if e.from.local < 0 || e.to.local < 0 {
			if err := ValidateEdge(e.rel, ft, tt); err != nil {
				return nil, invalidAt(path+".relation", err)
			}
		}
	}
	for i, t := range p.tags {
		path := fmt.Sprintf("tags[%d].target", i)
		if t.edge {
			if ed, ok := g.edges[t.target.id]; !ok || ed.tenant != o.TenantID || ed.program != o.ProgramID {
				return nil, invalidAt(path, fmt.Errorf("%w: %s", ErrUnknownEntity, t.target.id))
			}
			continue
		}
		if _, err := typeOf(path, t.target); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func (g *Graph) commit(o Observation, p *prepared, ids []string) Result {
	em := &emitter{g: g, tenant: o.TenantID, prog: o.ProgramID, origin: o.ObservationID, run: o.RunID, at: p.stamp.At}
	res := Result{RefToEntityID: make(map[string]string, len(ids))}
	idOf := func(ep endpoint) string {
		if ep.local >= 0 {
			return ids[ep.local]
		}
		return ep.id
	}
	for i, pe := range p.entities {
		id := ids[i]
		res.RefToEntityID[pe.ref] = id
		e, ok := g.entities[id]
		if !ok {
			e = &entityState{record: newRecord(id, o.TenantID, o.ProgramID, p, o.RunID, pe.content), assetType: pe.assetType, key: pe.key}
			e.scope = g.scopeOf(e)
			g.entities[id] = e
			res.EntitiesCreated++
			em.emit(ChangeEntityAdded, id, "", nil)
			continue
		}
		res.EntitiesUpdated++
		if e.see(p, o.RunID) {
			em.emit(ChangeEntityReactivated, id, "", nil)
		}
		if ops := e.content.Merge(pe.content, p.stamp); len(ops) > 0 {
			em.emit(ChangeEntityChanged, id, "", ops)
			if st := g.scopeOf(e); st != e.scope {
				e.scope = st
				em.emit(ChangeScopeStatusChanged, id, "", []PatchOp{{Op: "replace", Path: "/scope_status", Value: string(st)}})
			}
		}
	}
	for _, pe := range p.edges {
		k := EdgeKey{o.TenantID, o.ProgramID, idOf(pe.from), idOf(pe.to), pe.relType, pe.label, pe.discriminator}
		id := k.ID()
		ed, ok := g.edges[id]
		if !ok {
			ed = &edgeState{record: newRecord(id, o.TenantID, o.ProgramID, p, o.RunID, pe.content),
				relType: pe.relType, label: pe.label, disc: pe.discriminator, from: k.From, to: k.To}
			g.edges[id] = ed
			res.EdgesCreated++
			em.emit(ChangeEdgeAdded, "", id, nil)
			continue
		}
		res.EdgesUpdated++
		if ed.see(p, o.RunID) {
			em.emit(ChangeEdgeAdded, "", id, nil)
		}
		ed.content.Merge(pe.content, p.stamp)
	}
	for _, t := range p.tags {
		if t.edge {
			g.edges[t.target.id].tag(t, p.stamp)
		} else {
			g.entities[idOf(t.target)].tag(t, p.stamp)
		}
	}
	res.Changes = em.out
	return res
}

// scopeOf classifies the entity's merged content, which converges, so the
// status does too.
func (g *Graph) scopeOf(e *entityState) ScopeStatus {
	if g.scope == nil {
		return ScopeUnknown
	}
	a, err := JoinAsset(e.assetType, e.content.Values, Lenient)
	if err != nil {
		return ScopeUnknown
	}
	switch st := g.scope(e.tenant, e.program, a); st {
	case ScopeInScope, ScopeOutOfScope:
		return st
	}
	return ScopeUnknown
}
