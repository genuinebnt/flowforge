package engine

import (
	"time"

	"github.com/genuinebnt/flowforge/vm"
)

// Config tunes the engine. Zero fields take the defaults below.
type Config struct {
	// FuelLimit is the per-run fuel limit (spec 8.7, default 50,000,000).
	FuelLimit int64
	// FuelSlice is how much fuel the VM gets between checks of the lease
	// and of cancellation (default 1,000,000).
	FuelSlice int64
	// LeaseTTL is the lease duration (default 30 s, spec 9.4).
	LeaseTTL time.Duration
	// RenewEvery is how often a held lease is renewed (default LeaseTTL/3,
	// 10 s with the default TTL).
	RenewEvery time.Duration
	// PollInterval is the real-time period of the per-run watchdog that
	// renews the lease and notices cancellation, kill switches and the
	// deadline while an effect is in flight (default 1 s; tests use 1 ms).
	PollInterval time.Duration
	// CancelGrace is how long the engine waits for in-flight executions to
	// return after cancelling them (default 2 s, spec 9.8).
	CancelGrace time.Duration
	// TenantConcurrency caps the runs of one tenant a worker advances at
	// once (default 20, spec 9.4).
	TenantConcurrency int
	// WorkerConcurrency caps the runs a worker advances at once
	// (default 16).
	WorkerConcurrency int
	// IdleBackoff is the longest a worker sleeps when no run is ready and
	// no wake notification arrives (default 1 s; it starts at 1/16 of it).
	IdleBackoff time.Duration
	// SnapshotEveryEffects and SnapshotInterval control VM snapshots
	// (spec 8.7: every 500 effects or 5 minutes).
	SnapshotEveryEffects int
	SnapshotInterval     time.Duration
	// LogCap caps the message of a log event (default 4 KiB).
	LogCap int
	// EscalationRoles maps an approval role to the role an expired
	// approval escalates to (spec 9.7). At most MaxEscalations (default 3)
	// escalations happen per approval effect.
	EscalationRoles map[string]string
	MaxEscalations  int
	// RateLimitErrorBackoff is how long a tool call waits when the limiter
	// itself fails (fail closed, default 1 s).
	RateLimitErrorBackoff time.Duration
	// Executor is the executor name recorded in effect_started.
	Executor string
	// DrainTimeout bounds the graceful drain of Worker.Run once its context
	// is cancelled (SIGTERM): the worker stops claiming, in-flight effects
	// may finish and be recorded, each run is released at its next effect
	// boundary with next_wake_at = now, and only after DrainTimeout are the
	// remaining executions cancelled, as a crash would (their effects then
	// become outcome-unknown for the next owner). Real time. Default 20 s,
	// below the termination grace of the platform; negative disables the
	// drain (cancel at once).
	DrainTimeout time.Duration
}

func (c Config) withDefaults() Config {
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	if c.FuelLimit <= 0 {
		c.FuelLimit = vm.DefaultFuelLimit
	}
	if c.FuelSlice <= 0 {
		c.FuelSlice = 1_000_000
	}
	def(&c.LeaseTTL, 30*time.Second)
	def(&c.RenewEvery, c.LeaseTTL/3)
	def(&c.PollInterval, time.Second)
	def(&c.CancelGrace, 2*time.Second)
	def(&c.IdleBackoff, time.Second)
	def(&c.SnapshotInterval, 5*time.Minute)
	def(&c.RateLimitErrorBackoff, time.Second)
	if c.DrainTimeout == 0 {
		c.DrainTimeout = 20 * time.Second
	}
	if c.TenantConcurrency <= 0 {
		c.TenantConcurrency = 20
	}
	if c.WorkerConcurrency <= 0 {
		c.WorkerConcurrency = 16
	}
	if c.SnapshotEveryEffects <= 0 {
		c.SnapshotEveryEffects = 500
	}
	if c.LogCap <= 0 {
		c.LogCap = 4096
	}
	if c.MaxEscalations <= 0 {
		c.MaxEscalations = 3
	}
	if c.Executor == "" {
		c.Executor = "toolrunner"
	}
	return c
}
