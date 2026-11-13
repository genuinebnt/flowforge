package scope

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/convert"
)

// MemRepo is the in-memory Repo (dev stack and tests).
type MemRepo struct {
	mu        sync.Mutex
	programs  map[core.UUID]Program
	versions  map[core.UUID][]Version
	kills     map[[2]string]KillSwitch
	decisions map[core.UUID]DecisionRecord
	outbox    *bus.MemOutbox
}

// NewMemRepo returns an empty repository.
func NewMemRepo() *MemRepo {
	return &MemRepo{programs: map[core.UUID]Program{}, versions: map[core.UUID][]Version{},
		kills: map[[2]string]KillSwitch{}, decisions: map[core.UUID]DecisionRecord{}, outbox: bus.NewMemOutbox()}
}

var _ Repo = (*MemRepo)(nil)

func (r *MemRepo) emit(evs []Event) {
	for _, e := range evs {
		r.outbox.Append(bus.OutboxRow{EventID: e.ID.String(), Topic: convert.TopicScopeEvents, OrderingKey: e.Program.String(),
			Payload: convert.GenericOutbox(e.ID, e.Type, e.Tenant, e.Program, e.At, e.Payload)})
	}
}

// CreateProgram implements Repo.
func (r *MemRepo) CreateProgram(_ context.Context, p Program, initial *Version, evs []Event) (Program, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range r.programs {
		if x.Tenant == p.Tenant && x.Name == p.Name {
			return Program{}, ErrExists
		}
	}
	if initial != nil {
		initial.Program, initial.Version = p.ID, 1
		p.CurrentVersion = 1
		r.versions[p.ID] = []Version{*initial}
	}
	r.programs[p.ID] = p
	r.emit(evs)
	return p, nil
}

func (r *MemRepo) program(tenant, id core.UUID) (Program, error) {
	p, ok := r.programs[id]
	if !ok || p.Tenant != tenant {
		return Program{}, ErrNotFound
	}
	return p, nil
}

// GetProgram implements Repo.
func (r *MemRepo) GetProgram(_ context.Context, tenant, id core.UUID) (Program, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.program(tenant, id)
}

// ListPrograms implements Repo.
func (r *MemRepo) ListPrograms(_ context.Context, tenant, after core.UUID, limit int) ([]Program, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Program
	for _, p := range r.programs {
		if p.Tenant == tenant && cmp.Compare(p.ID.String(), after.String()) > 0 {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Program) int { return cmp.Compare(a.ID.String(), b.ID.String()) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// PutVersion implements Repo.
func (r *MemRepo) PutVersion(_ context.Context, tenant core.UUID, base int32, v Version, evs []Event) (Version, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.program(tenant, v.Program)
	if err != nil {
		return Version{}, err
	}
	if p.CurrentVersion != base {
		return Version{}, ErrConflict
	}
	v.Version = base + 1
	r.versions[p.ID] = append(r.versions[p.ID], v)
	p.CurrentVersion = v.Version
	r.programs[p.ID] = p
	r.emit(evs)
	return v, nil
}

// GetVersion implements Repo.
func (r *MemRepo) GetVersion(_ context.Context, tenant, program core.UUID, n int32) (Version, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p, err := r.program(tenant, program)
	if err != nil {
		return Version{}, err
	}
	if n == 0 {
		n = p.CurrentVersion
	}
	vs := r.versions[program]
	if n < 1 || int(n) > len(vs) {
		return Version{}, ErrNotFound
	}
	return vs[n-1], nil
}

// EffectiveVersion implements Repo.
func (r *MemRepo) EffectiveVersion(_ context.Context, tenant, program core.UUID, at time.Time) (int32, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.program(tenant, program); err != nil {
		return 0, err
	}
	var best int32
	for _, v := range r.versions[program] {
		if !v.EffectiveFrom.After(at) {
			best = max(best, v.Version)
		}
	}
	return best, nil
}

// DueVersions implements Repo.
func (r *MemRepo) DueVersions(_ context.Context, after, upTo time.Time, limit int) ([]DueVersion, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []DueVersion
	for pid, vs := range r.versions {
		for _, v := range vs {
			if v.EffectiveFrom.After(v.CreatedAt) && v.EffectiveFrom.After(after) && !v.EffectiveFrom.After(upTo) {
				out = append(out, DueVersion{Tenant: r.programs[pid].Tenant, Program: pid, Version: v.Version, EffectiveFrom: v.EffectiveFrom})
			}
		}
	}
	// PORT: map iteration order is random in Go; sort so activation events
	// are emitted in effective_from order, as the Postgres query returns them.
	slices.SortFunc(out, func(a, b DueVersion) int {
		return cmp.Or(a.EffectiveFrom.Compare(b.EffectiveFrom), cmp.Compare(a.Program.String(), b.Program.String()), cmp.Compare(a.Version, b.Version))
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// AppendEvents implements Repo.
func (r *MemRepo) AppendEvents(_ context.Context, evs []Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.emit(evs)
	return nil
}

// ListVersions implements Repo.
func (r *MemRepo) ListVersions(_ context.Context, tenant, program core.UUID, after int32, limit int) ([]Version, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.program(tenant, program); err != nil {
		return nil, err
	}
	var out []Version
	for _, v := range r.versions[program] {
		if v.Version > after && len(out) < limit {
			out = append(out, v)
		}
	}
	return out, nil
}

// SetKillSwitch implements Repo.
func (r *MemRepo) SetKillSwitch(_ context.Context, ks KillSwitch, evs []Event) (KillSwitch, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kills[[2]string{ks.Level, ks.Target.String()}] = ks
	r.emit(evs)
	return ks, nil
}

// KillSwitches implements Repo.
func (r *MemRepo) KillSwitches(_ context.Context, tenant core.UUID) ([]KillSwitch, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []KillSwitch
	for _, k := range r.kills {
		switch k.Level {
		case LevelGlobal:
		case LevelTenant:
			if k.Target != tenant {
				continue
			}
		case LevelProgram:
			if p, ok := r.programs[k.Target]; !ok || p.Tenant != tenant {
				continue
			}
		}
		out = append(out, k)
	}
	slices.SortFunc(out, func(a, b KillSwitch) int {
		return cmp.Or(cmp.Compare(a.Level, b.Level), cmp.Compare(a.Target.String(), b.Target.String()))
	})
	return out, nil
}

// RecordDecisions implements Repo.
func (r *MemRepo) RecordDecisions(_ context.Context, ds []DecisionRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range ds {
		r.decisions[d.ID] = d
	}
	return nil
}

// GetDecision implements Repo.
func (r *MemRepo) GetDecision(_ context.Context, tenant, id core.UUID) (DecisionRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.decisions[id]
	if !ok || d.Tenant != tenant {
		return DecisionRecord{}, ErrNotFound
	}
	return d, nil
}

// Decisions returns every recorded decision of tenant (tests).
func (r *MemRepo) Decisions(tenant core.UUID) []DecisionRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []DecisionRecord
	for _, d := range r.decisions {
		if d.Tenant == tenant {
			out = append(out, d)
		}
	}
	slices.SortFunc(out, func(a, b DecisionRecord) int { return cmp.Compare(a.ID.String(), b.ID.String()) })
	return out
}

// Outbox implements Repo.
func (r *MemRepo) Outbox() bus.OutboxSource { return r.outbox }

// Ping implements Repo.
func (r *MemRepo) Ping(context.Context) error { return nil }
