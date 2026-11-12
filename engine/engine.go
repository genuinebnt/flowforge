package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
)

// Deps are the ports the engine is wired with. Store, Repo, Compiler,
// Scope, Tools, Executor, Clock and Rng are required; the others are
// optional (a nil Ingester drops observations, a nil Limiter admits every
// call, a nil Publisher publishes nothing, nil Embedder and Similarity
// make those effects fail permanently).
type Deps struct {
	Store      eventstore.Store
	Repo       Repo
	Compiler   Compiler
	Scope      Scope
	Tools      ToolRegistry
	Executor   ToolExecutor
	Ingester   Ingester
	Limiter    Limiter
	Publisher  Publisher
	Embedder   Embedder
	Similarity SimilarityIndex
	Clock      core.Clock
	Rng        core.Rng
}

// Engine implements the EngineService API (spec 11.4) over its ports and
// creates the workers that advance runs (spec 9.4). It is safe for
// concurrent use.
type Engine struct {
	d   Deps
	cfg Config
	ids core.IDGen
	hub *hub
}

// New returns an engine. It fails if a required port is missing.
func New(d Deps, cfg Config) (*Engine, error) {
	switch {
	case d.Store == nil, d.Repo == nil, d.Compiler == nil, d.Scope == nil,
		d.Tools == nil, d.Executor == nil, d.Clock == nil, d.Rng == nil:
		return nil, errors.New("engine: Store, Repo, Compiler, Scope, Tools, Executor, Clock and Rng are required")
	}
	d.Rng = &lockedRng{r: d.Rng}
	return &Engine{d: d, cfg: cfg.withDefaults(), ids: core.IDGen{Clock: d.Clock, Rng: d.Rng}, hub: newHub()}, nil
}

// Config returns the effective configuration (defaults applied).
func (e *Engine) Config() Config { return e.cfg }

func (e *Engine) now() time.Time { return e.d.Clock.Now().UTC() }

// wake makes a run due now, notifies a worker advancing it in this
// process, and wakes idle local workers.
func (e *Engine) wake(ctx context.Context, run core.UUID) error {
	err := e.d.Store.Wake(ctx, run)
	e.hub.notify(run)
	return err
}

// lockedRng makes a core.Rng safe for concurrent use.
type lockedRng struct {
	mu sync.Mutex
	r  core.Rng
}

func (l *lockedRng) Uint64() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.r.Uint64()
}

// hub delivers in-process notifications: per-run signals to the worker
// advancing the run (cancel, approval decided, ...) and wakeups to idle
// workers. Cross-process delivery goes through Store.Wake and the
// workers' polling.
type hub struct {
	mu      sync.Mutex
	runs    map[core.UUID]map[chan struct{}]struct{}
	workers map[chan struct{}]struct{}
}

func newHub() *hub {
	return &hub{runs: map[core.UUID]map[chan struct{}]struct{}{}, workers: map[chan struct{}]struct{}{}}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (h *hub) subscribeRun(run core.UUID) (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.runs[run] == nil {
		h.runs[run] = map[chan struct{}]struct{}{}
	}
	h.runs[run][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.runs[run], ch)
		if len(h.runs[run]) == 0 {
			delete(h.runs, run)
		}
		h.mu.Unlock()
	}
}

func (h *hub) subscribeWorker() (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.workers[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.workers, ch)
		h.mu.Unlock()
	}
}

func (h *hub) notify(run core.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.runs[run] {
		signal(ch)
	}
	for ch := range h.workers {
		signal(ch)
	}
}

func (h *hub) notifyWorkers() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.workers {
		signal(ch)
	}
}
