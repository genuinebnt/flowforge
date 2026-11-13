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
	contentIdx map[string]int // content_changed event id -> index in changes
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
		applied: map[string]struct{}{}, runs: map[string]struct{}{}, contentIdx: map[string]int{},
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
	// classic is set once a non-authoritative observation (or the API)
	// reported the tag; such a tag is visible for good. authIn lists the
	// profiles whose authority unit currently holds the tag (sorted).
	classic bool
	authIn  []string
}

// visible reports whether the tag is part of the graph state.
func (t *tagState) visible() bool { return t.classic || len(t.authIn) > 0 }

// record is the state shared by entities and edges.
type record struct {
	id, tenant, program string
	content             Content
	state               State
	first, last         sighting
	sources             map[string]*sourceState
	tags                map[tagKey]*tagState
	lastRun             map[string]sighting // per tool: latest classic sighting
	missed              map[string]int      // per tool: consecutive covering runs missed
	// classicStale is set when covering runs made the classic support
	// stale; a classic sighting clears it.
	classicStale bool
	// authIn (edges): profiles whose authority unit holds the edge.
	authIn []string
	// authRefs (entities): incoming edges whose authIn is non-empty.
	authRefs int
}

type entityState struct {
	record
	assetType, key string
	scope          ScopeStatus
	// heads are the authority units of which this entity is the source,
	// by unitKey(profile, family).
	heads map[string]*authHead
}

type edgeState struct {
	record
	relType, label, disc string
	from, to             string
}

func newRecord(id, tenant, program string, p *prepared, run string, content map[string]any, classic bool) record {
	s := sighting{p.stamp, run}
	r := record{
		id: id, tenant: tenant, program: program,
		content: NewContent(content, p.stamp), state: StateActive,
		first: s, last: s,
		sources: map[string]*sourceState{}, tags: map[tagKey]*tagState{},
		lastRun: map[string]sighting{}, missed: map[string]int{},
	}
	if classic {
		r.lastRun[p.stamp.Source] = s
	}
	r.addSource(p.stamp.Source, p.percent, p.stamp.At)
	return r
}

// see records a sighting. A classic sighting also registers the tool for
// covering runs and clears classic staleness; an authoritative one only
// moves times, sources and content.
func (r *record) see(p *prepared, run string, classic bool) {
	s := sighting{p.stamp, run}
	if s.stamp.Compare(r.first.stamp) < 0 {
		r.first = s
	}
	if s.stamp.Compare(r.last.stamp) > 0 {
		r.last = s
	}
	tool := p.stamp.Source
	r.addSource(tool, p.percent, p.stamp.At)
	if !classic {
		return
	}
	if lr, ok := r.lastRun[tool]; !ok || s.stamp.Compare(lr.stamp) > 0 {
		r.lastRun[tool] = s
	}
	if _, ok := r.missed[tool]; ok {
		r.missed[tool] = 0
	}
	r.classicStale = false
}

// live is the liveness rule: a record is active while it has classic
// support that covering runs have not made stale, or authoritative
// support (an edge held by an authority unit, an entity that such an edge
// points at).
func (r *record) live() bool {
	return (len(r.lastRun) > 0 && !r.classicStale) || len(r.authIn) > 0 || r.authRefs > 0
}

// settle sets the state from live and reports whether it changed.
func (r *record) settle() bool {
	want := StateStale
	if r.live() {
		want = StateActive
	}
	if r.state == want {
		return false
	}
	r.state = want
	return true
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

func (r *record) tag(t preparedTag, stamp Stamp, classic bool) {
	if t.source != nil {
		r.addSource(t.source.Name, t.source.Confidence, stamp.At)
		return
	}
	k := tagKey{t.ptype, t.name}
	ts, ok := r.tags[k]
	if !ok {
		r.tags[k] = &tagState{content: NewContent(t.content, stamp), created: stamp.At, lastSeen: stamp.At, classic: classic}
		return
	}
	ts.content.Merge(t.content, stamp)
	ts.created, ts.lastSeen = MinTime(ts.created, stamp.At), MaxTime(ts.lastSeen, stamp.At)
	ts.classic = ts.classic || classic
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
	if p.auth != nil {
		if _, err := typeOf("authority.source", p.auth.source); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func (g *Graph) commit(o Observation, p *prepared, ids []string) Result {
	em := &emitter{g: g, tenant: o.TenantID, prog: o.ProgramID, origin: o.ObservationID, run: o.RunID, at: p.stamp.At,
		cause: p.cause, base: p.baseline}
	res := Result{RefToEntityID: make(map[string]string, len(ids))}
	idOf := func(ep endpoint) string {
		if ep.local >= 0 {
			return ids[ep.local]
		}
		return ep.id
	}
	// Authoritative edges leave the authority's source; their listed
	// targets (other than the source) are sighted authoritatively.
	srcID := ""
	authEdge := make([]bool, len(p.edges))
	authTarget := make([]bool, len(p.entities))
	if p.auth != nil {
		srcID = idOf(p.auth.source)
		for i, pe := range p.edges {
			if idOf(pe.from) == srcID {
				authEdge[i] = true
				if pe.to.local >= 0 && ids[pe.to.local] != srcID {
					authTarget[pe.to.local] = true
				}
			}
		}
	}
	tx := &authTx{g: g, touchedEnt: map[string]bool{}, touchedEdge: map[string]bool{}, tagBefore: map[tagRef]tagSnap{}}
	for i, pe := range p.entities {
		id := ids[i]
		res.RefToEntityID[pe.ref] = id
		classic := !authTarget[i]
		e, ok := g.entities[id]
		if !ok {
			e = &entityState{record: newRecord(id, o.TenantID, o.ProgramID, p, o.RunID, pe.content, classic), assetType: pe.assetType, key: pe.key}
			e.scope = g.scopeOf(e)
			g.entities[id] = e
			res.EntitiesCreated++
			em.emitEvent(ChangeEvent{Type: ChangeEntityAdded, EntityID: id, Base: entityBase(e)})
			if !classic {
				tx.touchedEnt[id] = true
			}
			continue
		}
		res.EntitiesUpdated++
		e.see(p, o.RunID, classic)
		if classic {
			if e.settle() {
				em.emit(ChangeEntityReactivated, id, "", nil)
			}
		} else {
			tx.touchedEnt[id] = true
		}
		if ops := e.content.Merge(pe.content, p.stamp); len(ops) > 0 {
			em.emit(ChangeEntityChanged, id, "", ops)
			if st := g.scopeOf(e); st != e.scope {
				e.scope = st
				em.emit(ChangeScopeStatusChanged, id, "", []PatchOp{{Op: "replace", Path: "/scope_status", Value: string(st)}})
			}
		}
	}
	for i, pe := range p.edges {
		k := EdgeKey{o.TenantID, o.ProgramID, idOf(pe.from), idOf(pe.to), pe.relType, pe.label, pe.discriminator}
		id := k.ID()
		classic := !authEdge[i]
		ed, ok := g.edges[id]
		if !ok {
			ed = &edgeState{record: newRecord(id, o.TenantID, o.ProgramID, p, o.RunID, pe.content, classic),
				relType: pe.relType, label: pe.label, disc: pe.discriminator, from: k.From, to: k.To}
			g.edges[id] = ed
			res.EdgesCreated++
			em.emitEvent(ChangeEvent{Type: ChangeEdgeAdded, EdgeID: id, Base: map[string]any{"content": cloneObject(ed.content.Values)}})
			if !classic {
				tx.touchedEdge[id] = true
			}
			continue
		}
		res.EdgesUpdated++
		ed.see(p, o.RunID, classic)
		if classic {
			if ed.settle() {
				em.emit(ChangeEdgeAdded, "", id, nil)
			}
		} else {
			tx.touchedEdge[id] = true
		}
		ed.content.Merge(pe.content, p.stamp)
	}
	var units map[string][]string // family -> members reported to the source's units
	if p.auth != nil {
		units = map[string][]string{}
		for i, pe := range p.edges {
			if authEdge[i] {
				fam := FamilyOfLabel(pe.label)
				id := EdgeKey{o.TenantID, o.ProgramID, idOf(pe.from), idOf(pe.to), pe.relType, pe.label, pe.discriminator}.ID()
				units[fam] = append(units[fam], id)
			}
		}
	}
	for _, t := range p.tags {
		var r *record
		ref := tagRef{ptype: t.ptype, name: t.name}
		if t.edge {
			r, ref.edge, ref.target = &g.edges[t.target.id].record, true, t.target.id
		} else {
			r, ref.target = &g.entities[idOf(t.target)].record, idOf(t.target)
		}
		auth := p.auth != nil && !t.edge && ref.target == srcID && t.source == nil
		if t.source == nil {
			tx.snapTag(ref, r)
		}
		r.tag(t, p.stamp, !auth)
		if auth {
			fam := FamilyOfProperty(t.ptype)
			units[fam] = append(units[fam], tagMember(t.ptype, t.name))
		}
	}
	if p.auth != nil {
		tx.applyAuthority(g.entities[srcID], p, units)
	}
	tx.settle(em)
	tx.emitTags(em)
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
