package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
)

// PgRepo is Repo over the notifier schema (migrations 0006 and 0014). The
// delivery worker spans tenants, so it connects with a role that bypasses
// row-level security; tenant-scoped queries filter by tenant explicitly.
type PgRepo struct{ DB bus.DB }

var _ Repo = (*PgRepo)(nil)

func pgErr(err error) error {
	var pe *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case errors.As(err, &pe) && pe.Code == "23505":
		return ErrExists
	}
	return err
}

func uuidOrNil(u core.UUID) *string {
	if u.IsZero() {
		return nil
	}
	s := u.String()
	return &s
}

const ruleCols = `id::text, tenant_id::text, coalesce(program_id::text, ''), name, source, compiled::text, enabled, version, created_by::text, created_at`

func scanRule(row pgx.Row) (Rule, error) {
	var r Rule
	var id, tenant, prog, compiled, by string
	if err := row.Scan(&id, &tenant, &prog, &r.Name, &r.Source, &compiled, &r.Enabled, &r.Version, &by, &r.CreatedAt); err != nil {
		return Rule{}, pgErr(err)
	}
	r.ID, r.Tenant, r.Compiled = core.MustParseUUID(id), core.MustParseUUID(tenant), json.RawMessage(compiled)
	if prog != "" {
		r.Program = core.MustParseUUID(prog)
	}
	if u := core.MustParseUUID(by); !u.IsZero() {
		r.CreatedBy = "user:" + u.String()
	}
	return r, nil
}

// PutRule implements Repo.
func (p *PgRepo) PutRule(ctx context.Context, r Rule, create bool) (Rule, error) {
	compiled := string(r.Compiled)
	if compiled == "" {
		compiled = "{}"
	}
	var row pgx.Row
	if create {
		row = p.DB.QueryRow(ctx, `INSERT INTO notifier.alert_rules (id, tenant_id, program_id, name, source, compiled, enabled, version, created_by, created_at)
VALUES ($1, $2, $3, $4, $5, $6::text::jsonb, $7, 1, $8, $9) RETURNING `+ruleCols,
			r.ID.String(), r.Tenant.String(), uuidOrNil(r.Program), r.Name, r.Source, compiled, r.Enabled, scopesvc.ActorUUID(r.CreatedBy).String(), r.CreatedAt)
	} else {
		row = p.DB.QueryRow(ctx, `UPDATE notifier.alert_rules SET program_id = $3, name = $4, source = $5, compiled = $6::text::jsonb, enabled = $7, version = version + 1
WHERE id = $1 AND tenant_id = $2 RETURNING `+ruleCols,
			r.ID.String(), r.Tenant.String(), uuidOrNil(r.Program), r.Name, r.Source, compiled, r.Enabled)
	}
	return scanRule(row)
}

// GetRule implements Repo.
func (p *PgRepo) GetRule(ctx context.Context, tenant, id core.UUID) (Rule, error) {
	return scanRule(p.DB.QueryRow(ctx, `SELECT `+ruleCols+` FROM notifier.alert_rules WHERE id = $1 AND tenant_id = $2`, id.String(), tenant.String()))
}

func (p *PgRepo) rules(ctx context.Context, q string, args ...any) ([]Rule, error) {
	rows, err := p.DB.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRules implements Repo.
func (p *PgRepo) ListRules(ctx context.Context, tenant, after core.UUID, limit int) ([]Rule, error) {
	return p.rules(ctx, `SELECT `+ruleCols+` FROM notifier.alert_rules WHERE tenant_id = $1 AND id > $2 ORDER BY id LIMIT $3`, tenant.String(), after.String(), limit)
}

// EnabledRules implements Repo.
func (p *PgRepo) EnabledRules(ctx context.Context, tenant core.UUID) ([]Rule, error) {
	return p.rules(ctx, `SELECT `+ruleCols+` FROM notifier.alert_rules WHERE tenant_id = $1 AND enabled ORDER BY id`, tenant.String())
}

// DeleteRule implements Repo.
func (p *PgRepo) DeleteRule(ctx context.Context, tenant, id core.UUID) error {
	tag, err := p.DB.Exec(ctx, `DELETE FROM notifier.alert_rules WHERE id = $1 AND tenant_id = $2`, id.String(), tenant.String())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

const channelCols = `id::text, tenant_id::text, kind, name, config::text, enabled`

func scanChannel(row pgx.Row) (Channel, error) {
	var c Channel
	var id, tenant, cfg string
	if err := row.Scan(&id, &tenant, &c.Kind, &c.Name, &cfg, &c.Enabled); err != nil {
		return Channel{}, pgErr(err)
	}
	c.ID, c.Tenant = core.MustParseUUID(id), core.MustParseUUID(tenant)
	_ = json.Unmarshal([]byte(cfg), &c.Config)
	return c, nil
}

// PutChannel implements Repo.
func (p *PgRepo) PutChannel(ctx context.Context, c Channel) (Channel, error) {
	cfg, _ := json.Marshal(c.Config)
	return scanChannel(p.DB.QueryRow(ctx, `INSERT INTO notifier.channels (id, tenant_id, kind, name, config, enabled) VALUES ($1, $2, $3, $4, $5::text::jsonb, $6)
ON CONFLICT (id) DO UPDATE SET kind = EXCLUDED.kind, name = EXCLUDED.name, config = EXCLUDED.config, enabled = EXCLUDED.enabled
WHERE notifier.channels.tenant_id = EXCLUDED.tenant_id RETURNING `+channelCols,
		c.ID.String(), c.Tenant.String(), c.Kind, c.Name, string(cfg), c.Enabled))
}

// ListChannels implements Repo.
func (p *PgRepo) ListChannels(ctx context.Context, tenant core.UUID) ([]Channel, error) {
	rows, err := p.DB.Query(ctx, `SELECT `+channelCols+` FROM notifier.channels WHERE tenant_id = $1 ORDER BY id`, tenant.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetChannel implements Repo.
func (p *PgRepo) GetChannel(ctx context.Context, id core.UUID) (Channel, error) {
	return scanChannel(p.DB.QueryRow(ctx, `SELECT `+channelCols+` FROM notifier.channels WHERE id = $1`, id.String()))
}

type pgTx struct{ tx pgx.Tx }

func (t pgTx) MarkProcessed(ctx context.Context, consumer, id string) (bool, error) {
	return bus.MarkProcessedIn(ctx, t.tx, "notifier.processed_events", consumer, id)
}

func (t pgTx) InsertAlert(ctx context.Context, a AlertEvent) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO notifier.alert_events (id, tenant_id, rule_id, trigger_event_id, dedupe_key, status, payload, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7::text::jsonb, $8) ON CONFLICT (rule_id, dedupe_key, trigger_event_id) DO NOTHING`,
		a.ID.String(), a.Tenant.String(), a.RuleID.String(), a.TriggerEventID.String(), a.DedupeKey, a.Status, string(a.Payload), a.CreatedAt)
	return err
}

func (t pgTx) InsertDelivery(ctx context.Context, d Delivery) error {
	_, err := t.tx.Exec(ctx, `INSERT INTO notifier.deliveries (id, alert_event_id, channel_id, status, attempts, next_attempt_at, target)
SELECT $1, $2, $3, $4, 0, $5, $6 WHERE EXISTS (SELECT 1 FROM notifier.alert_events WHERE id = $2)`,
		d.ID.String(), d.AlertEventID.String(), d.ChannelID.String(), d.Status, d.NextAttemptAt, d.Target)
	return err
}

// InTx implements Repo.
func (p *PgRepo) InTx(ctx context.Context, f func(context.Context, Tx) error) error {
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := f(ctx, pgTx{tx}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const alertCols = `id::text, tenant_id::text, rule_id::text, trigger_event_id::text, dedupe_key, status, payload::text, created_at`

func scanAlert(row pgx.Row) (AlertEvent, error) {
	var a AlertEvent
	var id, tenant, rule, trig, payload string
	if err := row.Scan(&id, &tenant, &rule, &trig, &a.DedupeKey, &a.Status, &payload, &a.CreatedAt); err != nil {
		return AlertEvent{}, pgErr(err)
	}
	a.ID, a.Tenant, a.RuleID, a.TriggerEventID = core.MustParseUUID(id), core.MustParseUUID(tenant), core.MustParseUUID(rule), core.MustParseUUID(trig)
	a.Payload = json.RawMessage(payload)
	return a, nil
}

// ListAlertEvents implements Repo.
func (p *PgRepo) ListAlertEvents(ctx context.Context, tenant, rule core.UUID, status string, after core.UUID, limit int) ([]AlertEvent, error) {
	rows, err := p.DB.Query(ctx, `SELECT `+alertCols+` FROM notifier.alert_events WHERE tenant_id = $1 AND ($2::uuid IS NULL OR rule_id = $2)
AND ($3 = '' OR status = $3) AND id > $4 ORDER BY id LIMIT $5`, tenant.String(), uuidOrNil(rule), status, after.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertEvent
	for rows.Next() {
		a, err := scanAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAlertEvent implements Repo.
func (p *PgRepo) GetAlertEvent(ctx context.Context, id core.UUID) (AlertEvent, error) {
	return scanAlert(p.DB.QueryRow(ctx, `SELECT `+alertCols+` FROM notifier.alert_events WHERE id = $1`, id.String()))
}

// SetAlertStatus implements Repo.
func (p *PgRepo) SetAlertStatus(ctx context.Context, id core.UUID, status string) error {
	_, err := p.DB.Exec(ctx, `UPDATE notifier.alert_events SET status = $2 WHERE id = $1`, id.String(), status)
	return err
}

const deliveryCols = `id::text, alert_event_id::text, channel_id::text, target, status, attempts, coalesce(next_attempt_at, 'epoch'), coalesce(last_status_code, 0), coalesce(last_error, ''), coalesce(sent_at, 'epoch')`

func (p *PgRepo) deliveries(ctx context.Context, q string, args ...any) ([]Delivery, error) {
	rows, err := p.DB.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		var id, alert, ch string
		if err := rows.Scan(&id, &alert, &ch, &d.Target, &d.Status, &d.Attempts, &d.NextAttemptAt, &d.LastStatusCode, &d.LastError, &d.SentAt); err != nil {
			return nil, err
		}
		d.ID, d.AlertEventID, d.ChannelID = core.MustParseUUID(id), core.MustParseUUID(alert), core.MustParseUUID(ch)
		if d.SentAt.Unix() == 0 {
			d.SentAt = time.Time{}
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DueDeliveries implements Repo.
func (p *PgRepo) DueDeliveries(ctx context.Context, now time.Time, limit int) ([]Delivery, error) {
	return p.deliveries(ctx, `SELECT `+deliveryCols+` FROM notifier.deliveries WHERE status = 'pending' AND next_attempt_at <= $1 ORDER BY next_attempt_at, id LIMIT $2`, now, limit)
}

// DeliveriesOf implements Repo.
func (p *PgRepo) DeliveriesOf(ctx context.Context, alert core.UUID) ([]Delivery, error) {
	return p.deliveries(ctx, `SELECT `+deliveryCols+` FROM notifier.deliveries WHERE alert_event_id = $1 ORDER BY id`, alert.String())
}

// UpdateDelivery implements Repo.
func (p *PgRepo) UpdateDelivery(ctx context.Context, d Delivery) error {
	var sent *time.Time
	if !d.SentAt.IsZero() {
		sent = &d.SentAt
	}
	_, err := p.DB.Exec(ctx, `UPDATE notifier.deliveries SET status = $2, attempts = $3, next_attempt_at = $4, last_status_code = nullif($5, 0), last_error = nullif($6, ''), sent_at = $7 WHERE id = $1`,
		d.ID.String(), d.Status, d.Attempts, d.NextAttemptAt, d.LastStatusCode, d.LastError, sent)
	return err
}

// Ping implements Repo.
func (p *PgRepo) Ping(ctx context.Context) error {
	_, err := p.DB.Exec(ctx, `SELECT 1`)
	return err
}
