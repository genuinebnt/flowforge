package notifier

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/core"
)

// MemRepo is the in-memory Repo (dev stack and tests).
type MemRepo struct {
	mu         sync.Mutex
	txMu       sync.Mutex
	rules      map[core.UUID]Rule
	channels   map[core.UUID]Channel
	alerts     map[core.UUID]AlertEvent
	deliveries map[core.UUID]Delivery
	processed  map[[2]string]bool
	throttle   map[[2]core.UUID]map[string]alerts.ThrottleWindow
	digests    map[[2]core.UUID]map[string]alerts.Digest
}

// NewMemRepo returns an empty repository.
func NewMemRepo() *MemRepo {
	return &MemRepo{rules: map[core.UUID]Rule{}, channels: map[core.UUID]Channel{}, alerts: map[core.UUID]AlertEvent{},
		deliveries: map[core.UUID]Delivery{}, processed: map[[2]string]bool{},
		throttle: map[[2]core.UUID]map[string]alerts.ThrottleWindow{}, digests: map[[2]core.UUID]map[string]alerts.Digest{}}
}

var _ Repo = (*MemRepo)(nil)

func byID[T any](m map[core.UUID]T, keep func(T) bool, after core.UUID, limit int) []T {
	ids := make([]core.UUID, 0, len(m))
	for id, v := range m {
		if keep(v) && cmp.Compare(id.String(), after.String()) > 0 {
			ids = append(ids, id)
		}
	}
	slices.SortFunc(ids, func(a, b core.UUID) int { return cmp.Compare(a.String(), b.String()) })
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	out := make([]T, 0, len(ids))
	for _, id := range ids {
		out = append(out, m[id])
	}
	return out
}

// PutRule implements Repo.
func (r *MemRepo) PutRule(_ context.Context, rule Rule, create bool) (Rule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cur, exists := r.rules[rule.ID]
	if create && exists || !create && (!exists || cur.Tenant != rule.Tenant) {
		if create {
			return Rule{}, ErrExists
		}
		return Rule{}, ErrNotFound
	}
	for id, x := range r.rules {
		if id != rule.ID && x.Tenant == rule.Tenant && x.Name == rule.Name {
			return Rule{}, ErrExists
		}
	}
	if exists {
		rule.Version, rule.CreatedAt, rule.CreatedBy = cur.Version+1, cur.CreatedAt, cur.CreatedBy
	} else {
		rule.Version = 1
	}
	r.rules[rule.ID] = rule
	return rule, nil
}

// GetRule implements Repo.
func (r *MemRepo) GetRule(_ context.Context, tenant, id core.UUID) (Rule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	x, ok := r.rules[id]
	if !ok || x.Tenant != tenant {
		return Rule{}, ErrNotFound
	}
	return x, nil
}

// ListRules implements Repo.
func (r *MemRepo) ListRules(_ context.Context, tenant, after core.UUID, limit int) ([]Rule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return byID(r.rules, func(x Rule) bool { return x.Tenant == tenant }, after, limit), nil
}

// DeleteRule implements Repo.
func (r *MemRepo) DeleteRule(_ context.Context, tenant, id core.UUID) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	x, ok := r.rules[id]
	if !ok || x.Tenant != tenant {
		return ErrNotFound
	}
	delete(r.rules, id)
	for aid, a := range r.alerts {
		if a.RuleID == id {
			delete(r.alerts, aid)
			for did, d := range r.deliveries {
				if d.AlertEventID == aid {
					delete(r.deliveries, did)
				}
			}
		}
	}
	return nil
}

// EnabledRules implements Repo.
func (r *MemRepo) EnabledRules(_ context.Context, tenant core.UUID) ([]Rule, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return byID(r.rules, func(x Rule) bool { return x.Tenant == tenant && x.Enabled }, core.Nil, 0), nil
}

// PutChannel implements Repo (upsert by id; names are unique per tenant).
func (r *MemRepo) PutChannel(_ context.Context, c Channel) (Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.channels[c.ID]; ok && cur.Tenant != c.Tenant {
		return Channel{}, ErrNotFound
	}
	for id, x := range r.channels {
		if id != c.ID && x.Tenant == c.Tenant && x.Name == c.Name {
			return Channel{}, ErrExists
		}
	}
	r.channels[c.ID] = c
	return c, nil
}

// ListChannels implements Repo.
func (r *MemRepo) ListChannels(_ context.Context, tenant core.UUID) ([]Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return byID(r.channels, func(x Channel) bool { return x.Tenant == tenant }, core.Nil, 0), nil
}

// GetChannel implements Repo.
func (r *MemRepo) GetChannel(_ context.Context, id core.UUID) (Channel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.channels[id]
	if !ok {
		return Channel{}, ErrNotFound
	}
	return c, nil
}

type memTx struct {
	r          *MemRepo
	marks      [][2]string
	alerts     []AlertEvent
	deliveries []Delivery
	state      []func() // applied with the other writes
}

func (t *memTx) SaveAlertState(_ context.Context, tenant, program core.UUID, c alerts.StateChange) error {
	k := [2]core.UUID{tenant, program}
	t.state = append(t.state, func() {
		if t.r.throttle[k] == nil {
			t.r.throttle[k] = map[string]alerts.ThrottleWindow{}
		}
		if t.r.digests[k] == nil {
			t.r.digests[k] = map[string]alerts.Digest{}
		}
		for _, w := range c.Throttle {
			t.r.throttle[k][w.Key] = w
		}
		for _, d := range c.Digests {
			d.Items = slices.Clone(d.Items)
			d.Actions = nil
			t.r.digests[k][d.RuleID] = d
		}
	})
	return nil
}

func (t *memTx) CloseDigest(_ context.Context, tenant, program core.UUID, rule string) error {
	t.state = append(t.state, func() { delete(t.r.digests[[2]core.UUID{tenant, program}], rule) })
	return nil
}

// LoadAlertState implements Repo.
func (r *MemRepo) LoadAlertState(_ context.Context, tenant, program core.UUID) (alerts.ProcessorState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := [2]core.UUID{tenant, program}
	var s alerts.ProcessorState
	for _, w := range r.throttle[k] {
		s.Throttle = append(s.Throttle, w)
	}
	for _, d := range r.digests[k] {
		d.Items = slices.Clone(d.Items)
		s.Digests = append(s.Digests, d)
	}
	return s, nil
}

// PruneAlertState implements Repo.
func (r *MemRepo) PruneAlertState(_ context.Context, now time.Time, limit int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int64
	for _, m := range r.throttle {
		for key, w := range m {
			if n >= int64(limit) {
				return n, nil
			}
			if !now.Before(w.Until) {
				delete(m, key)
				n++
			}
		}
	}
	return n, nil
}

func (t *memTx) MarkProcessed(_ context.Context, consumer, id string) (bool, error) {
	k := [2]string{consumer, id}
	t.r.mu.Lock()
	seen := t.r.processed[k]
	t.r.mu.Unlock()
	if seen || slices.Contains(t.marks, k) {
		return false, nil
	}
	t.marks = append(t.marks, k)
	return true, nil
}

func (t *memTx) InsertAlert(_ context.Context, a AlertEvent) error {
	t.alerts = append(t.alerts, a)
	return nil
}

func (t *memTx) InsertDelivery(_ context.Context, d Delivery) error {
	t.deliveries = append(t.deliveries, d)
	return nil
}

// InTx implements Repo: writes are buffered and applied only when f
// succeeds; transactions are serialized.
func (r *MemRepo) InTx(ctx context.Context, f func(context.Context, Tx) error) error {
	r.txMu.Lock()
	defer r.txMu.Unlock()
	tx := &memTx{r: r}
	if err := f(ctx, tx); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, k := range tx.marks {
		r.processed[k] = true
	}
	for _, a := range tx.alerts {
		r.alerts[a.ID] = a
	}
	for _, d := range tx.deliveries {
		r.deliveries[d.ID] = d
	}
	for _, f := range tx.state {
		f()
	}
	return nil
}

// ListAlertEvents implements Repo.
func (r *MemRepo) ListAlertEvents(_ context.Context, tenant, rule core.UUID, status string, after core.UUID, limit int) ([]AlertEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return byID(r.alerts, func(a AlertEvent) bool {
		return a.Tenant == tenant && (rule.IsZero() || a.RuleID == rule) && (status == "" || a.Status == status)
	}, after, limit), nil
}

// GetAlertEvent implements Repo.
func (r *MemRepo) GetAlertEvent(_ context.Context, id core.UUID) (AlertEvent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.alerts[id]
	if !ok {
		return AlertEvent{}, ErrNotFound
	}
	return a, nil
}

// SetAlertStatus implements Repo.
func (r *MemRepo) SetAlertStatus(_ context.Context, id core.UUID, status string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	a, ok := r.alerts[id]
	if !ok {
		return ErrNotFound
	}
	a.Status = status
	r.alerts[id] = a
	return nil
}

// DueDeliveries implements Repo.
func (r *MemRepo) DueDeliveries(_ context.Context, now time.Time, limit int) ([]Delivery, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return byID(r.deliveries, func(d Delivery) bool { return d.Status == DeliveryPending && !d.NextAttemptAt.After(now) }, core.Nil, limit), nil
}

// DeliveriesOf implements Repo.
func (r *MemRepo) DeliveriesOf(_ context.Context, alert core.UUID) ([]Delivery, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return byID(r.deliveries, func(d Delivery) bool { return d.AlertEventID == alert }, core.Nil, 0), nil
}

// UpdateDelivery implements Repo.
func (r *MemRepo) UpdateDelivery(_ context.Context, d Delivery) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.deliveries[d.ID]; !ok {
		return ErrNotFound
	}
	r.deliveries[d.ID] = d
	return nil
}

// Ping implements Repo.
func (r *MemRepo) Ping(context.Context) error { return nil }
