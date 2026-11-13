package contentstore

import (
	"encoding/json"
	"time"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
)

// TopicContentChanges is the bus topic of content events (spec 11.7
// style; ordering key program_id). Consumers: the graph service (ingests
// the observation and records content_changed) and the notifier (alerts
// on alertable changes).
const TopicContentChanges = "content.changes"

// Content event types.
const (
	// EventChanged: a capture whose change set is a change (structural or
	// level 3), or a key's baseline.
	EventChanged = "content.changed"
	// EventCaptured: any other accepted capture (no change, masked-only,
	// reordered, degraded, or an unchanged re-fetch); it still carries the
	// observation so the graph refreshes last_seen and authority.
	EventCaptured = "content.captured"
)

// ChangeRef is one level-3 change as carried by an event (the feature
// value stays in the store; events name kind and key only).
type ChangeRef struct {
	Op      string `json:"op"`
	Kind    string `json:"kind"`
	Key     string `json:"key"`
	Newness string `json:"newness,omitempty"`
}

// maxEventChanges bounds EventPayload.Changes.
const maxEventChanges = 100

// EventPayload is the payload of a content event.
type EventPayload struct {
	SnapshotID   string `json:"snapshot_id"`
	SnapshotFrom string `json:"snapshot_from,omitempty"`
	DiffID       string `json:"diff_id,omitempty"`
	URL          string `json:"url"`
	Profile      string `json:"profile"`
	EntityID     string `json:"entity_id,omitempty"`
	RunID        string `json:"run_id,omitempty"`
	EffectPath   string `json:"effect_path,omitempty"`
	// Observation is the oam observation (content.ObservationJSON), as a
	// string so that numbers survive the generic Struct envelope exactly.
	Observation string `json:"observation"`
	// ContentChange is the content_changed patch (oam), present when the
	// capture is a change of a stored predecessor or a baseline.
	ContentChange *oam.ContentChangePatch `json:"content_change,omitempty"`
	Outcome       string                  `json:"outcome"`
	Class         string                  `json:"class,omitempty"`
	Newness       string                  `json:"newness,omitempty"`
	Cause         string                  `json:"cause"`
	Baseline      bool                    `json:"baseline,omitempty"`
	Score         float64                 `json:"score"`
	ScoreFeatures []oam.ScoreFeature      `json:"score_features,omitempty"`
	Changes       []ChangeRef             `json:"changes,omitempty"`
	// Alertable is false for baselines, non-target causes, pre-existing
	// content (a key's first capture, complete or not) and non-changes:
	// V2 §12.2, §12.3, §12.13, default feeds and alerts hide them.
	Alertable bool `json:"alertable"`
}

// outboxJSON is the outbox row format shared by every service
// (services/internal/convert OutboxJSON).
type outboxJSON struct {
	EventID    string          `json:"event_id"`
	Type       string          `json:"type"`
	Schema     int             `json:"schema_version"`
	TenantID   string          `json:"tenant_id"`
	ProgramID  string          `json:"program_id,omitempty"`
	OccurredAt string          `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload"`
}

// newEvent builds the outbox event of one capture.
func newEvent(id, tenant, program core.UUID, typ string, at time.Time, p EventPayload) (OutboxEvent, error) {
	pb, err := json.Marshal(p)
	if err != nil {
		return OutboxEvent{}, internal("event payload", err)
	}
	b, err := json.Marshal(outboxJSON{EventID: id.String(), Type: typ, Schema: 1, TenantID: tenant.String(),
		ProgramID: program.String(), OccurredAt: core.FormatTime(at), Payload: pb})
	if err != nil {
		return OutboxEvent{}, internal("event", err)
	}
	return OutboxEvent{EventID: id, Topic: TopicContentChanges, OrderingKey: program.String(), Payload: b}, nil
}

// eventPayload fills the change fields of a payload from a change set.
func eventPayload(p EventPayload, cs *content.ChangeSet, from, to, diffID string) (EventPayload, error) {
	if cs == nil {
		p.Cause = oam.CauseTarget
		p.Outcome = content.DiffNoChange
		return p, nil
	}
	p.Outcome, p.Class, p.Newness, p.Cause, p.Baseline, p.Score = cs.Outcome, cs.Class, cs.Newness, cs.Cause, cs.Baseline, cs.Score.Value
	for _, r := range cs.Score.Reasons {
		p.ScoreFeatures = append(p.ScoreFeatures, oam.ScoreFeature{Name: r.Name, Weight: r.Weight})
	}
	for _, c := range cs.Changes {
		if len(p.Changes) == maxEventChanges {
			break
		}
		p.Changes = append(p.Changes, ChangeRef{Op: c.Op, Kind: string(c.Feature.Kind), Key: c.Feature.Key, Newness: c.Newness})
	}
	if cs.IsChange() && len(cs.Levels) > 0 && diffID != "" {
		patch, err := cs.Patch(from, to, diffID)
		if err != nil {
			return p, internal("content change patch", err)
		}
		p.ContentChange = &patch
	}
	p.Alertable = Alertable(*cs)
	return p, nil
}

// Alertable reports a change set default feeds and alerts show: a change
// (structural or level 3) that is not a baseline, has cause target, is not
// degraded and is not pre-existing content (V2 §12.2, §12.3, §12.13).
func Alertable(cs content.ChangeSet) bool {
	return cs.IsChange() && !cs.Baseline && cs.Cause == oam.CauseTarget && cs.Outcome != content.DiffDegraded &&
		cs.Newness != content.NewnessPreExisting
}
