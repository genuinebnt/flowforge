package graphstore

import (
	"context"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
)

// Mem is the in-memory Store: one oam.Graph (the reference merge) per
// (tenant, program), plus the indexes the read paths need. It is used by
// tests of other packages and by the dev stack. Safe for concurrent use;
// writes are serialized.
type Mem struct {
	cfg Config

	mu         sync.RWMutex
	progs      map[string]*memProgram
	entityProg map[string]*memProgram // entity id -> owning program
	edgeProg   map[string]*memProgram // edge id -> owning program
	applied    map[string]struct{}    // tenant NUL observation id
	snaps      map[string][]SnapshotInfo
	notify     Notifier
}

type memProgram struct {
	tenant, program ID
	g               *oam.Graph
	out, in         map[string][]string // entity id -> edge ids
	edges           map[string]struct{}
	lastSeq         int64
}

var _ Store = (*Mem)(nil)

// NewMem returns an empty in-memory store.
func NewMem(cfg Config) *Mem {
	return &Mem{
		cfg:   cfg.WithDefaults(),
		progs: map[string]*memProgram{}, entityProg: map[string]*memProgram{}, edgeProg: map[string]*memProgram{},
		applied: map[string]struct{}{}, snaps: map[string][]SnapshotInfo{},
	}
}

func progKey(tenant, program ID) string { return tenant.Text + "/" + program.Text }

// program returns the program's graph, creating it; m.mu must be held
// for writing.
func (m *Mem) program(tenant, program ID) *memProgram {
	k := progKey(tenant, program)
	p, ok := m.progs[k]
	if !ok {
		p = &memProgram{
			tenant: tenant, program: program,
			g:   oam.NewGraph(oam.Config{StaleAfterRuns: m.cfg.StaleAfterRuns, Scope: m.cfg.Scope}),
			out: map[string][]string{}, in: map[string][]string{}, edges: map[string]struct{}{},
		}
		m.progs[k] = p
	}
	return p
}

// index records new entities and edges named by change events.
func (m *Mem) index(p *memProgram, evs []oam.ChangeEvent) {
	for _, ev := range evs {
		p.lastSeq = max(p.lastSeq, ev.Seq)
		switch {
		case ev.Type == oam.ChangeEntityAdded:
			m.entityProg[ev.EntityID] = p
		case ev.Type == oam.ChangeEdgeAdded:
			if _, ok := p.edges[ev.EdgeID]; ok {
				continue
			}
			e, ok := p.g.Edge(ev.EdgeID)
			if !ok {
				continue
			}
			p.edges[e.ID] = struct{}{}
			p.out[e.FromEntity] = append(p.out[e.FromEntity], e.ID)
			p.in[e.ToEntity] = append(p.in[e.ToEntity], e.ID)
			m.edgeProg[e.ID] = p
		}
	}
}

// changes converts events, adding the entity snapshot; m.mu must be held.
func (m *Mem) changes(p *memProgram, evs []oam.ChangeEvent) []ChangeEvent {
	out := make([]ChangeEvent, 0, len(evs))
	for _, ev := range evs {
		c := ChangeFromOAM(ev)
		if ev.EntityID != "" {
			if e, ok := p.g.Entity(ev.EntityID); ok {
				v := EntityFromOAM(e, false)
				c.Entity = &v
			}
		}
		out = append(out, c)
	}
	return out
}

// IngestObservation implements Store.
func (m *Mem) IngestObservation(ctx context.Context, req IngestRequest) (IngestResult, error) {
	if err := ctx.Err(); err != nil {
		return IngestResult{}, Classify(err)
	}
	pi, err := PrepareIngest(req)
	if err != nil {
		return IngestResult{}, err
	}
	o := pi.Observation
	dk := pi.Tenant.Text + "\x00" + o.ObservationID
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.applied[dk]; dup {
		return IngestResult{Duplicate: true, RefToEntityID: map[string]string{}}, nil
	}
	p := m.program(pi.Tenant, pi.Program)
	res, err := p.g.Apply(o)
	if err != nil {
		return IngestResult{}, Classify(err)
	}
	m.applied[dk] = struct{}{}
	m.index(p, res.Changes)
	out := IngestResult{
		Duplicate: res.Duplicate, EntitiesCreated: res.EntitiesCreated, EntitiesUpdated: res.EntitiesUpdated,
		EdgesCreated: res.EdgesCreated, EdgesUpdated: res.EdgesUpdated,
		Changes: m.changes(p, res.Changes), RefToEntityID: res.RefToEntityID,
	}
	if len(res.Changes) > 0 {
		m.notify.Broadcast()
	}
	return out, nil
}

// SetStale implements Store.
func (m *Mem) SetStale(ctx context.Context, c Caller, req SetStaleRequest) (SetStaleResult, error) {
	if err := ctx.Err(); err != nil {
		return SetStaleResult{}, Classify(err)
	}
	tenant, err := TenantOf(c)
	if err != nil {
		return SetStaleResult{}, err
	}
	program, run, tool, err := ValidateSetStale(req)
	if err != nil {
		return SetStaleResult{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.program(tenant, program)
	res, err := p.g.CompleteRun(oam.CoveringRun{
		TenantID: tenant.Text, ProgramID: program.Text, RunID: run.Text, Tool: tool,
		AssetTypes: req.AssetTypes, StaleAfterRuns: req.StaleAfterRuns, CompletedAt: m.cfg.Now(),
	})
	if err != nil {
		return SetStaleResult{}, Classify(err)
	}
	m.index(p, res.Changes)
	if len(res.Changes) > 0 {
		m.notify.Broadcast()
	}
	return SetStaleResult{Duplicate: res.Duplicate, EntitiesMarked: res.EntitiesMarked, EdgesMarked: res.EdgesMarked,
		Changes: m.changes(p, res.Changes)}, nil
}

// AddTag implements Store. The tag is applied as an observation from
// source SourceAPI stamped with the clock, so it merges with ingested tags
// by the same last-writer-wins rule.
func (m *Mem) AddTag(ctx context.Context, c Caller, req AddTagRequest) (Tag, error) {
	tenant, err := TenantOf(c)
	if err != nil {
		return Tag{}, err
	}
	target, err := parseTarget(req)
	if err != nil {
		return Tag{}, err
	}
	prop, err := TagProperty(req)
	if err != nil {
		return Tag{}, err
	}
	m.mu.RLock()
	p := m.ownerOf(tenant, req.TargetKind, target)
	m.mu.RUnlock()
	if p == nil {
		return Tag{}, NotFound(string(req.TargetKind), target)
	}
	ir, err := AddTagIngest(tenant, p.program, target, prop, m.cfg.Now())
	if err != nil {
		return Tag{}, err
	}
	if _, err := m.IngestObservation(ctx, ir); err != nil {
		return Tag{}, err
	}
	ptype, name, _, _ := oam.SplitProperty(prop)
	m.mu.RLock()
	defer m.mu.RUnlock()
	var tags []oam.Tag
	if req.TargetKind == TargetEdge {
		e, _ := p.g.Edge(target)
		tags = e.Tags
	} else {
		e, _ := p.g.Entity(target)
		tags = e.Tags
	}
	for _, t := range tags {
		if t.PropertyType == ptype && t.Name == name {
			return TagFromOAM(tenant.Text, req.TargetKind, target, t), nil
		}
	}
	return Tag{}, Internal("add tag", errTagMissing)
}

// ownerOf returns the program owning a target of the tenant, or nil.
func (m *Mem) ownerOf(tenant ID, kind TargetKind, id string) *memProgram {
	idx := m.entityProg
	if kind == TargetEdge {
		idx = m.edgeProg
	}
	p := idx[id]
	if p == nil || p.tenant != tenant {
		return nil
	}
	return p
}

// parseTarget canonicalizes an AddTagRequest target.
func parseTarget(req AddTagRequest) (string, error) {
	switch req.TargetKind {
	case TargetEntity:
		id, err := ParseID(core.PrefixEntity, "target_id", req.TargetID)
		return id.Text, err
	case TargetEdge:
		id, err := ParseID(core.PrefixEdge, "target_id", req.TargetID)
		return id.Text, err
	}
	return "", InvalidArgument("target_kind", "target_kind must be entity or edge")
}

// AddTagIngest builds the observation that applies an API tag (source
// SourceAPI, observed at at). Its id is derived from the tag and the time,
// so a retried call at the same instant is deduplicated.
func AddTagIngest(tenant, program ID, target string, prop oam.Property, at time.Time) (IngestRequest, error) {
	ptype, name, content, err := oam.SplitProperty(prop)
	if err != nil {
		return IngestRequest{}, Classify(err)
	}
	canon, err := core.CanonicalJSON(content)
	if err != nil {
		return IngestRequest{}, Classify(err)
	}
	at = StoreTime(at)
	obsID := "api-tag:" + DeriveUUID("ff-graph-api-tag", tenant.Text, target, ptype, name, string(canon), at.Format(time.RFC3339Nano)).String()
	o := oam.Observation{
		ObservationID: obsID, Tool: oam.Tool{Name: SourceAPI}, ObservedAt: at,
		Tags: []oam.ObservedTag{{Target: target, Property: prop}},
	}
	return IngestRequest{TenantID: tenant.Text, ProgramID: program.Text, Observation: o}, nil
}

type errString string

func (e errString) Error() string { return string(e) }

const errTagMissing = errString("graphstore: tag missing after apply")
