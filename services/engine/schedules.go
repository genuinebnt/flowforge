package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/api/idtoken"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// Workflow schedules (G-OPS-10). Spec 17 names one Cloud Scheduler job
// per schedule targeting POST /internal/schedules/{id}:fire; a schedule a
// user creates at runtime cannot create such a job (the engine has no
// Cloud Scheduler permission, and the free tier has 3 jobs). Instead the
// engine stores schedules (engine.schedules, migration 0017) and fires
// the due ones itself:
//
//   - in process, every Options.ScheduleInterval (local profiles);
//   - on GCP also from one Cloud Scheduler job that calls
//     POST /internal/schedules:tick every minute, so scaled-to-zero Cloud
//     Run instances still fire on time.
//
// Every firing submits the run with idempotency key
// schedule:<id>:<fire time> (the key spec 17 gives the fire endpoint), so
// two engines, or the loop and a tick, firing the same minute create one
// run. A schedule whose engine was down over several firing times fires
// once for the most recent of them (missed firings are coalesced, as
// Cloud Scheduler does), then continues from now.

// Schedule is one workflow schedule.
type Schedule struct {
	ID              core.UUID
	Tenant          core.UUID
	Name            string
	Cron            string // five fields, UTC (ParseCron)
	WorkflowName    string
	WorkflowVersion int32 // 0: latest
	Program         core.UUID
	Input           json.RawMessage
	Enabled         bool
	NextFireAt      time.Time
	LastFireAt      time.Time
	LastRunID       core.UUID
	LastError       string
	CreatedBy       string
	CreatedAt       time.Time
}

// FireResult is what firing a schedule changed.
type FireResult struct {
	FiredAt    time.Time
	NextFireAt time.Time // zero: no further firing (the schedule is disabled)
	RunID      core.UUID
	Error      string
}

// Schedule store errors.
var (
	ErrScheduleExists   = errors.New("engine: schedule name taken")
	ErrScheduleNotFound = errors.New("engine: schedule not found")
)

// ScheduleStore persists schedules. FireDue hands each due schedule
// (enabled, next_fire_at <= now) to fire, earliest next_fire_at first and
// ties by id so every store picks the same ones under limit, and stores
// the result atomically with its claim; concurrent callers never fire the
// same schedule at the same time. It returns how many it fired.
type ScheduleStore interface {
	Create(ctx context.Context, s Schedule) error
	List(ctx context.Context, tenant core.UUID) ([]Schedule, error)
	Get(ctx context.Context, tenant, id core.UUID) (Schedule, error)
	Delete(ctx context.Context, tenant, id core.UUID) error
	FireDue(ctx context.Context, now time.Time, limit int, fire func(context.Context, Schedule) FireResult) (int, error)
}

// MemSchedules is the in-memory ScheduleStore.
type MemSchedules struct {
	mu   sync.Mutex
	fire sync.Mutex
	m    map[core.UUID]Schedule
}

// NewMemSchedules returns an empty store.
func NewMemSchedules() *MemSchedules { return &MemSchedules{m: map[core.UUID]Schedule{}} }

// Create implements ScheduleStore.
func (s *MemSchedules) Create(_ context.Context, sc Schedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, x := range s.m {
		if x.Tenant == sc.Tenant && x.Name == sc.Name {
			return ErrScheduleExists
		}
	}
	s.m[sc.ID] = sc
	return nil
}

// List implements ScheduleStore.
func (s *MemSchedules) List(_ context.Context, tenant core.UUID) ([]Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Schedule
	for _, x := range s.m {
		if x.Tenant == tenant {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Get implements ScheduleStore.
func (s *MemSchedules) Get(_ context.Context, tenant, id core.UUID) (Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.m[id]
	if !ok || x.Tenant != tenant {
		return Schedule{}, ErrScheduleNotFound
	}
	return x, nil
}

// Delete implements ScheduleStore.
func (s *MemSchedules) Delete(_ context.Context, tenant, id core.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.m[id]
	if !ok || x.Tenant != tenant {
		return ErrScheduleNotFound
	}
	delete(s.m, id)
	return nil
}

// FireDue implements ScheduleStore (one firing pass at a time). Due
// schedules fire earliest next_fire_at first, ties by id, at most limit.
func (s *MemSchedules) FireDue(ctx context.Context, now time.Time, limit int, fire func(context.Context, Schedule) FireResult) (int, error) {
	s.fire.Lock()
	defer s.fire.Unlock()
	s.mu.Lock()
	var due []Schedule
	for _, x := range s.m {
		if x.Enabled && !x.NextFireAt.After(now) {
			due = append(due, x)
		}
	}
	s.mu.Unlock()
	// Earliest first, ties by id, the order of PgSchedules' ORDER BY
	// next_fire_at, id, so both stores fire the same schedules under a
	// limit and in the same order.
	//
	// PORT: Go map iteration is random, and Postgres orders uuid by its 16
	// bytes, which bytes.Compare reproduces.
	slices.SortFunc(due, func(a, b Schedule) int {
		if c := a.NextFireAt.Compare(b.NextFireAt); c != 0 {
			return c
		}
		return bytes.Compare(a.ID[:], b.ID[:])
	})
	if len(due) > limit {
		due = due[:limit]
	}
	for _, x := range due {
		r := fire(ctx, x)
		s.mu.Lock()
		if cur, ok := s.m[x.ID]; ok {
			cur.LastFireAt, cur.LastRunID, cur.LastError = r.FiredAt, r.RunID, r.Error
			cur.NextFireAt = r.NextFireAt
			if r.NextFireAt.IsZero() {
				cur.Enabled = false
			}
			s.m[x.ID] = cur
		}
		s.mu.Unlock()
	}
	return len(due), nil
}

// PgSchedules is ScheduleStore over engine.schedules.
type PgSchedules struct{ DB bus.DB }

const scheduleCols = `id::text, tenant_id::text, name, cron, workflow_name, workflow_version, COALESCE(program_id::text, ''),
  input::text, enabled, next_fire_at, last_fire_at, COALESCE(last_run_id::text, ''), last_error, created_by, created_at`

func scanSchedule(row pgx.Row) (Schedule, error) {
	var s Schedule
	var id, tenant, program, input, run string
	var last *time.Time
	if err := row.Scan(&id, &tenant, &s.Name, &s.Cron, &s.WorkflowName, &s.WorkflowVersion, &program, &input, &s.Enabled,
		&s.NextFireAt, &last, &run, &s.LastError, &s.CreatedBy, &s.CreatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return s, ErrScheduleNotFound
		}
		return s, err
	}
	s.ID, s.Tenant, s.Input = core.MustParseUUID(id), core.MustParseUUID(tenant), json.RawMessage(input)
	if program != "" {
		s.Program = core.MustParseUUID(program)
	}
	if run != "" {
		s.LastRunID = core.MustParseUUID(run)
	}
	if last != nil {
		s.LastFireAt = *last
	}
	return s, nil
}

func nullUUID(u core.UUID) any {
	if u.IsZero() {
		return nil
	}
	return u.String()
}

// Create implements ScheduleStore.
func (p PgSchedules) Create(ctx context.Context, s Schedule) error {
	input := string(s.Input)
	if input == "" {
		input = "{}"
	}
	_, err := p.DB.Exec(ctx, `INSERT INTO engine.schedules (id, tenant_id, name, cron, workflow_name, workflow_version, program_id,
  input, enabled, next_fire_at, created_by, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8::text::jsonb, $9, $10, $11, $12)`,
		s.ID.String(), s.Tenant.String(), s.Name, s.Cron, s.WorkflowName, s.WorkflowVersion, nullUUID(s.Program), input, s.Enabled,
		s.NextFireAt, s.CreatedBy, s.CreatedAt)
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return ErrScheduleExists
	}
	return err
}

// List implements ScheduleStore.
func (p PgSchedules) List(ctx context.Context, tenant core.UUID) ([]Schedule, error) {
	rows, err := p.DB.Query(ctx, `SELECT `+scheduleCols+` FROM engine.schedules WHERE tenant_id = $1 ORDER BY name`, tenant.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schedule
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Get implements ScheduleStore.
func (p PgSchedules) Get(ctx context.Context, tenant, id core.UUID) (Schedule, error) {
	return scanSchedule(p.DB.QueryRow(ctx, `SELECT `+scheduleCols+` FROM engine.schedules WHERE tenant_id = $1 AND id = $2`, tenant.String(), id.String()))
}

// Delete implements ScheduleStore.
func (p PgSchedules) Delete(ctx context.Context, tenant, id core.UUID) error {
	tag, err := p.DB.Exec(ctx, `DELETE FROM engine.schedules WHERE tenant_id = $1 AND id = $2`, tenant.String(), id.String())
	if err == nil && tag.RowsAffected() == 0 {
		return ErrScheduleNotFound
	}
	return err
}

// FireDue implements ScheduleStore, firing due schedules earliest
// next_fire_at first, ties by id (as MemSchedules does), at most limit.
// Each schedule is claimed with
// FOR UPDATE SKIP LOCKED in its own transaction, which stays open while
// fire submits the run, so another engine skips it; the run's idempotency
// key covers a crash between the submit and the commit.
func (p PgSchedules) FireDue(ctx context.Context, now time.Time, limit int, fire func(context.Context, Schedule) FireResult) (int, error) {
	n := 0
	for n < limit {
		fired, err := p.fireOne(ctx, now, fire)
		if err != nil || !fired {
			return n, err
		}
		n++
	}
	return n, nil
}

func (p PgSchedules) fireOne(ctx context.Context, now time.Time, fire func(context.Context, Schedule) FireResult) (bool, error) {
	tx, err := p.DB.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	s, err := scanSchedule(tx.QueryRow(ctx, `SELECT `+scheduleCols+` FROM engine.schedules
WHERE enabled AND next_fire_at <= $1 ORDER BY next_fire_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, now))
	if errors.Is(err, ErrScheduleNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	r := fire(ctx, s)
	var next any
	if !r.NextFireAt.IsZero() {
		next = r.NextFireAt
	}
	if _, err := tx.Exec(ctx, `UPDATE engine.schedules SET last_fire_at = $2, last_run_id = $3, last_error = $4,
  next_fire_at = COALESCE($5, next_fire_at), enabled = enabled AND $5::timestamptz IS NOT NULL WHERE id = $1`,
		s.ID.String(), r.FiredAt, nullUUID(r.RunID), r.Error, next); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Scheduler fires due schedules through the engine.
type Scheduler struct {
	Store  ScheduleStore
	Engine *engine.Engine
	Clock  core.Clock
	Logger *slog.Logger
	// Limit bounds one Tick (default 100), so a tick request stays short.
	Limit int
}

// Tick fires every due schedule (at most Limit) and returns how many.
func (s *Scheduler) Tick(ctx context.Context) (int, error) {
	limit := s.Limit
	if limit <= 0 {
		limit = 100
	}
	now := s.Clock.Now().UTC()
	return s.Store.FireDue(ctx, now, limit, func(ctx context.Context, sc Schedule) FireResult {
		at := sc.NextFireAt.UTC()
		res := FireResult{FiredAt: at}
		c, err := ParseCron(sc.Cron)
		if err != nil {
			// Create validates the expression, so only a hand-edited row gets
			// here: record why and disable it rather than retry every tick.
			res.Error = err.Error()
			return res
		}
		// Coalesce missed firings: the next time is after now, not after
		// the (possibly long past) firing time.
		res.NextFireAt = c.Next(now)
		run, err := s.Engine.SubmitRun(ctx, engine.Caller{Tenant: sc.Tenant, Actor: "schedule:" + convert.ID(core.PrefixSchedule, sc.ID)},
			engine.SubmitRunRequest{WorkflowName: sc.WorkflowName, WorkflowVersion: sc.WorkflowVersion, ProgramID: sc.Program,
				Input: sc.Input, IdempotencyKey: "schedule:" + convert.ID(core.PrefixSchedule, sc.ID) + ":" + core.FormatTime(at)})
		if err != nil {
			res.Error = truncate(err.Error(), 500)
			s.Logger.WarnContext(ctx, "engine.schedule_fire_failed", "schedule_id", convert.ID(core.PrefixSchedule, sc.ID), "error", res.Error)
			return res
		}
		res.RunID = run.ID
		return res
	})
}

// Run ticks every interval until ctx is done.
func (s *Scheduler) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.Logger.WarnContext(ctx, "engine.schedule_tick_failed", "error", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// ScheduleJSON is the wire form of a schedule (the gateway's /v1/schedules
// body and response).
type ScheduleJSON struct {
	ID              string          `json:"id,omitempty"`
	Name            string          `json:"name"`
	Cron            string          `json:"cron"`
	WorkflowName    string          `json:"workflow_name"`
	WorkflowVersion int32           `json:"workflow_version,omitempty"`
	ProgramID       string          `json:"program_id,omitempty"`
	Input           json.RawMessage `json:"input,omitempty"`
	Enabled         *bool           `json:"enabled,omitempty"`
	NextFireAt      string          `json:"next_fire_at,omitempty"`
	LastFireAt      string          `json:"last_fire_at,omitempty"`
	LastRunID       string          `json:"last_run_id,omitempty"`
	LastError       string          `json:"last_error,omitempty"`
	CreatedBy       string          `json:"created_by,omitempty"`
	CreatedAt       string          `json:"created_at,omitempty"`
}

func scheduleJSON(s Schedule) ScheduleJSON {
	en := s.Enabled
	j := ScheduleJSON{ID: convert.ID(core.PrefixSchedule, s.ID), Name: s.Name, Cron: s.Cron, WorkflowName: s.WorkflowName,
		WorkflowVersion: s.WorkflowVersion, Input: s.Input, Enabled: &en, LastError: s.LastError, CreatedBy: s.CreatedBy,
		CreatedAt: core.FormatTime(s.CreatedAt)}
	if !s.Program.IsZero() {
		j.ProgramID = convert.ID(core.PrefixProgram, s.Program)
	}
	if en {
		j.NextFireAt = core.FormatTime(s.NextFireAt)
	}
	if !s.LastFireAt.IsZero() {
		j.LastFireAt = core.FormatTime(s.LastFireAt)
	}
	if !s.LastRunID.IsZero() {
		j.LastRunID = convert.ID(core.PrefixRun, s.LastRunID)
	}
	return j
}

var scheduleName = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// TickAuth authenticates POST /internal/schedules:tick: an internal token
// (any service or the gateway), or, when Audience is set, a Google ID
// token for that audience issued to ServiceAccount (the Cloud Scheduler
// job's OIDC token; Cloud Run's IAM check has already run).
type TickAuth struct {
	Verifier       platform.TokenVerifier
	Audience       string
	ServiceAccount string
	// ValidateOIDC defaults to idtoken.Validate (Google's certificates).
	ValidateOIDC func(ctx context.Context, token, audience string) (*idtoken.Payload, error)
}

func (a TickAuth) check(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if a.Verifier != nil {
		if _, err := platform.IdentityFromBearer(r.Context(), a.Verifier, h); err == nil {
			return true
		}
	}
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if a.Audience == "" || !ok {
		return false
	}
	validate := a.ValidateOIDC
	if validate == nil {
		validate = idtoken.Validate
	}
	p, err := validate(r.Context(), tok, a.Audience)
	if err != nil {
		return false
	}
	email, _ := p.Claims["email"].(string)
	verified, _ := p.Claims["email_verified"].(bool)
	return a.ServiceAccount != "" && email == a.ServiceAccount && verified
}

// SchedulesHandler serves the schedule API under /internal/schedules
// (called by the gateway with the caller's internal token; the gateway has
// checked the runs:submit permission):
//
//	GET    /internal/schedules          list the tenant's schedules
//	POST   /internal/schedules          create one (ScheduleJSON)
//	GET    /internal/schedules/{id}     get one
//	DELETE /internal/schedules/{id}     delete one
//	POST   /internal/schedules/{id}:fire  the spec 17 Cloud Scheduler target
//	POST   /internal/schedules:tick     fire every due schedule (TickAuth)
//
// Maintenance, when set, runs on every tick too, so the single Cloud
// Scheduler job also drives retention on GCP (G-OPS-03).
func (s *Service) SchedulesHandler(sched *Scheduler, auth TickAuth, maintenance func(context.Context) error) http.Handler {
	fire := s.HTTPHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/internal/schedules:tick":
			if r.Method != http.MethodPost {
				http.NotFound(w, r)
				return
			}
			if !auth.check(r) {
				http.Error(w, "unauthenticated", http.StatusUnauthorized)
				return
			}
			n, err := sched.Tick(r.Context())
			var merr error
			if maintenance != nil {
				merr = maintenance(r.Context())
			}
			if err = errors.Join(err, merr); err != nil {
				s.d.Logger.WarnContext(r.Context(), "engine.tick_failed", "error", err.Error())
				http.Error(w, "tick failed", http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"fired": n})
			return
		case strings.HasSuffix(path, ":fire"):
			fire.ServeHTTP(w, r)
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
		ctx := r.Context()
		switch {
		case path == "/internal/schedules" && r.Method == http.MethodGet:
			list, err := sched.Store.List(ctx, ident.Tenant)
			if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			out := []ScheduleJSON{}
			for _, x := range list {
				out = append(out, scheduleJSON(x))
			}
			writeJSON(w, http.StatusOK, map[string]any{"schedules": out})
		case path == "/internal/schedules" && r.Method == http.MethodPost:
			var body ScheduleJSON
			if err := core.DecodeStrict(io.LimitReader(r.Body, 1<<20), &body); err != nil {
				http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
				return
			}
			sc, msg := s.newSchedule(ctx, sched, ident, body)
			if msg != "" {
				http.Error(w, msg, http.StatusBadRequest)
				return
			}
			if err := sched.Store.Create(ctx, sc); errors.Is(err, ErrScheduleExists) {
				http.Error(w, "a schedule with this name exists", http.StatusConflict)
				return
			} else if err != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusCreated, scheduleJSON(sc))
		case strings.HasPrefix(path, "/internal/schedules/"):
			id, err := core.ParsePublicID(core.PrefixSchedule, strings.TrimPrefix(path, "/internal/schedules/"))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			switch r.Method {
			case http.MethodGet:
				sc, err := sched.Store.Get(ctx, ident.Tenant, id)
				if err != nil {
					http.NotFound(w, r)
					return
				}
				writeJSON(w, http.StatusOK, scheduleJSON(sc))
			case http.MethodDelete:
				if err := sched.Store.Delete(ctx, ident.Tenant, id); errors.Is(err, ErrScheduleNotFound) {
					http.NotFound(w, r)
				} else if err != nil {
					http.Error(w, "internal error", http.StatusInternalServerError)
				} else {
					w.WriteHeader(http.StatusNoContent)
				}
			default:
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	})
}

// newSchedule validates a create request. The workflow must exist now
// (a typo would otherwise only show up as last_error at the first firing).
func (s *Service) newSchedule(ctx context.Context, sched *Scheduler, ident platform.Identity, b ScheduleJSON) (Schedule, string) {
	if !scheduleName.MatchString(b.Name) {
		return Schedule{}, "name must match [a-z][a-z0-9_.-]{0,63}"
	}
	c, err := ParseCron(b.Cron)
	if err != nil {
		return Schedule{}, err.Error()
	}
	if b.WorkflowName == "" {
		return Schedule{}, "workflow_name is required"
	}
	program, err := convert.OptionalID(core.PrefixProgram, "program_id", b.ProgramID)
	if err != nil {
		return Schedule{}, "invalid program_id"
	}
	if len(b.Input) > 0 && !json.Valid(b.Input) {
		return Schedule{}, "input must be JSON"
	}
	if _, err := s.Engine.GetWorkflowVersion(ctx, engine.Caller{Tenant: ident.Tenant, Actor: ident.Actor}, b.WorkflowName, b.WorkflowVersion); err != nil {
		return Schedule{}, fmt.Sprintf("workflow %s: %v", b.WorkflowName, err)
	}
	now := sched.Clock.Now().UTC()
	next := c.Next(now)
	if next.IsZero() {
		return Schedule{}, "the cron expression never fires"
	}
	enabled := b.Enabled == nil || *b.Enabled
	return Schedule{ID: core.NewUUIDv7(sched.Clock, &lockedRng{r: core.SystemRng{}}), Tenant: ident.Tenant, Name: b.Name, Cron: c.String(),
		WorkflowName: b.WorkflowName, WorkflowVersion: b.WorkflowVersion, Program: program, Input: b.Input, Enabled: enabled,
		NextFireAt: next, CreatedBy: ident.Actor, CreatedAt: now}, ""
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
