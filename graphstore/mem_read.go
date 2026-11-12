package graphstore

import (
	"context"
	"slices"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/csr"
	"github.com/genuinebnt/flowforge/oam"
)

// memReader is the traversal Reader over one program; the caller holds
// m.mu for reading.
type memReader struct{ p *memProgram }

func (r memReader) Entities(_ context.Context, ids []string) (map[string]Entity, error) {
	out := make(map[string]Entity, len(ids))
	for _, id := range ids {
		if e, ok := r.p.g.Entity(id); ok {
			out[id] = EntityFromOAM(e, false)
		}
	}
	return out, nil
}

func (r memReader) Edges(_ context.Context, ids []string, out, in bool, labels []string) ([]Edge, error) {
	seen := map[string]bool{}
	var res []Edge
	add := func(eids []string) {
		for _, id := range eids {
			if seen[id] {
				continue
			}
			seen[id] = true
			if e, ok := r.p.g.Edge(id); ok && (len(labels) == 0 || slices.Contains(labels, e.Label)) {
				res = append(res, EdgeFromOAM(e, false))
			}
		}
	}
	for _, id := range ids {
		if out {
			add(r.p.out[id])
		}
		if in {
			add(r.p.in[id])
		}
	}
	return res, nil
}

// entity returns a tenant's entity and its program; m.mu must be held.
func (m *Mem) entity(c Caller, field, id string) (*memProgram, oam.Entity, error) {
	tenant, err := TenantOf(c)
	if err != nil {
		return nil, oam.Entity{}, err
	}
	eid, err := EntityIDOf(field, id)
	if err != nil {
		return nil, oam.Entity{}, err
	}
	p := m.ownerOf(tenant, TargetEntity, eid.Text)
	if p == nil {
		return nil, oam.Entity{}, NotFound("entity", eid.Text)
	}
	e, ok := p.g.Entity(eid.Text)
	if !ok {
		return nil, oam.Entity{}, NotFound("entity", eid.Text)
	}
	return p, e, nil
}

// programOf returns the tenant's program graph, or nil when it has no
// data; m.mu must be held.
func (m *Mem) programOf(c Caller, programID string) (ID, ID, *memProgram, error) {
	tenant, err := TenantOf(c)
	if err != nil {
		return ID{}, ID{}, nil, err
	}
	prog, err := ParseID(core.PrefixProgram, "program_id", programID)
	if err != nil {
		return ID{}, ID{}, nil, err
	}
	return tenant, prog, m.progs[progKey(tenant, prog)], nil
}

// GetEntity implements Store.
func (m *Mem) GetEntity(ctx context.Context, c Caller, req GetEntityRequest) (Entity, error) {
	if err := ctx.Err(); err != nil {
		return Entity{}, Classify(err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, e, err := m.entity(c, "entity_id", req.EntityID)
	if err != nil {
		return Entity{}, err
	}
	return EntityFromOAM(e, req.IncludeTags), nil
}

// FindEntities implements Store.
func (m *Mem) FindEntities(ctx context.Context, c Caller, req FindEntitiesRequest) (FindEntitiesResult, error) {
	if err := ctx.Err(); err != nil {
		return FindEntitiesResult{}, Classify(err)
	}
	tenant, err := TenantOf(c)
	if err != nil {
		return FindEntitiesResult{}, err
	}
	q, err := ValidateFind(req)
	if err != nil {
		return FindEntitiesResult{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	p := m.progs[progKey(tenant, q.Program)]
	var res FindEntitiesResult
	if p == nil {
		return res, nil
	}
	for _, oe := range p.g.Entities() {
		if oe.ID <= q.After {
			continue
		}
		e := EntityFromOAM(oe, false)
		if !q.Match(e) {
			continue
		}
		if len(res.Entities) == q.Size {
			res.NextPageToken = EncodeIDToken(res.Entities[q.Size-1].ID)
			break
		}
		res.Entities = append(res.Entities, e)
	}
	return res, nil
}

// Neighbors implements Store.
func (m *Mem) Neighbors(ctx context.Context, c Caller, req NeighborsRequest) (Subgraph, error) {
	req, err := ValidateNeighbors(req)
	if err != nil {
		return Subgraph{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, e, err := m.entity(c, "entity_id", req.EntityID)
	if err != nil {
		return Subgraph{}, err
	}
	return TraverseNeighbors(ctx, memReader{p}, EntityFromOAM(e, false), req)
}

// FindPath implements Store.
func (m *Mem) FindPath(ctx context.Context, c Caller, req FindPathRequest) (Path, error) {
	req, err := ValidatePath(req)
	if err != nil {
		return Path{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, from, err := m.entity(c, "from_entity_id", req.FromEntityID)
	if err != nil {
		return Path{}, err
	}
	q, to, err := m.entity(c, "to_entity_id", req.ToEntityID)
	if err != nil {
		return Path{}, err
	}
	if p != q {
		return Path{}, nil
	}
	return TraversePath(ctx, memReader{p}, EntityFromOAM(from, false), EntityFromOAM(to, false), req)
}

// BlastRadius implements Store.
func (m *Mem) BlastRadius(ctx context.Context, c Caller, req BlastRadiusRequest) ([]Reach, error) {
	req, err := ValidateBlast(req)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, e, err := m.entity(c, "entity_id", req.EntityID)
	if err != nil {
		return nil, err
	}
	return TraverseBlastRadius(ctx, memReader{p}, EntityFromOAM(e, false), req)
}

// events returns the program's change events with seq > after that pass
// keep, at most limit, with entity snapshots; m.mu must be held.
func (m *Mem) events(p *memProgram, after int64, limit int, keep func(oam.ChangeEvent) bool) []ChangeEvent {
	var sel []oam.ChangeEvent
	for _, ev := range p.g.Changes() {
		if ev.Seq <= after || !keep(ev) {
			continue
		}
		sel = append(sel, ev)
		if len(sel) == limit {
			break
		}
	}
	return m.changes(p, sel)
}

// Diff implements Store.
func (m *Mem) Diff(ctx context.Context, c Caller, req DiffRequest) (DiffResult, error) {
	if err := ctx.Err(); err != nil {
		return DiffResult{}, Classify(err)
	}
	tenant, err := TenantOf(c)
	if err != nil {
		return DiffResult{}, err
	}
	q, err := ValidateDiff(req)
	if err != nil {
		return DiffResult{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	p := m.progs[progKey(tenant, q.Program)]
	if p == nil {
		if !q.RunA.IsZero() {
			return DiffResult{}, NotFound("run", q.RunA.Text)
		}
		return DiffResult{}, nil
	}
	keep := func(ev oam.ChangeEvent) bool { return !ev.At.Before(q.Since) }
	if !q.RunA.IsZero() {
		lastOf := func(run string) int64 {
			var s int64
			for _, ev := range p.g.Changes() {
				if ev.RunID == run {
					s = ev.Seq
				}
			}
			return s
		}
		lo := lastOf(q.RunA.Text)
		if lo == 0 {
			return DiffResult{}, NotFound("run", q.RunA.Text)
		}
		hi := p.lastSeq
		if !q.RunB.IsZero() {
			if hi = lastOf(q.RunB.Text); hi == 0 {
				return DiffResult{}, NotFound("run", q.RunB.Text)
			}
		}
		keep = func(ev oam.ChangeEvent) bool { return ev.Seq > lo && ev.Seq <= hi }
	}
	evs := m.events(p, q.After, q.Size+1, keep)
	var res DiffResult
	if len(evs) > q.Size {
		evs = evs[:q.Size]
		res.NextPageToken = EncodeSeqToken(evs[q.Size-1].Seq)
	}
	res.Changes = evs
	return res, nil
}

// Search implements Store.
func (m *Mem) Search(ctx context.Context, c Caller, req SearchRequest) (SearchResult, error) {
	if err := ctx.Err(); err != nil {
		return SearchResult{}, Classify(err)
	}
	query, limit, err := ValidateSearch(req)
	if err != nil {
		return SearchResult{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, _, p, err := m.programOf(c, req.ProgramID)
	if err != nil || p == nil {
		return SearchResult{}, err
	}
	var hits []SearchHit
	for _, oe := range p.g.Entities() {
		if len(req.AssetTypes) > 0 && !slices.Contains(req.AssetTypes, oe.AssetType) {
			continue
		}
		score, matched, ok := MatchSearch(query, SearchText(oe.Content), req.Fuzzy, m.cfg.FuzzyThreshold)
		if ok {
			hits = append(hits, SearchHit{Entity: EntityFromOAM(oe, false), Score: score, MatchedTrigrams: matched})
		}
	}
	SortHits(hits)
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return SearchResult{Hits: hits}, nil
}

// ReadChanges implements Store.
func (m *Mem) ReadChanges(ctx context.Context, c Caller, req ChangesRequest) ([]ChangeEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, Classify(err)
	}
	tenant, err := TenantOf(c)
	if err != nil {
		return nil, err
	}
	prog, limit, err := ValidateChanges(req)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	p := m.progs[progKey(tenant, prog)]
	if p == nil {
		return nil, nil
	}
	return m.events(p, req.AfterSeq, limit, func(ev oam.ChangeEvent) bool {
		return len(req.Types) == 0 || slices.Contains(req.Types, ev.Type)
	}), nil
}

// WatchChanges implements Store.
func (m *Mem) WatchChanges(ctx context.Context, c Caller, req WatchRequest, fn func(ChangeEvent) error) error {
	cr := ChangesRequest{ProgramID: req.ProgramID, AfterSeq: req.AfterSeq, Types: req.Types, Limit: MaxPageSize}
	if _, _, err := ValidateChanges(cr); err != nil {
		return err
	}
	if _, err := TenantOf(c); err != nil {
		return err
	}
	read := func(ctx context.Context, after int64) ([]ChangeEvent, error) {
		cr.AfterSeq = after
		return m.ReadChanges(ctx, c, cr)
	}
	return WatchLoop(ctx, req.AfterSeq, read, &m.notify, 0, fn)
}

// Export implements Store.
func (m *Mem) Export(ctx context.Context, c Caller, programID string) (Export, error) {
	if err := ctx.Err(); err != nil {
		return Export{}, Classify(err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, _, p, err := m.programOf(c, programID)
	ex := Export{Entities: []Entity{}, Edges: []Edge{}}
	if err != nil || p == nil {
		return ex, err
	}
	for _, e := range p.g.Entities() {
		ex.Entities = append(ex.Entities, EntityFromOAM(e, true))
	}
	for _, e := range p.g.Edges() {
		ex.Edges = append(ex.Edges, EdgeFromOAM(e, true))
	}
	return ex, nil
}

// BuildSnapshot implements Store.
func (m *Mem) BuildSnapshot(ctx context.Context, c Caller, programID string) (*csr.Graph, uint64, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, Classify(err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	tenant, prog, p, err := m.programOf(c, programID)
	if err != nil {
		return nil, 0, err
	}
	var ents []Entity
	var edges []Edge
	var wm uint64
	if p != nil {
		for _, e := range p.g.Entities() {
			ents = append(ents, EntityFromOAM(e, false))
		}
		for _, e := range p.g.Edges() {
			edges = append(edges, EdgeFromOAM(e, false))
		}
		wm = uint64(p.lastSeq)
	}
	g, err := BuildCSR(SnapshotMeta(tenant, prog, wm, m.cfg.Now()), ents, edges)
	return g, wm, err
}

// Components implements Store.
func (m *Mem) Components(ctx context.Context, c Caller, programID string) (Components, error) {
	g, wm, err := m.BuildSnapshot(ctx, c, programID)
	if err != nil {
		return Components{}, err
	}
	return ComponentsOf(g, wm)
}

// ComponentsOf runs weakly connected components on a snapshot.
func ComponentsOf(g *csr.Graph, watermark uint64) (Components, error) {
	part := csr.WeaklyConnectedComponents(g)
	out := Components{WatermarkSeq: watermark, Groups: make([][]string, 0, part.Count())}
	for _, grp := range part.Groups() {
		ids := make([]string, 0, len(grp))
		for _, v := range grp {
			u, err := g.NodeID(v)
			if err != nil {
				return Components{}, Internal("components", err)
			}
			ids = append(ids, core.PublicID(core.PrefixEntity, u))
		}
		out.Groups = append(out.Groups, ids)
	}
	return out, nil
}

// WriteSnapshot implements Store.
func (m *Mem) WriteSnapshot(ctx context.Context, c Caller, programID string, blobs BlobStore) (SnapshotInfo, error) {
	g, wm, err := m.BuildSnapshot(ctx, c, programID)
	if err != nil {
		return SnapshotInfo{}, err
	}
	tenant, _ := TenantOf(c)
	prog, _ := ParseID(core.PrefixProgram, "program_id", programID)
	info, err := PublishSnapshot(ctx, g, tenant, prog, wm, blobs, m.cfg.Now())
	if err != nil {
		return SnapshotInfo{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := progKey(tenant, prog)
	for _, s := range m.snaps[k] {
		if s.WatermarkSeq == wm {
			return s, nil
		}
	}
	m.snaps[k] = append(m.snaps[k], info)
	return info, nil
}

// LatestSnapshot implements Store.
func (m *Mem) LatestSnapshot(ctx context.Context, c Caller, programID string) (SnapshotInfo, error) {
	if err := ctx.Err(); err != nil {
		return SnapshotInfo{}, Classify(err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	tenant, prog, _, err := m.programOf(c, programID)
	if err != nil {
		return SnapshotInfo{}, err
	}
	var best *SnapshotInfo
	for i, s := range m.snaps[progKey(tenant, prog)] {
		if best == nil || s.WatermarkSeq > best.WatermarkSeq {
			best = &m.snaps[progKey(tenant, prog)][i]
		}
	}
	if best == nil {
		return SnapshotInfo{}, NotFound("snapshot of program", prog.Text)
	}
	return *best, nil
}
