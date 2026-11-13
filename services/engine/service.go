package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/eventstore"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
)

// Consumer is the inbox consumer name of the engine.
const Consumer = "engine"

// ToolStore is a tool registry that can also register specs (MemTools or
// pgrepo.Tools).
type ToolStore interface {
	engine.ToolRegistry
	Register(ctx context.Context, s engine.ToolSpec) error
}

// MemToolStore adapts MemTools to ToolStore.
type MemToolStore struct{ *MemTools }

// Register implements ToolStore.
func (m MemToolStore) Register(_ context.Context, s engine.ToolSpec) error {
	if cur, ok := m.Spec(s.Name); ok && cur.ComputeSHA256() == s.ComputeSHA256() {
		return nil
	}
	m.MemTools.Register(s)
	return nil
}

// Deps are the engine service's adapters.
type Deps struct {
	Store     eventstore.Store
	Repo      engine.Repo
	Tools     ToolStore
	Publisher engine.Publisher
	// Outbox is the run.events / approval.events outbox the relay publishes.
	Outbox  bus.OutboxSource
	Scope   *scopesvc.Client
	Runner  pb.ToolRunnerServiceClient
	Graph   pb.GraphServiceClient
	Limiter engine.Limiter
	Bus     bus.Bus // nil: no relay, no scope.events subscription
	Inbox   bus.Inbox
	Logger  *slog.Logger
	Metrics *platform.Metrics
	Clock   core.Clock
	// Verifier authenticates the schedule endpoint.
	Verifier platform.TokenVerifier
}

// Options tune the service.
type Options struct {
	Engine  engine.Config
	Workers int    // default 4
	Name    string // worker id prefix, default "engine"
	// Wakeups is an external wake source (LISTEN engine_wake).
	Wakeups <-chan struct{}
	// ToolSyncInterval defaults to one minute.
	ToolSyncInterval time.Duration
}

// Service is a wired engine.
type Service struct {
	Engine *engine.Engine
	Server *Server
	d      Deps
	o      Options
	ready  chan struct{}
	once   sync.Once
}

// New wires the engine (compiler.EngineAdapter as the Compiler port).
func New(d Deps, o Options) (*Service, error) {
	if d.Logger == nil {
		d.Logger = platform.DiscardLogger()
	}
	if d.Metrics == nil {
		d.Metrics = platform.NewMetrics(nil)
	}
	if d.Clock == nil {
		d.Clock = &core.SystemClock{}
	}
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if o.Name == "" {
		o.Name = "engine"
	}
	if o.ToolSyncInterval <= 0 {
		o.ToolSyncInterval = time.Minute
	}
	if o.Engine.Executor == "" {
		o.Engine.Executor = "tool-runner"
	}
	var lim engine.Limiter
	if d.Limiter != nil {
		lim = MeteredLimiter{Next: d.Limiter, Metrics: d.Metrics}
	}
	var pub engine.Publisher
	if d.Publisher != nil {
		pub = MeteredPublisher{Next: d.Publisher, Metrics: d.Metrics, Logger: d.Logger}
	}
	var ing engine.Ingester
	if d.Graph != nil {
		ing = &Ingester{C: d.Graph}
	}
	e, err := engine.New(engine.Deps{
		Store: d.Store, Repo: d.Repo, Compiler: Compiler{}, Scope: d.Scope, Tools: d.Tools,
		Executor: &Executor{C: d.Runner, Logger: d.Logger}, Ingester: ing, Limiter: lim, Publisher: pub,
		Observer: EngineObserver{Metrics: d.Metrics}, Clock: d.Clock, Rng: &lockedRng{r: core.SystemRng{}},
	}, o.Engine)
	if err != nil {
		return nil, err
	}
	return &Service{Engine: e, Server: &Server{E: e}, d: d, o: o, ready: make(chan struct{})}, nil
}

type lockedRng struct {
	mu sync.Mutex
	r  core.Rng
}

func (l *lockedRng) Uint64() uint64 { l.mu.Lock(); defer l.mu.Unlock(); return l.r.Uint64() }

// ToolsReady is closed after the first successful tool sync.
func (s *Service) ToolsReady() <-chan struct{} { return s.ready }

// SyncTools registers the tool runner's tools once.
func (s *Service) SyncTools(ctx context.Context) error {
	n, err := SyncTools(ctx, s.d.Runner, s.d.Tools.Register, s.d.Logger)
	if err != nil {
		return err
	}
	s.d.Logger.InfoContext(ctx, "engine.tools_synced", "tools", n)
	s.once.Do(func() { close(s.ready) })
	return nil
}

// Run starts the worker pool, the tool sync loop, the outbox relay and the
// scope.events subscription, and blocks until ctx is done.
func (s *Service) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wakes := make([]chan struct{}, s.o.Workers)
	for i := range s.o.Workers {
		wakes[i] = make(chan struct{}, 1)
		w := s.Engine.NewWorker(eventstore.WorkerID(fmt.Sprintf("%s-%d-%d", s.o.Name, core.SystemRng{}.Uint64()%1000000, i)), engine.WithWakeups(wakes[i]))
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.Run(ctx); err != nil && ctx.Err() == nil {
				s.d.Logger.ErrorContext(ctx, "engine.worker_stopped", "error", err.Error())
			}
		}()
	}
	if s.o.Wakeups != nil {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case <-s.o.Wakeups:
					for _, c := range wakes {
						select {
						case c <- struct{}{}:
						default:
						}
					}
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		backoff := 200 * time.Millisecond
		for ctx.Err() == nil {
			err := s.SyncTools(ctx)
			wait := s.o.ToolSyncInterval
			if err != nil {
				s.d.Logger.WarnContext(ctx, "engine.tool_sync_failed", "error", err.Error())
				wait, backoff = backoff, min(backoff*2, 10*time.Second)
			}
			select {
			case <-ctx.Done():
			case <-time.After(wait):
			}
		}
	}()
	if s.d.Bus != nil {
		if s.d.Outbox != nil {
			relay := &bus.Relay{Source: s.d.Outbox, Bus: s.d.Bus, Logger: s.d.Logger, Metrics: s.d.Metrics, Interval: 100 * time.Millisecond}
			wg.Add(1)
			go func() { defer wg.Done(); _ = relay.Run(ctx) }()
		}
		if err := s.d.Bus.Subscribe(ctx, convert.TopicScopeEvents, Consumer, s.HandleScopeEvent); err != nil {
			return err
		}
	} else {
		s.d.Logger.WarnContext(ctx, "engine.no_bus", "detail", "no event bus: run.events stay in the outbox and scope.events are not consumed")
	}
	wg.Wait()
	return nil
}

// HandleScopeEvent applies a scope.events message (spec 11.7): a new scope
// version pauses and re-validates the program's runs, an engaged kill
// switch cancels every run in its reach. Duplicates are skipped through the
// inbox; the handlers are idempotent, so a redelivery after a crash between
// the effect and the mark is harmless.
func (s *Service) HandleScopeEvent(ctx context.Context, env *pb.EventEnvelope) error {
	if s.d.Inbox != nil {
		if seen, err := s.d.Inbox.Processed(ctx, Consumer, env.GetEventId()); err != nil {
			return err
		} else if seen {
			return nil
		}
	}
	if s.d.Scope != nil {
		s.d.Scope.Invalidate()
	}
	tenant, err := convert.EnvelopeTenant(env)
	if err != nil {
		return nil // malformed: nothing to do, never redeliver
	}
	p := convert.StructToMap(env.GetGeneric())
	switch env.GetType() {
	case scopesvc.EventVersionPublished:
		v, _ := p["version"].(float64)
		program := convert.EnvelopeProgram(env)
		if program.IsZero() || v < 1 {
			return nil
		}
		if err := s.Engine.HandleScopeChanged(ctx, tenant, program, int32(v)); err != nil {
			return err
		}
	case scopesvc.EventKillSwitchSet:
		if on, _ := p["enabled"].(bool); !on {
			break
		}
		level, _ := p["level"].(string)
		target := core.Nil
		switch level {
		case scopesvc.LevelTenant:
			target = tenant
		case scopesvc.LevelProgram:
			target = convert.EnvelopeProgram(env)
		}
		if err := s.Engine.HandleKillSwitch(ctx, level, target); err != nil {
			return err
		}
	}
	if s.d.Inbox != nil {
		_, err := s.d.Inbox.MarkProcessed(ctx, Consumer, env.GetEventId())
		return err
	}
	return nil
}

// ScheduleFire is the body of POST /internal/schedules/{id}:fire (Cloud
// Scheduler).
type ScheduleFire struct {
	WorkflowName    string          `json:"workflow_name"`
	WorkflowVersion int32           `json:"workflow_version"`
	ProgramID       string          `json:"program_id"`
	Input           json.RawMessage `json:"input"`
	// FireTime defaults to the X-CloudScheduler-ScheduleTime header, else
	// the current minute.
	FireTime string `json:"fire_time"`
}

// HTTPHandler serves POST /internal/schedules/{id}:fire: it creates a run
// with idempotency key schedule:<id>:<fire time>, so a retried delivery of
// the same firing returns the same run. The caller presents an internal
// token (authorization: Bearer); the run is submitted as schedule:<id>.
func (s *Service) HTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := strings.CutPrefix(r.URL.Path, "/internal/schedules/")
		id, ok2 := strings.CutSuffix(rest, ":fire")
		if r.Method != http.MethodPost || !ok || !ok2 || id == "" || strings.Contains(id, "/") {
			http.NotFound(w, r)
			return
		}
		if s.d.Verifier == nil {
			http.Error(w, "no verifier", http.StatusUnauthorized)
			return
		}
		ident, err := platform.IdentityFromBearer(r.Context(), s.d.Verifier, r.Header.Get("Authorization"))
		if err != nil {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		var body ScheduleFire
		if err := core.DecodeStrict(io.LimitReader(r.Body, 1<<20), &body); err != nil {
			http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
			return
		}
		fire := body.FireTime
		if fire == "" {
			fire = r.Header.Get("X-CloudScheduler-ScheduleTime")
		}
		at, err := core.ParseTime(fire)
		if err != nil {
			at = s.d.Clock.Now().UTC().Truncate(time.Minute)
		}
		program, err := convert.OptionalID(core.PrefixProgram, "program_id", body.ProgramID)
		if err != nil {
			http.Error(w, "invalid program_id", http.StatusBadRequest)
			return
		}
		c := engine.Caller{Tenant: ident.Tenant, Actor: "schedule:" + id}
		run, err := s.Engine.SubmitRun(r.Context(), c, engine.SubmitRunRequest{WorkflowName: body.WorkflowName,
			WorkflowVersion: body.WorkflowVersion, ProgramID: program, Input: body.Input,
			IdempotencyKey: "schedule:" + id + ":" + core.FormatTime(at)})
		if err != nil {
			status := http.StatusInternalServerError
			var ce *core.Error
			if errors.As(err, &ce) {
				switch ce.Kind {
				case core.KindInvalidArgument:
					status = http.StatusBadRequest
				case core.KindNotFound:
					status = http.StatusNotFound
				case core.KindAlreadyExists, core.KindAborted, core.KindFailedPrecondition:
					status = http.StatusConflict
				case core.KindPermissionDenied:
					status = http.StatusForbidden
				}
			}
			http.Error(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"run_id": convert.ID(core.PrefixRun, run.ID), "status": string(run.Status)})
	})
}

// ListenWake LISTENs on the engine_wake channel (spec 9.4) and signals out
// for every notification, reconnecting with backoff, until ctx is done.
func ListenWake(ctx context.Context, dsn string, out chan<- struct{}, log *slog.Logger) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := pgx.Connect(ctx, dsn)
		if err == nil {
			_, err = conn.Exec(ctx, "LISTEN engine_wake")
		}
		for err == nil {
			_, err = conn.WaitForNotification(ctx)
			if err == nil {
				select {
				case out <- struct{}{}:
				default:
				}
				backoff = time.Second
			}
		}
		if conn != nil {
			_ = conn.Close(context.WithoutCancel(ctx))
		}
		if ctx.Err() != nil {
			return
		}
		log.WarnContext(ctx, "engine.listen_failed", "error", err.Error())
		select {
		case <-ctx.Done():
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}
