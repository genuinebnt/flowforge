package oam

import (
	"strconv"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// ChangeType names a graph change event (spec 5.7, 10.5).
type ChangeType string

// Change event types.
const (
	ChangeEntityAdded        ChangeType = "entity_added"
	ChangeEntityChanged      ChangeType = "entity_changed"
	ChangeEntityStale        ChangeType = "entity_stale"
	ChangeEntityReactivated  ChangeType = "entity_reactivated"
	ChangeEdgeAdded          ChangeType = "edge_added"
	ChangeEdgeRemoved        ChangeType = "edge_removed"
	ChangeScopeStatusChanged ChangeType = "scope_status_changed"
)

// ChangeEvent is one effective change (the ChangeEvent message of spec
// 11.6 without the denormalized entity snapshot, which the graph service
// adds). Events record history: unlike the graph state they depend on the
// order observations arrived in.
type ChangeEvent struct {
	ID        string     `json:"id"`
	Seq       int64      `json:"seq"`
	TenantID  string     `json:"tenant_id,omitempty"`
	ProgramID string     `json:"program_id,omitempty"`
	Type      ChangeType `json:"type"`
	EntityID  string     `json:"entity_id,omitempty"`
	EdgeID    string     `json:"edge_id,omitempty"`
	RunID     string     `json:"run_id,omitempty"`
	Patch     []PatchOp  `json:"patch,omitempty"`
	At        time.Time  `json:"at"`
}

// emitter numbers the events of one graph operation and derives their ids
// from the operation's identity, so replays produce the same ids.
type emitter struct {
	g      *Graph
	tenant string
	prog   string
	origin string // observation id or "run:" + run id
	run    string
	at     time.Time
	out    []ChangeEvent
}

func (e *emitter) emit(typ ChangeType, entityID, edgeID string, patch []PatchOp) {
	u := derivedUUID("ff-oam-change", e.tenant, e.prog, e.origin, strconv.Itoa(len(e.out)))
	ev := ChangeEvent{
		ID: core.PublicID(core.PrefixChange, u), Seq: int64(len(e.g.changes) + 1),
		TenantID: e.tenant, ProgramID: e.prog, Type: typ,
		EntityID: entityID, EdgeID: edgeID, RunID: e.run, Patch: patch, At: e.at,
	}
	e.g.changes = append(e.g.changes, ev)
	e.out = append(e.out, ev)
}
