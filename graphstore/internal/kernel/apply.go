package kernel

import (
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/oam"
)

// Event is one change event of an operation, before it gets a seq.
type Event struct {
	ID       string
	Type     oam.ChangeType
	EntityID string
	EdgeID   string
	RunID    string
	Patch    []oam.PatchOp
	At       time.Time
}

// Result reports the effect of Apply (oam.Result without seqs).
type Result struct {
	EntitiesCreated, EntitiesUpdated int
	EdgesCreated, EdgesUpdated       int
	RefToEntityID                    map[string]string
	Events                           []Event
}

type emitter struct {
	tenant, prog, origin, run string
	at                        time.Time
	out                       []Event
}

func (e *emitter) emit(typ oam.ChangeType, entityID, edgeID string, patch []oam.PatchOp) {
	u := graphstore.ChangeUUID(e.tenant, e.prog, e.origin, len(e.out))
	e.out = append(e.out, Event{ID: core.PublicID(core.PrefixChange, u), Type: typ, EntityID: entityID, EdgeID: edgeID,
		RunID: e.run, Patch: patch, At: e.at})
}

func invalidAt(path string, err error) error { return &oam.ValidationError{Path: path, Err: err} }

// Apply applies a prepared observation to st exactly as oam.Graph.Apply
// does. st must hold every existing record among p.EntityIDs() and
// p.EdgeIDs() of the observation's tenant and program (and nothing of
// other tenants or programs). On error st is unchanged.
func Apply(st *State, p *Prepared, scope oam.ScopeFunc) (Result, error) {
	tenant, prog := p.Obs.TenantID, p.Obs.ProgramID
	for _, e := range p.Entities {
		if ex, ok := st.Entities[e.ID]; ok && (ex.Key != e.Key || ex.AssetType != e.AssetType) {
			return Result{}, fmt.Errorf("oam: entity id collision for %s", e.Key)
		}
	}
	typeOf := func(path string, ep Endpoint) (string, error) {
		if ep.Local >= 0 {
			return p.Entities[ep.Local].AssetType, nil
		}
		e, ok := st.Entities[ep.ID]
		if !ok {
			return "", invalidAt(path, fmt.Errorf("%w: %s", oam.ErrUnknownEntity, ep.ID))
		}
		return e.AssetType, nil
	}
	for i, e := range p.Edges {
		path := fmt.Sprintf("edges[%d]", i)
		ft, err := typeOf(path+".from", e.From)
		if err != nil {
			return Result{}, err
		}
		tt, err := typeOf(path+".to", e.To)
		if err != nil {
			return Result{}, err
		}
		if e.From.Local < 0 || e.To.Local < 0 {
			if err := oam.ValidateEdge(e.Rel, ft, tt); err != nil {
				return Result{}, invalidAt(path+".relation", err)
			}
		}
	}
	for i, t := range p.Tags {
		path := fmt.Sprintf("tags[%d].target", i)
		if t.Edge {
			if _, ok := st.Edges[t.Target.ID]; !ok {
				return Result{}, invalidAt(path, fmt.Errorf("%w: %s", oam.ErrUnknownEntity, t.Target.ID))
			}
			continue
		}
		if _, err := typeOf(path, t.Target); err != nil {
			return Result{}, err
		}
	}

	em := &emitter{tenant: tenant, prog: prog, origin: p.Obs.ObservationID, run: p.Obs.RunID, at: p.Stamp.At}
	res := Result{RefToEntityID: make(map[string]string, len(p.Entities))}
	for _, pe := range p.Entities {
		id := pe.ID
		res.RefToEntityID[pe.Ref] = id
		e, ok := st.Entities[id]
		if !ok {
			e = &Entity{Record: newRecord(id, tenant, prog, p, pe.Content), AssetType: pe.AssetType, Key: pe.Key}
			e.Scope = ScopeOf(e, scope)
			st.Entities[id] = e
			res.EntitiesCreated++
			em.emit(oam.ChangeEntityAdded, id, "", nil)
			continue
		}
		res.EntitiesUpdated++
		if e.see(p) {
			em.emit(oam.ChangeEntityReactivated, id, "", nil)
		}
		if ops := e.Content.Merge(pe.Content, p.Stamp); len(ops) > 0 {
			em.emit(oam.ChangeEntityChanged, id, "", ops)
			if s := ScopeOf(e, scope); s != e.Scope {
				e.Scope = s
				em.emit(oam.ChangeScopeStatusChanged, id, "", []oam.PatchOp{{Op: "replace", Path: "/scope_status", Value: string(s)}})
			}
		}
	}
	for _, pe := range p.Edges {
		k := p.EdgeKey(pe)
		id := k.ID()
		ed, ok := st.Edges[id]
		if !ok {
			ed = &Edge{Record: newRecord(id, tenant, prog, p, pe.Content),
				RelType: pe.RelType, Label: pe.Label, Disc: pe.Disc, From: k.From, To: k.To}
			st.Edges[id] = ed
			res.EdgesCreated++
			em.emit(oam.ChangeEdgeAdded, "", id, nil)
			continue
		}
		res.EdgesUpdated++
		if ed.see(p) {
			em.emit(oam.ChangeEdgeAdded, "", id, nil)
		}
		ed.Content.Merge(pe.Content, p.Stamp)
	}
	for _, t := range p.Tags {
		if t.Edge {
			st.Edges[t.Target.ID].tag(t, p.Stamp)
		} else {
			st.Entities[p.IDOf(t.Target)].tag(t, p.Stamp)
		}
	}
	res.Events = em.out
	return res, nil
}

// ScopeOf classifies an entity's merged content (oam rule: anything but
// in or out of scope is unknown).
func ScopeOf(e *Entity, scope oam.ScopeFunc) oam.ScopeStatus {
	if scope == nil {
		return oam.ScopeUnknown
	}
	a, err := oam.JoinAsset(e.AssetType, e.Content.Values, oam.Lenient)
	if err != nil {
		return oam.ScopeUnknown
	}
	switch s := scope(e.Tenant, e.Program, a); s {
	case oam.ScopeInScope, oam.ScopeOutOfScope:
		return s
	}
	return oam.ScopeUnknown
}

// Cover applies one covering run of tool to r (oam rule) and reports
// whether r became stale.
func (r *Record) Cover(tool, run string, n int) bool {
	rs, ok := r.Runs[tool]
	if !ok {
		return false
	}
	r.Dirty = true
	if rs.Last.Run == run {
		rs.Missed = 0
	} else {
		rs.Missed++
	}
	rs.Covered = true
	if r.State != oam.StateActive {
		return false
	}
	for _, s := range r.Runs {
		if s.Covered && s.Missed < n {
			return false
		}
	}
	r.State = oam.StateStale
	return true
}

// CoverRun applies a covering run to every record in st that tool has
// reported (entities restricted to assetTypes when non-empty), in id
// order, and returns the entity_stale and edge_removed events, as
// oam.Graph.CompleteRun does.
func CoverRun(st *State, tenant, prog, tool, run string, assetTypes []string, n int, at time.Time) (entities, edges int, events []Event) {
	em := &emitter{tenant: tenant, prog: prog, origin: graphstore.CoveringRunOrigin(tool, run), run: run, at: at}
	for _, id := range slices.Sorted(maps.Keys(st.Entities)) {
		e := st.Entities[id]
		if len(assetTypes) > 0 && !slices.Contains(assetTypes, e.AssetType) {
			continue
		}
		if e.Cover(tool, run, n) {
			entities++
			em.emit(oam.ChangeEntityStale, id, "", nil)
		}
	}
	for _, id := range slices.Sorted(maps.Keys(st.Edges)) {
		if st.Edges[id].Cover(tool, run, n) {
			edges++
			em.emit(oam.ChangeEdgeRemoved, "", id, nil)
		}
	}
	return entities, edges, em.out
}

// Missing returns the entity and edge ids the observation references
// (edge endpoints and tag targets given as ent_/edg_ ids) that st does not
// hold, sorted and unique: the ids a store waits for before it applies the
// observation (pending references, G-CORE-03). st must have been loaded
// for p.EntityIDs() and p.EdgeIDs() as for Apply. Apply's label checks on
// those references run only once the ids exist, exactly as oam.Graph
// buffers.
func Missing(st *State, p *Prepared) []string {
	var out []string
	entity := func(ep Endpoint) {
		if ep.Local < 0 {
			if _, ok := st.Entities[ep.ID]; !ok {
				out = append(out, ep.ID)
			}
		}
	}
	for _, e := range p.Edges {
		entity(e.From)
		entity(e.To)
	}
	for _, t := range p.Tags {
		if !t.Edge {
			entity(t.Target)
		} else if _, ok := st.Edges[t.Target.ID]; !ok {
			out = append(out, t.Target.ID)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
