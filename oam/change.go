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
	// ChangeTagChanged (V2 §3.4): a visible entity or edge tag was
	// created, changed or removed. TagType and TagName name the tag; the
	// patch is a JSON patch of the tag content: [{"op":"add","path":"",
	// "value":{...}}] when it appears, [{"op":"remove","path":""}] when it
	// disappears, field-level add/replace ops otherwise. SourceProperty
	// tags (provenance) emit no events.
	ChangeTagChanged ChangeType = "tag_changed"
	// ChangeContentChanged (V2 §3.4): the content service recorded a diff
	// between two snapshots of an entity (ContentChange carries it).
	ChangeContentChanged ChangeType = "content_changed"
)

// ChangeTypes lists every change type, in the order of the SQL check
// constraint.
func ChangeTypes() []ChangeType {
	return []ChangeType{ChangeEntityAdded, ChangeEntityChanged, ChangeEntityStale, ChangeEntityReactivated,
		ChangeEdgeAdded, ChangeEdgeRemoved, ChangeScopeStatusChanged, ChangeContentChanged, ChangeTagChanged}
}

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
	// TagType and TagName name the tag of a tag_changed event.
	TagType string `json:"tag_type,omitempty"`
	TagName string `json:"tag_name,omitempty"`
	// Base is the state of a record right after its creation, for
	// replaying events (AsOf reads): {"content": {...}, "scope_status":
	// "..."} on entity_added, {"content": {...}} on the edge_added that
	// created an edge (absent on a reactivation).
	Base map[string]any `json:"base,omitempty"`
	// ContentChange is the patch of a content_changed event.
	ContentChange *ContentChangePatch `json:"content_change,omitempty"`
	// Cause and Baseline are copied from the observation (or content
	// change) that produced the event; "" means target.
	Cause    string `json:"cause,omitempty"`
	Baseline bool   `json:"baseline,omitempty"`
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
	cause  string
	base   bool
	out    []ChangeEvent
}

func (e *emitter) emit(typ ChangeType, entityID, edgeID string, patch []PatchOp) {
	e.emitEvent(ChangeEvent{Type: typ, EntityID: entityID, EdgeID: edgeID, Patch: patch})
}

// emitEvent fills the operation's fields (id, seq, tenant, program, run,
// time, cause, baseline) into ev and records it.
func (e *emitter) emitEvent(ev ChangeEvent) {
	u := derivedUUID("ff-oam-change", e.tenant, e.prog, e.origin, strconv.Itoa(len(e.out)))
	ev.ID, ev.Seq = core.PublicID(core.PrefixChange, u), int64(len(e.g.changes)+1)
	ev.TenantID, ev.ProgramID, ev.RunID, ev.At = e.tenant, e.prog, e.run, e.at
	ev.Cause, ev.Baseline = e.cause, e.base
	e.g.changes = append(e.g.changes, ev)
	e.out = append(e.out, ev)
}
