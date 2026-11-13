package bus

import (
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
)

// Topics of spec 11.7. They mirror convert.Topic*; the bus keeps its own
// copy so OrderingKey does not depend on the conversion package.
const (
	topicRunEvents      = "run.events"
	topicApprovalEvents = "approval.events"
	topicGraphChanges   = "graph.changes"
	topicScopeEvents    = "scope.events"
	topicAuthEvents     = "auth.events"
)

// OrderingKey returns the spec 11.7 ordering key of env on topic: run_id
// (run.events), approval_id (approval.events), program_id (graph.changes,
// scope.events) and user_id (auth.events). Other topics, including the
// dead-letter topics, have none ("").
//
// The key travels inside the envelope rather than as a Publish argument so
// the Bus port stays the same for every adapter and a relay never has to
// carry the outbox column separately (the outbox ordering_key column holds
// the same value).
func OrderingKey(topic string, env *pb.EventEnvelope) string {
	switch topic {
	case topicRunEvents:
		return env.GetRunEvent().GetRunId()
	case topicApprovalEvents:
		return env.GetApprovalEvent().GetApprovalId()
	case topicGraphChanges, topicScopeEvents:
		return env.GetProgramId()
	case topicAuthEvents:
		if v, ok := env.GetGeneric().GetFields()["user_id"]; ok {
			return v.GetStringValue()
		}
	}
	return ""
}

// Attributes are the message attributes: those of spec 11.7 (tenant_id and
// type; role and status on approval.events) plus event_id on every topic
// and traceparent when the envelope carries one, which this implementation
// adds. Pub/Sub filters and dead-letter tooling read them without decoding
// the envelope.
func Attributes(topic string, env *pb.EventEnvelope) map[string]string {
	a := map[string]string{"tenant_id": env.GetTenantId(), "type": env.GetType(), "event_id": env.GetEventId()}
	if tp := env.GetTraceparent(); tp != "" {
		a["traceparent"] = tp
	}
	if topic == topicApprovalEvents {
		a["role"] = env.GetApprovalEvent().GetRole()
		a["status"] = env.GetApprovalEvent().GetStatus()
	}
	return a
}
