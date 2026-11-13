package graphstore

import (
	"context"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
)

// memEvents is the EventSource of one program; m.mu is held.
type memEvents struct{ p *memProgram }

func (s memEvents) events(ids []string, at int64, entity bool) map[string][]ChangeEvent {
	want := make(map[string]bool, len(ids))
	for _, id := range ids {
		want[id] = true
	}
	out := map[string][]ChangeEvent{}
	for _, ev := range s.p.g.Changes() {
		if ev.Seq > at {
			break
		}
		id := ev.EdgeID
		if entity {
			id = ev.EntityID
		}
		if id != "" && want[id] {
			out[id] = append(out[id], ChangeFromOAM(ev))
		}
	}
	return out
}

func (s memEvents) EntityEvents(_ context.Context, ids []string, at int64) (map[string][]ChangeEvent, error) {
	return s.events(ids, at, true), nil
}

func (s memEvents) EdgeEvents(_ context.Context, ids []string, at int64) (map[string][]ChangeEvent, error) {
	return s.events(ids, at, false), nil
}

// RecordContentChange implements Store.
func (m *Mem) RecordContentChange(ctx context.Context, c Caller, req ContentChange) (ChangeEvent, error) {
	if err := ctx.Err(); err != nil {
		return ChangeEvent{}, Classify(err)
	}
	run, err := ParseOptionalID(core.PrefixRun, "run_id", req.RunID)
	if err != nil {
		return ChangeEvent{}, err
	}
	at := StoreTime(req.At)
	if req.At.IsZero() {
		at = m.cfg.Now()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, e, err := m.entity(c, "entity_id", req.EntityID)
	if err != nil {
		return ChangeEvent{}, err
	}
	ev, _, err := p.g.RecordContentChange(oam.ContentChange{TenantID: e.TenantID, ProgramID: e.ProgramID, EntityID: e.ID,
		RunID: run.Text, At: at, Patch: req.Patch})
	if err != nil {
		return ChangeEvent{}, Classify(err)
	}
	m.index(p, []oam.ChangeEvent{ev})
	m.notify.Broadcast()
	return m.changes(p, []oam.ChangeEvent{ev})[0], nil
}

// GetEntityAsOf implements Store.
func (m *Mem) GetEntityAsOf(ctx context.Context, c Caller, req GetEntityAsOfRequest) (Entity, error) {
	if err := ctx.Err(); err != nil {
		return Entity{}, Classify(err)
	}
	if err := ValidateAsOfSeq(req.Seq); err != nil {
		return Entity{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, e, err := m.entity(c, "entity_id", req.EntityID)
	if err != nil {
		return Entity{}, err
	}
	return GetAsOf(ctx, memEvents{p}, EntityFromOAM(e, false), req.Seq, req.IncludeTags)
}

// NeighborsAsOf implements Store.
func (m *Mem) NeighborsAsOf(ctx context.Context, c Caller, req NeighborsAsOfRequest) (Subgraph, error) {
	req, err := ValidateNeighborsAsOf(req)
	if err != nil {
		return Subgraph{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, e, err := m.entity(c, "entity_id", req.EntityID)
	if err != nil {
		return Subgraph{}, err
	}
	start, err := GetAsOf(ctx, memEvents{p}, EntityFromOAM(e, false), req.Seq, false)
	if err != nil {
		return Subgraph{}, err
	}
	return TraverseNeighbors(ctx, AsOfReader{Cur: memReader{p}, Ev: memEvents{p}, At: req.Seq}, start, req.NeighborsRequest)
}

// Commonness implements Store.
func (m *Mem) Commonness(ctx context.Context, c Caller, req CommonnessRequest) (Commonness, error) {
	if err := ctx.Err(); err != nil {
		return Commonness{}, Classify(err)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, e, err := m.entity(c, "entity_id", req.EntityID)
	if err != nil {
		return Commonness{}, err
	}
	return ComputeCommonness(ctx, memReader{p}, EntityFromOAM(e, false))
}
