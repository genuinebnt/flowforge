// Package notifier is the notifier service (spec 3.1, 15): the
// NotifierService admin API (rules compiled with package alerts; a rule
// with errors cannot be enabled), the consumer of graph.changes,
// run.events, approval.events and auth.events (processed_events inbox in
// the same transaction as alert creation), and the delivery worker with
// the spec 15.1 retry schedule over webhook (HMAC-signed, SSRF-checked),
// Slack and e-mail channels. Secret references resolve through the
// Secrets port.
package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/core"
)

// Rule is an alert rule (notifier.alert_rules).
type Rule struct {
	ID        core.UUID
	Tenant    core.UUID
	Program   core.UUID // zero: every program of the tenant
	Name      string
	Source    string
	Compiled  json.RawMessage
	Enabled   bool
	Version   int32
	CreatedBy string
	CreatedAt time.Time
}

// Channel is a delivery channel (notifier.channels). Config holds secret
// references only, never secret values.
type Channel struct {
	ID      core.UUID
	Tenant  core.UUID
	Kind    string // slack, email, webhook
	Name    string
	Config  map[string]any
	Enabled bool
}

// Channel kinds.
const (
	KindSlack   = "slack"
	KindEmail   = "email"
	KindWebhook = "webhook"
)

// AlertEvent is one alert (notifier.alert_events).
type AlertEvent struct {
	ID             core.UUID
	Tenant         core.UUID
	RuleID         core.UUID
	TriggerEventID core.UUID
	DedupeKey      string
	Status         string // pending, sent, failed, suppressed
	Payload        json.RawMessage
	CreatedAt      time.Time
}

// Delivery is one send of an alert to one channel (notifier.deliveries).
type Delivery struct {
	ID             core.UUID
	AlertEventID   core.UUID
	ChannelID      core.UUID
	Target         string // Slack channel override or e-mail recipient
	Status         string // pending, sent, failed
	Attempts       int
	NextAttemptAt  time.Time
	LastStatusCode int
	LastError      string
	SentAt         time.Time
}

// Delivery statuses.
const (
	DeliveryPending = "pending"
	DeliverySent    = "sent"
	DeliveryFailed  = "failed"
)

// Errors of the repositories.
var (
	ErrNotFound = errors.New("notifier: not found")
	ErrExists   = errors.New("notifier: already exists")
)

// Tx is the transaction an event is processed in: the inbox mark, the
// alert events and their deliveries commit together (spec 15.1).
type Tx interface {
	MarkProcessed(ctx context.Context, consumer, eventID string) (bool, error)
	InsertAlert(ctx context.Context, a AlertEvent) error
	InsertDelivery(ctx context.Context, d Delivery) error
	// SaveAlertState upserts the throttle windows and open digests one
	// event changed (alerts.Outcome.State) for the processor of (tenant,
	// program), so they survive a restart (G-LIM-06).
	SaveAlertState(ctx context.Context, tenant, program core.UUID, c alerts.StateChange) error
	// CloseDigest deletes a flushed digest of rule (a public alr_ id).
	CloseDigest(ctx context.Context, tenant, program core.UUID, rule string) error
}

// Repo is the notifier's storage. Tenant-scoped methods never return
// another tenant's rows; the delivery worker methods span tenants.
type Repo interface {
	PutRule(ctx context.Context, r Rule, create bool) (Rule, error)
	GetRule(ctx context.Context, tenant, id core.UUID) (Rule, error)
	ListRules(ctx context.Context, tenant, after core.UUID, limit int) ([]Rule, error)
	DeleteRule(ctx context.Context, tenant, id core.UUID) error
	EnabledRules(ctx context.Context, tenant core.UUID) ([]Rule, error)

	PutChannel(ctx context.Context, c Channel) (Channel, error)
	ListChannels(ctx context.Context, tenant core.UUID) ([]Channel, error)
	GetChannel(ctx context.Context, id core.UUID) (Channel, error)

	InTx(ctx context.Context, f func(ctx context.Context, tx Tx) error) error
	ListAlertEvents(ctx context.Context, tenant, rule core.UUID, status string, after core.UUID, limit int) ([]AlertEvent, error)
	GetAlertEvent(ctx context.Context, id core.UUID) (AlertEvent, error)
	SetAlertStatus(ctx context.Context, id core.UUID, status string) error

	DueDeliveries(ctx context.Context, now time.Time, limit int) ([]Delivery, error)
	DeliveriesOf(ctx context.Context, alert core.UUID) ([]Delivery, error)
	UpdateDelivery(ctx context.Context, d Delivery) error
	Ping(ctx context.Context) error

	// LoadAlertState returns the persisted throttle windows and digests of
	// the processor of (tenant, program); a new processor restores them.
	LoadAlertState(ctx context.Context, tenant, program core.UUID) (alerts.ProcessorState, error)
	// PruneAlertState deletes throttle windows that ended before now, at
	// most limit (maintenance; expired windows are also skipped on load).
	PruneAlertState(ctx context.Context, now time.Time, limit int) (int64, error)
}
