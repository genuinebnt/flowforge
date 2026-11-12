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
const (
	EventProgramCreated   = "scope.program_created"
	EventVersionPublished = "scope.version_published"
	EventKillSwitchSet    = "scope.kill_switch_set"
)

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
	// PutVersion stores v as version base+1 if the program's current version
	// is base (ErrConflict otherwise) and makes it current.
	PutVersion(ctx context.Context, tenant core.UUID, base int32, v Version, evs []Event) (Version, error)
	// GetVersion returns version n (0: current).
	GetVersion(ctx context.Context, tenant, program core.UUID, n int32) (Version, error)
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
