package convert

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/graphstore"
)

// Topics of spec 11.7.
const (
	TopicRunEvents      = "run.events"
	TopicApprovalEvents = "approval.events"
	TopicGraphChanges   = "graph.changes"
	TopicScopeEvents    = "scope.events"
	TopicAuthEvents     = "auth.events"
)

// publicID renders a UUID or public id string with prefix p ("" and
// strings that are neither stay as they are).
func publicID(p core.Prefix, s string) string {
	if s == "" {
		return ""
	}
	u, err := core.ParsePublicID(p, s)
	if err != nil {
		return s
	}
	return core.PublicID(p, u)
}

// OutboxJSON is the row payload of every outbox table (engine.outbox,
// scope.outbox, identity.outbox): the envelope with a JSON payload.
type OutboxJSON struct {
	EventID    string          `json:"event_id"`
	Type       string          `json:"type"`
	Schema     int             `json:"schema_version"`
	TenantID   string          `json:"tenant_id"`
	ProgramID  string          `json:"program_id,omitempty"`
	OccurredAt string          `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload"`
	// Generic is the payload key identity.outbox rows use (the gateway
	// writes the envelope's oneof name); it is read when Payload is absent.
	Generic json.RawMessage `json:"generic,omitempty"`
}

// EnvelopeFromOutbox builds the proto envelope of an outbox row on topic.
// run.events and approval.events get their typed payloads; every other
// topic carries the payload as a generic Struct.
func EnvelopeFromOutbox(topic string, raw []byte, traceparent string) (*pb.EventEnvelope, error) {
	var o OutboxJSON
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, err
	}
	if len(o.Payload) == 0 {
		o.Payload = o.Generic
	}
	at, _ := core.ParseTime(o.OccurredAt)
	env := &pb.EventEnvelope{EventId: o.EventID, Type: o.Type, SchemaVersion: int32(max(o.Schema, 1)),
		TenantId: publicID(core.PrefixTenant, o.TenantID), ProgramId: publicID(core.PrefixProgram, o.ProgramID),
		OccurredAt: TS(at), Traceparent: traceparent}
	switch topic {
	case TopicRunEvents:
		var p struct {
			RunID   string          `json:"run_id"`
			Seq     int64           `json:"seq"`
			Type    string          `json:"type"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(o.Payload, &p); err != nil {
			return nil, err
		}
		env.Payload = &pb.EventEnvelope_RunEvent{RunEvent: &pb.RunEventPayload{RunId: publicID(core.PrefixRun, p.RunID),
			Seq: p.Seq, Type: p.Type, Payload: MustStruct(p.Payload)}}
	case TopicApprovalEvents:
		var p struct {
			ApprovalID string `json:"approval_id"`
			RunID      string `json:"run_id"`
			Role       string `json:"role"`
			Status     string `json:"status"`
			Reason     string `json:"reason"`
			ExpiresAt  string `json:"expires_at"`
		}
		if err := json.Unmarshal(o.Payload, &p); err != nil {
			return nil, err
		}
		exp, _ := core.ParseTime(p.ExpiresAt)
		// The engine names a new approval by its status (pending); the
		// event vocabulary of spec 15.2 calls it approval.requested.
		if env.Type == "approval."+engine.ApprovalPending {
			env.Type = "approval.requested"
		}
		env.Payload = &pb.EventEnvelope_ApprovalEvent{ApprovalEvent: &pb.ApprovalEventPayload{
			ApprovalId: publicID(core.PrefixApproval, p.ApprovalID), RunId: publicID(core.PrefixRun, p.RunID),
			Role: p.Role, Status: p.Status, Reason: p.Reason, ExpiresAt: TS(exp)}}
	default:
		env.Payload = &pb.EventEnvelope_Generic{Generic: MustStruct(o.Payload)}
	}
	return env, nil
}

// OutboxFromEngine renders an engine envelope as an outbox row payload
// (the same JSON pgrepo.Outbox writes).
func OutboxFromEngine(e engine.Envelope) []byte {
	o := OutboxJSON{EventID: e.EventID.String(), Type: e.Type, Schema: 1, TenantID: e.Tenant.String(),
		OccurredAt: core.FormatTime(e.OccurredAt), Payload: e.Payload}
	if !e.Program.IsZero() {
		o.ProgramID = e.Program.String()
	}
	b, _ := json.Marshal(o)
	return b
}

// EnvelopeFromEngine converts an engine outbox envelope.
func EnvelopeFromEngine(e engine.Envelope, traceparent string) (*pb.EventEnvelope, error) {
	return EnvelopeFromOutbox(e.Topic, OutboxFromEngine(e), traceparent)
}

// GenericOutbox builds the scope.outbox payload of a scope.* event, whose
// payload EnvelopeFromOutbox carries as a generic Struct. (auth.* events
// use the same shape, written by the gateway's identity service with the
// payload under "generic".)
func GenericOutbox(eventID core.UUID, typ string, tenant, program core.UUID, at time.Time, payload map[string]any) []byte {
	p, _ := json.Marshal(payload)
	o := OutboxJSON{EventID: eventID.String(), Type: typ, Schema: 1, TenantID: tenant.String(), OccurredAt: core.FormatTime(at), Payload: p}
	if !program.IsZero() {
		o.ProgramID = program.String()
	}
	b, _ := json.Marshal(o)
	return b
}

// ChangeEnvelope wraps a graph change event for graph.changes.
func ChangeEnvelope(c graphstore.ChangeEvent, traceparent string) *pb.EventEnvelope {
	return &pb.EventEnvelope{EventId: c.ID, Type: "graph." + string(c.Type), SchemaVersion: 1,
		TenantId: publicID(core.PrefixTenant, c.TenantID), ProgramId: publicID(core.PrefixProgram, c.ProgramID),
		OccurredAt: TS(c.At), Traceparent: traceparent, Payload: &pb.EventEnvelope_GraphChange{GraphChange: ChangeProto(c)}}
}

// EnvelopeTenant returns the tenant of an envelope.
func EnvelopeTenant(env *pb.EventEnvelope) (core.UUID, error) {
	return core.ParsePublicID(core.PrefixTenant, env.GetTenantId())
}

// EnvelopeProgram returns the program of an envelope (zero when absent).
func EnvelopeProgram(env *pb.EventEnvelope) core.UUID {
	u, _ := core.ParsePublicID(core.PrefixProgram, env.GetProgramId())
	return u
}

// EventUUID returns the UUID of an envelope's event id (public ids of any
// prefix and bare UUIDs are accepted).
func EventUUID(id string) (core.UUID, error) {
	if i := strings.LastIndexByte(id, '_'); i > 0 {
		return core.ParsePublicID(core.Prefix(id[:i]), id)
	}
	return core.ParseUUID(id)
}
