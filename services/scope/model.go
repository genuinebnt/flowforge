// Package scope is the scope service (spec 3.1, 4, 11.3): programs, scope
// versions with optimistic concurrency, kill switches and the decision log,
// in memory or over the scope schema of Postgres. Check and CheckBatch
// compile and cache each (program, version) policy, check kill switches
// first, resolve and pin addresses for active checks, record every
// decision and fail closed. State changes publish scope.events through the
// outbox. Client adapts the gRPC service to engine.Scope and to the
// target checks of the tool runner, egress proxy and similarity service.
package scope

import (
	"context"
	"errors"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/internal/bus"
)

// Program is a bug-bounty or engagement program (scope.programs).
type Program struct {
	ID             core.UUID
	Tenant         core.UUID
	Name           string
	Platform       string
	Status         string
	CurrentVersion int32
	CreatedAt      time.Time
}

// Version is one immutable scope document version (scope.scope_versions).
type Version struct {
	Program       core.UUID
	Version       int32
	Document      scope.Document
	EffectiveFrom time.Time
	CreatedBy     string
	CreatedAt     time.Time
}

// Kill switch levels (spec 11.3).
const (
	LevelGlobal  = "global"
	LevelTenant  = "tenant"
	LevelProgram = "program"
)

// KillSwitch is one switch (scope.kill_switches). Target is the tenant or
// program id, zero for the global switch.
type KillSwitch struct {
	Level   string
	Target  core.UUID
	Enabled bool
	Reason  string
	SetBy   string
	SetAt   time.Time
}

// DecisionRecord is one row of the decision log (scope.scope_decisions).
type DecisionRecord struct {
	ID           core.UUID
	Tenant       core.UUID
	Program      core.UUID
	ScopeVersion int32
	RunID        core.UUID
	EffectPath   string
	Class        core.ActivityClass
	Decision     scope.Decision
}

// Event is a scope.events outbox message.
type Event struct {
	ID      core.UUID
	Type    string
	Tenant  core.UUID
	Program core.UUID
	At      time.Time
	Payload map[string]any
}

// Event types published on scope.events.
//
// scope.version_published means "this version is now in effect": it is
// written by PutScope when effective_from is not in the future, and by
// Server.ActivateDue once a future-dated version's effective_from has
// passed. A future-dated PutScope writes scope.version_scheduled instead,
// so consumers that re-validate runs (the engine) never move a run onto a
// version before it takes effect (spec 4.1 effective_from; GAPS G-CORE-01).
const (
	EventProgramCreated   = "scope.program_created"
	EventVersionPublished = "scope.version_published"
	EventVersionScheduled = "scope.version_scheduled"
	EventKillSwitchSet    = "scope.kill_switch_set"
)

// DueVersion names a future-dated version whose effective_from has been
// reached (Repo.DueVersions).
type DueVersion struct {
	Tenant        core.UUID
	Program       core.UUID
	Version       int32
	EffectiveFrom time.Time
}

// Repo errors.
var (
	ErrNotFound = errors.New("scope service: not found")
	ErrExists   = errors.New("scope service: already exists")
	ErrConflict = errors.New("scope service: version conflict")
)

// Repo is the scope service's storage. Every read and write takes the
// tenant and never returns another tenant's rows (ErrNotFound).
type Repo interface {
	// CreateProgram stores p and, when initial is set, its version 1, with
	// the outbox events, atomically. A duplicate name is ErrExists.
	CreateProgram(ctx context.Context, p Program, initial *Version, evs []Event) (Program, error)
	GetProgram(ctx context.Context, tenant, id core.UUID) (Program, error)
	// ListPrograms lists by id after the cursor.
	ListPrograms(ctx context.Context, tenant, after core.UUID, limit int) ([]Program, error)
	// PutVersion stores v as version base+1 if the program's latest
	// published version (Program.CurrentVersion, the optimistic-concurrency
	// head) is base (ErrConflict otherwise). Whether the version is in
	// effect is decided by its EffectiveFrom, not by this call.
	PutVersion(ctx context.Context, tenant core.UUID, base int32, v Version, evs []Event) (Version, error)
	// GetVersion returns version n; n = 0 is the latest published version
	// (the head), which may not be in effect yet. Callers that need the
	// version in effect use EffectiveVersion.
	GetVersion(ctx context.Context, tenant, program core.UUID, n int32) (Version, error)
	// EffectiveVersion returns the highest version whose effective_from is
	// at or before at (spec 4.1), 0 when no version is in effect yet, and
	// ErrNotFound when the tenant has no such program.
	EffectiveVersion(ctx context.Context, tenant, program core.UUID, at time.Time) (int32, error)
	// DueVersions returns, across tenants, the versions that were
	// future-dated when published (effective_from > created_at) and whose
	// effective_from lies in (after, upTo], ordered by effective_from.
	DueVersions(ctx context.Context, after, upTo time.Time, limit int) ([]DueVersion, error)
	// AppendEvents writes outbox events outside a state change (activation
	// notices); event ids already present are ignored.
	AppendEvents(ctx context.Context, evs []Event) error
	// ListVersions lists ascending versions after the cursor.
	ListVersions(ctx context.Context, tenant, program core.UUID, after int32, limit int) ([]Version, error)
	SetKillSwitch(ctx context.Context, ks KillSwitch, evs []Event) (KillSwitch, error)
	// KillSwitches returns the global switch and those of the tenant and its
	// programs.
	KillSwitches(ctx context.Context, tenant core.UUID) ([]KillSwitch, error)
	RecordDecisions(ctx context.Context, ds []DecisionRecord) error
	GetDecision(ctx context.Context, tenant, id core.UUID) (DecisionRecord, error)
	// Outbox is the scope.events outbox.
	Outbox() bus.OutboxSource
	// Ping checks the storage (readiness).
	Ping(ctx context.Context) error
}

// Engaged returns the first engaged switch applying to (tenant, program):
// global, then tenant, then program.
func Engaged(kss []KillSwitch, tenant, program core.UUID) (KillSwitch, bool) {
	for _, lvl := range []string{LevelGlobal, LevelTenant, LevelProgram} {
		for _, k := range kss {
			if !k.Enabled || k.Level != lvl {
				continue
			}
			if lvl == LevelGlobal || (lvl == LevelTenant && k.Target == tenant) || (lvl == LevelProgram && !program.IsZero() && k.Target == program) {
				return k, true
			}
		}
	}
	return KillSwitch{}, false
}
