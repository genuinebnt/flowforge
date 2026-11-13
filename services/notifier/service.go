package notifier

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// Consumer is the processed_events consumer name.
const Consumer = alerts.DefaultConsumer

// Topics the notifier subscribes to (spec 15.1, plus content.changes of the
// V2 content service).
var Topics = []string{convert.TopicGraphChanges, convert.TopicRunEvents, convert.TopicApprovalEvents, convert.TopicAuthEvents,
	convert.TopicContentChanges}

// Options configure the notifier service.
type Options struct {
	Repo    Repo
	Senders *Senders
	Bus     bus.Bus
	Clock   core.Clock
	Logger  *slog.Logger
	Metrics *platform.Metrics
	Render  alerts.RenderOptions
	// DeliveryInterval is the delivery worker's poll interval (default 500 ms).
	DeliveryInterval time.Duration
}

// Service holds the rule processors, consumes events and delivers.
type Service struct {
	o Options

	mu    sync.Mutex
	procs map[[2]core.UUID]*alerts.Processor
	rng   *lockedRng
}

type lockedRng struct {
	mu sync.Mutex
	r  core.Rng
}

func (l *lockedRng) Uint64() uint64 { l.mu.Lock(); defer l.mu.Unlock(); return l.r.Uint64() }

// NewService returns a notifier.
func NewService(o Options) *Service {
	if o.Clock == nil {
		o.Clock = &core.SystemClock{}
	}
	if o.Logger == nil {
		o.Logger = platform.DiscardLogger()
	}
	if o.Metrics == nil {
		o.Metrics = platform.NewMetrics(nil)
	}
	if o.DeliveryInterval <= 0 {
		o.DeliveryInterval = 500 * time.Millisecond
	}
	return &Service{o: o, procs: map[[2]core.UUID]*alerts.Processor{}, rng: &lockedRng{r: core.SystemRng{}}}
}

// Invalidate drops the processors of tenant (its rules changed). The next
// event rebuilds them from the current rules and the persisted throttle
// windows and digests (G-LIM-06), so a rule edit does not reopen windows.
func (s *Service) Invalidate(tenant core.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.procs {
		if k[0] == tenant {
			delete(s.procs, k)
		}
	}
}

// txKey carries the processing transaction to the processed store.
type txKey struct{}

type txProcessed struct{}

func (txProcessed) MarkProcessed(ctx context.Context, consumer, eventID string, _ time.Time) (bool, error) {
	tx, ok := ctx.Value(txKey{}).(Tx)
	if !ok {
		return false, errors.New("notifier: no transaction in context")
	}
	return tx.MarkProcessed(ctx, consumer, eventID)
}

func (s *Service) processor(ctx context.Context, tenant, program core.UUID) (*alerts.Processor, error) {
	k := [2]core.UUID{tenant, program}
	s.mu.Lock()
	p := s.procs[k]
	s.mu.Unlock()
	if p != nil {
		return p, nil
	}
	rules, err := s.o.Repo.EnabledRules(ctx, tenant)
	if err != nil {
		return nil, err
	}
	set := alerts.NewRuleSet(s.o.Render)
	for _, r := range rules {
		if !r.Program.IsZero() && r.Program != program {
			continue
		}
		compiled, diags := alerts.Compile(r.Source)
		if compiled == nil {
			s.o.Logger.WarnContext(ctx, "notifier.rule_invalid", "rule_id", convert.ID(core.PrefixAlertRule, r.ID), "diagnostics", len(diags))
			continue
		}
		_ = set.Add(convert.ID(core.PrefixAlertRule, r.ID), compiled)
	}
	p, err = alerts.NewProcessor(set, alerts.ProcessorConfig{Consumer: Consumer, Clock: s.o.Clock, Store: txProcessed{}})
	if err != nil {
		return nil, err
	}
	st, err := s.o.Repo.LoadAlertState(ctx, tenant, program)
	if err != nil {
		return nil, err
	}
	p.Restore(st)
	s.mu.Lock()
	s.procs[k] = p
	s.mu.Unlock()
	return p, nil
}

// EventFromEnvelope builds the typed rule input from an envelope (spec 11.7,
// 15.2): graph changes fill change and entity, run events run, approval
// events approval; scope.* and auth.* carry only the envelope fields.
func EventFromEnvelope(env *pb.EventEnvelope) *alerts.Event {
	ev := &alerts.Event{ID: env.GetEventId(), Type: env.GetType(), SchemaVersion: int64(env.GetSchemaVersion()),
		TenantID: env.GetTenantId(), ProgramID: env.GetProgramId(), OccurredAt: convert.Time(env.GetOccurredAt())}
	switch p := env.GetPayload().(type) {
	case *pb.EventEnvelope_GraphChange:
		c := p.GraphChange
		ev.Change = &alerts.Change{ID: c.GetId(), Seq: c.GetSeq(), Type: c.GetType(), EntityID: c.GetEntityId(), EdgeID: c.GetEdgeId(),
			RunID: c.GetRunId(), Patch: convert.StructToMap(c.GetPatch()), At: convert.Time(c.GetAt())}
		if e := c.GetEntity(); e != nil {
			ev.Entity = &alerts.Entity{ID: e.GetId(), AssetType: e.GetAssetType(), Key: e.GetKey(), Content: convert.StructToMap(e.GetContent()),
				ScopeStatus: e.GetScopeStatus(), Confidence: e.GetConfidence(), State: e.GetState(), CreatedAt: convert.Time(e.GetCreatedAt()),
				LastSeen: convert.Time(e.GetLastSeen()), FirstSeenRun: e.GetFirstSeenRun(), LastSeenRun: e.GetLastSeenRun()}
		}
	case *pb.EventEnvelope_RunEvent:
		r := p.RunEvent
		pl := convert.StructToMap(r.GetPayload())
		run := &alerts.Run{ID: r.GetRunId(), Seq: r.GetSeq(), Type: r.GetType(), Payload: pl}
		str := func(m map[string]any, k string) string { s, _ := m[k].(string); return s }
		run.Code, run.Message, run.Path, run.Resource = str(pl, "code"), str(pl, "message"), str(pl, "path"), str(pl, "resource")
		if e, ok := pl["error"].(map[string]any); ok && run.Code == "" {
			run.Code, run.Message = str(e, "code"), str(e, "message")
		}
		switch r.GetType() {
		case engine.EvRunSucceeded:
			run.Status = string(engine.StatusSucceeded)
		case engine.EvRunFailed:
			run.Status = string(engine.StatusFailed)
		case engine.EvRunCancelled:
			run.Status = string(engine.StatusCancelled)
		}
		ev.Run = run
	case *pb.EventEnvelope_Generic:
		// Content events (V2 §6) become a content_changed change whose patch
		// is the event payload without the observation (the graph's copy).
		if strings.HasPrefix(env.GetType(), "content.") {
			pl := convert.StructToMap(p.Generic)
			delete(pl, "observation")
			ev.Change = &alerts.Change{ID: env.GetEventId(), Type: "content_changed", Patch: pl, At: ev.OccurredAt}
			if rid, ok := pl["run_id"].(string); ok {
				ev.Change.RunID = rid
			}
			if eid, ok := pl["entity_id"].(string); ok {
				ev.Change.EntityID = eid
			}
			ev.Diff = DiffFromContent(pl)
		}
	case *pb.EventEnvelope_ApprovalEvent:
		a := p.ApprovalEvent
		ev.Approval = &alerts.Approval{ID: a.GetApprovalId(), RunID: a.GetRunId(), Role: a.GetRole(), Status: a.GetStatus(),
			Reason: a.GetReason(), ExpiresAt: convert.Time(a.GetExpiresAt())}
	}
	return ev
}

// DiffFromContent builds the alert `diff` section from a content event
// payload (contentstore.EventPayload as a generic map). Every text is
// redacted again with content.Redact before it can reach an alert (V2
// §12.11): the content service already redacts hunks, and the notifier
// does not rely on that alone.
func DiffFromContent(pl map[string]any) *alerts.Diff {
	str := func(k string) string { s, _ := pl[k].(string); return s }
	red := func(s string) string { b, _ := content.Redact([]byte(s)); return string(b) }
	d := &alerts.Diff{SnapshotID: str("snapshot_id"), FromSnapshotID: str("snapshot_from"), DiffID: str("diff_id"), URL: str("url"),
		Profile: str("profile"), Outcome: str("outcome"), Class: str("class"), Newness: str("newness"), Cause: str("cause"),
		Excerpt: red(str("excerpt"))}
	d.Baseline, _ = pl["baseline"].(bool)
	d.Alertable, _ = pl["alertable"].(bool)
	d.Score, _ = pl["score"].(float64)
	if ls, ok := pl["levels"].([]any); ok {
		for _, l := range ls {
			if s, ok := l.(string); ok {
				d.Levels = append(d.Levels, s)
			}
		}
	}
	if cs, ok := pl["changes"].([]any); ok {
		for _, c := range cs {
			m, _ := c.(map[string]any)
			op, _ := m["op"].(string)
			kind, _ := m["kind"].(string)
			key, _ := m["key"].(string)
			if op != "" && kind != "" {
				d.Changes = append(d.Changes, red(op+" "+kind+"|"+key))
			}
		}
	}
	if fs, ok := pl["score_features"].([]any); ok {
		for _, f := range fs {
			m, _ := f.(map[string]any)
			n, _ := m["name"].(string)
			w, _ := m["weight"].(float64)
			d.ScoreFeatures = append(d.ScoreFeatures, alerts.ScoreFeature{Name: n, Weight: w})
		}
	}
	return d
}

// contentHidden reports a content event that default alerting hides
// (V2 §12.2, §12.3, §12.13: baselines, non-target causes, pre-existing
// content and non-changes carry alertable=false), and the graph's copy of
// content changes.
func contentHidden(env *pb.EventEnvelope) bool {
	// The graph's own content_changed events (graph.changes) restate the
	// content event without its classification; alerts come from
	// content.changes only, so one change alerts once.
	if env.GetType() == "graph.content_changed" {
		return true
	}
	if !strings.HasPrefix(env.GetType(), "content.") {
		return false
	}
	g, ok := env.GetPayload().(*pb.EventEnvelope_Generic)
	if !ok {
		return true
	}
	a, _ := convert.StructToMap(g.Generic)["alertable"].(bool)
	return !a
}

// HandleEvent is the bus handler: it evaluates the tenant's rules and, in
// one transaction, records the event in processed_events and stores the
// alert events and their deliveries (spec 15.1). Redeliveries are no-ops.
func (s *Service) HandleEvent(ctx context.Context, env *pb.EventEnvelope) error {
	tenant, err := convert.EnvelopeTenant(env)
	if err != nil || env.GetEventId() == "" {
		s.o.Logger.WarnContext(ctx, "notifier.bad_envelope", "event_id", env.GetEventId())
		return nil
	}
	if contentHidden(env) {
		return nil
	}
	ev := EventFromEnvelope(env)
	program := convert.EnvelopeProgram(env)
	p, err := s.processor(ctx, tenant, program)
	if err != nil {
		return err
	}
	trigger, err := convert.EventUUID(env.GetEventId())
	if err != nil {
		return nil
	}
	err = s.o.Repo.InTx(ctx, func(ctx context.Context, tx Tx) error {
		out, err := p.Process(context.WithValue(ctx, txKey{}, tx), ev)
		if err != nil || out.Duplicate {
			return err
		}
		if !out.State.Empty() {
			if err := tx.SaveAlertState(ctx, tenant, program, out.State); err != nil {
				return err
			}
		}
		ids := map[[2]string]core.UUID{}
		for _, a := range out.Alerts {
			rule, err := core.ParsePublicID(core.PrefixAlertRule, a.RuleID)
			if err != nil {
				continue
			}
			id := s.newID()
			body, _ := json.Marshal(alerts.AlertBody{AlertID: convert.ID(core.PrefixAlertEvent, id), Payload: a.Payload})
			if err := tx.InsertAlert(ctx, AlertEvent{ID: id, Tenant: tenant, RuleID: rule, TriggerEventID: trigger, DedupeKey: a.DedupeKey,
				Status: string(a.Status), Payload: body, CreatedAt: a.CreatedAt}); err != nil {
				return err
			}
			s.o.Metrics.AlertsTotal.Inc(string(a.Status))
			if a.Status == alerts.StatusPending {
				ids[[2]string{a.RuleID, a.DedupeKey}] = id
			}
		}
		for _, n := range out.Notifications {
			if n.Payload == nil {
				continue
			}
			id, ok := ids[[2]string{n.RuleID, n.DedupeKey}]
			if !ok {
				continue
			}
			if err := s.deliveries(ctx, tx, tenant, id, n); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		// The processor's memory already holds this event's windows but the
		// transaction did not commit: drop it so the redelivery starts from
		// the persisted state instead of being throttled by itself.
		s.drop(tenant, program, p)
	}
	return err
}

// drop forgets the cached processor of (tenant, program) if it is still p.
func (s *Service) drop(tenant, program core.UUID, p *alerts.Processor) {
	k := [2]core.UUID{tenant, program}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.procs[k] == p {
		delete(s.procs, k)
	}
}

func (s *Service) newID() core.UUID { return core.NewUUIDv7(s.o.Clock, s.rng) }

// deliveries creates one delivery per action, to the tenant's matching
// enabled channel: webhook(name) by name, slack(...) and email(...) to the
// first enabled channel of that kind. Actions without a channel are logged.
func (s *Service) deliveries(ctx context.Context, tx Tx, tenant, alert core.UUID, n alerts.Notification) error {
	chans, err := s.o.Repo.ListChannels(ctx, tenant)
	if err != nil {
		return err
	}
	for _, a := range n.Actions {
		var ch *Channel
		for i := range chans {
			c := &chans[i]
			if !c.Enabled || c.Kind != string(a.Kind) {
				continue
			}
			if a.Kind == alerts.ActionWebhook && c.Name != a.Target {
				continue
			}
			ch = c
			break
		}
		if ch == nil {
			s.o.Logger.WarnContext(ctx, "notifier.no_channel", "kind", string(a.Kind), "target", a.Target, "rule_id", n.RuleID)
			continue
		}
		target := a.Target
		if a.Kind == alerts.ActionWebhook {
			target = ""
		}
		if err := tx.InsertDelivery(ctx, Delivery{ID: s.newID(), AlertEventID: alert, ChannelID: ch.ID, Target: target,
			Status: DeliveryPending, NextAttemptAt: n.DeliverAt}); err != nil {
			return err
		}
	}
	return nil
}

// Run subscribes to the event topics and runs the delivery worker and the
// digest flusher until ctx is done.
func (s *Service) Run(ctx context.Context) error {
	if s.o.Bus != nil {
		for _, t := range Topics {
			if err := s.o.Bus.Subscribe(ctx, t, Consumer, s.HandleEvent); err != nil {
				return err
			}
		}
	} else {
		s.o.Logger.WarnContext(ctx, "notifier.no_bus", "detail", "no event bus: no events are consumed")
	}
	t := time.NewTicker(s.o.DeliveryInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		s.FlushDigests(ctx)
		if _, err := s.DeliverDue(ctx); err != nil && ctx.Err() == nil {
			s.o.Logger.WarnContext(ctx, "notifier.delivery_loop", "error", err.Error())
		}
	}
}

// FlushDigests delivers every digest whose window has ended: one alert
// event and its deliveries, and the persisted digest deleted, in one
// transaction. Run calls it on every delivery tick.
func (s *Service) FlushDigests(ctx context.Context) {
	s.mu.Lock()
	procs := make(map[[2]core.UUID]*alerts.Processor, len(s.procs))
	for k, v := range s.procs {
		procs[k] = v
	}
	s.mu.Unlock()
	for k, p := range procs {
		for _, n := range p.Flush() {
			if n.Digest == nil {
				continue
			}
			rule, err := core.ParsePublicID(core.PrefixAlertRule, n.RuleID)
			if err != nil {
				continue
			}
			id := s.newID()
			body, _ := json.Marshal(map[string]any{"alert_id": convert.ID(core.PrefixAlertEvent, id), "digest": n.Digest})
			err = s.o.Repo.InTx(ctx, func(ctx context.Context, tx Tx) error {
				if err := tx.InsertAlert(ctx, AlertEvent{ID: id, Tenant: k[0], RuleID: rule, TriggerEventID: id,
					DedupeKey: "digest:" + n.Digest.WindowEnd.Format(time.RFC3339), Status: string(alerts.StatusPending), Payload: body,
					CreatedAt: s.o.Clock.Now().UTC()}); err != nil {
					return err
				}
				if err := tx.CloseDigest(ctx, k[0], k[1], n.RuleID); err != nil {
					return err
				}
				return s.deliveries(ctx, tx, k[0], id, n)
			})
			if err != nil {
				// The digest is still persisted: reloading the processor
				// brings it back, due, for the next flush.
				s.drop(k[0], k[1], p)
				s.o.Logger.ErrorContext(ctx, "notifier.digest_failed", "rule_id", n.RuleID, "error", err.Error())
			}
		}
	}
}

// DeliverDue sends up to 50 due deliveries once each (the earliest due
// first; Run calls it every DeliveryInterval) and returns how many it
// tried.
// Failures are rescheduled with the spec 15.1 backoff (alerts.NextAttempt)
// and marked failed after 24 h; one channel's failure never blocks another.
func (s *Service) DeliverDue(ctx context.Context) (int, error) {
	now := s.o.Clock.Now().UTC()
	due, err := s.o.Repo.DueDeliveries(ctx, now, 50)
	if err != nil {
		return 0, err
	}
	for _, d := range due {
		s.deliver(ctx, d, now)
	}
	return len(due), nil
}

func (s *Service) deliver(ctx context.Context, d Delivery, now time.Time) {
	alert, err := s.o.Repo.GetAlertEvent(ctx, d.AlertEventID)
	if err != nil {
		return
	}
	ch, err := s.o.Repo.GetChannel(ctx, d.ChannelID)
	if err != nil {
		return
	}
	var body struct {
		alerts.Payload
		AlertID string `json:"alert_id"`
	}
	_ = json.Unmarshal(alert.Payload, &body)
	m := Message{DeliveryID: convert.ID(core.PrefixDelivery, d.ID), AlertID: convert.ID(core.PrefixAlertEvent, alert.ID),
		Severity: body.Severity.String(), Summary: body.Summary, Body: alert.Payload, At: now}
	if body.Summary == "" {
		m.Summary = "Flowforge digest"
	}
	sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	code, err := s.o.Senders.Send(sctx, ch, d.Target, m)
	cancel()
	d.Attempts++
	d.LastStatusCode = code
	if err == nil {
		d.Status, d.SentAt, d.LastError = DeliverySent, now, ""
		s.o.Metrics.DeliveriesTotal.Inc(ch.Kind, "sent")
	} else {
		d.LastError = truncate(err.Error(), 500)
		next, ok := alerts.NextAttempt(d.Attempts, alert.CreatedAt, now, s.rng)
		if ok {
			d.NextAttemptAt = next
			s.o.Metrics.DeliveriesTotal.Inc(ch.Kind, "retry")
		} else {
			d.Status = DeliveryFailed
			s.o.Metrics.DeliveriesTotal.Inc(ch.Kind, "failed")
		}
		s.o.Logger.WarnContext(ctx, "notifier.delivery_failed", "delivery_id", m.DeliveryID, "kind", ch.Kind, "attempt", d.Attempts, "error", d.LastError)
	}
	if err := s.o.Repo.UpdateDelivery(ctx, d); err != nil {
		return
	}
	all, err := s.o.Repo.DeliveriesOf(ctx, alert.ID)
	if err != nil {
		return
	}
	sent, pending := 0, 0
	for _, x := range all {
		switch x.Status {
		case DeliverySent:
			sent++
		case DeliveryPending:
			pending++
		}
	}
	if pending == 0 {
		status := string(alerts.StatusFailed)
		if sent > 0 {
			status = string(alerts.StatusSent)
		}
		_ = s.o.Repo.SetAlertStatus(ctx, alert.ID, status)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
