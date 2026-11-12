package pgrepo

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
)

// Outbox is engine.Publisher over engine.outbox (spec 10.4, 23.7). A
// relay publishes the rows to Pub/Sub and sets published_at. Rows are
// keyed by the envelope's deterministic event id, so republishing after a
// crash inserts nothing.
type Outbox struct {
	db DB
}

var _ engine.Publisher = (*Outbox)(nil)

// NewOutbox returns an outbox over db.
func NewOutbox(db DB) *Outbox { return &Outbox{db: db} }

type envelopeJSON struct {
	EventID    string          `json:"event_id"`
	Type       string          `json:"type"`
	Schema     int             `json:"schema_version"`
	TenantID   string          `json:"tenant_id"`
	ProgramID  string          `json:"program_id,omitempty"`
	OccurredAt string          `json:"occurred_at"`
	Payload    json.RawMessage `json:"payload"`
}

// Publish implements engine.Publisher.
func (o *Outbox) Publish(ctx context.Context, envs []engine.Envelope) error {
	if len(envs) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, e := range envs {
		ej := envelopeJSON{EventID: e.EventID.String(), Type: e.Type, Schema: 1, TenantID: e.Tenant.String(),
			OccurredAt: core.FormatTime(e.OccurredAt), Payload: e.Payload}
		if !e.Program.IsZero() {
			ej.ProgramID = e.Program.String()
		}
		payload, err := json.Marshal(ej)
		if err != nil {
			return err
		}
		b.Queue(`INSERT INTO engine.outbox (event_id, topic, ordering_key, payload) VALUES ($1, $2, $3, $4::text::jsonb)
ON CONFLICT (event_id) DO NOTHING`, pgUUID(e.EventID), e.Topic, pgOptText(e.OrderingKey), string(payload))
	}
	tx, err := o.db.Begin(ctx)
	if err != nil {
		return mapErr("publish", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := tx.SendBatch(ctx, b).Close(); err != nil {
		return mapErr("publish", err)
	}
	return mapErr("publish", tx.Commit(ctx))
}
