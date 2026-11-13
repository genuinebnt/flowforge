package scope

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// NetResolver adapts net.Resolver to the scope.Resolver port (production
// name resolution for active checks and the dnsresolve tool).
type NetResolver struct{ R *net.Resolver }

// LookupAddrs implements scope.Resolver.
func (n NetResolver) LookupAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	r := n.R
	if r == nil {
		r = net.DefaultResolver
	}
	addrs, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for i, a := range addrs {
		addrs[i] = a.Unmap()
	}
	return addrs, nil
}

// Options configure a Server.
type Options struct {
	Repo     Repo
	Resolver scope.Resolver // nil: NetResolver
	Clock    core.Clock
	Logger   *slog.Logger
	Metrics  *platform.Metrics
}

// Server implements flowforge.v1.ScopeService.
type Server struct {
	pb.UnimplementedScopeServiceServer
	o Options

	mu       sync.Mutex
	policies map[policyKey]compiled
}

// compiled is a cached policy with the effective_from of its version, so
// a pinned check can refuse a version that is not in effect yet.
type compiled struct {
	pol *scope.Policy
	eff time.Time
}

type policyKey struct {
	program core.UUID
	version int32
}

// NewServer returns a scope service.
func NewServer(o Options) *Server {
	if o.Resolver == nil {
		o.Resolver = NetResolver{}
	}
	if o.Clock == nil {
		o.Clock = &core.SystemClock{}
	}
	if o.Logger == nil {
		o.Logger = platform.DiscardLogger()
	}
	if o.Metrics == nil {
		o.Metrics = platform.NewMetrics(nil)
	}
	return &Server{o: o, policies: map[policyKey]compiled{}}
}

func repoErr(what string, err error) error {
	switch {
	case errors.Is(err, ErrNotFound):
		return core.Errorf(core.KindNotFound, core.ReasonNotFound, "%s not found", what)
	case errors.Is(err, ErrExists):
		return core.Errorf(core.KindAlreadyExists, core.ReasonAlreadyExists, "%s already exists", what)
	case errors.Is(err, ErrConflict):
		return core.Errorf(core.KindAborted, core.ReasonVersionConflict, "the scope changed since base_version; re-read and retry")
	case err == nil:
		return nil
	}
	var ce *core.Error
	if errors.As(err, &ce) {
		return err
	}
	return &core.Error{Kind: core.KindUnavailable, Reason: core.ReasonUnavailable, Message: what + ": storage unavailable", Err: err}
}

func (s *Server) newID() core.UUID { return core.NewUUIDv7(s.o.Clock, core.SystemRng{}) }

// ProgramProto converts a program.
func ProgramProto(p Program) *pb.Program {
	return &pb.Program{Id: convert.ID(core.PrefixProgram, p.ID), Name: p.Name, Platform: p.Platform, Status: p.Status,
		CurrentVersion: p.CurrentVersion, CreatedAt: convert.TS(p.CreatedAt)}
}

// VersionProto converts a scope version.
func VersionProto(v Version) *pb.ScopeVersion {
	return &pb.ScopeVersion{ProgramId: convert.ID(core.PrefixProgram, v.Program), Version: v.Version,
		Document: convert.ScopeDocProto(v.Document), EffectiveFrom: convert.TS(v.EffectiveFrom),
		CreatedBy: v.CreatedBy, CreatedAt: convert.TS(v.CreatedAt)}
}

func killProto(k KillSwitch) *pb.KillSwitch {
	target := ""
	switch k.Level {
	case LevelTenant:
		target = convert.ID(core.PrefixTenant, k.Target)
	case LevelProgram:
		target = convert.ID(core.PrefixProgram, k.Target)
	}
	return &pb.KillSwitch{Level: k.Level, TargetId: target, Enabled: k.Enabled, Reason: k.Reason, SetBy: k.SetBy, SetAt: convert.TS(k.SetAt)}
}

// CreateProgram implements ScopeService.
func (s *Server) CreateProgram(ctx context.Context, req *pb.CreateProgramRequest) (*pb.Program, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetName() == "" || len(req.GetName()) > 200 {
		return nil, convert.Invalid("name", "name is required (at most 200 bytes)")
	}
	now := s.o.Clock.Now().UTC()
	p := Program{ID: s.newID(), Tenant: id.Tenant, Name: req.GetName(), Platform: req.GetPlatform(), Status: "active", CreatedAt: now}
	evs := []Event{{ID: s.newID(), Type: EventProgramCreated, Tenant: id.Tenant, Program: p.ID, At: now,
		Payload: map[string]any{"program_id": convert.ID(core.PrefixProgram, p.ID), "name": p.Name}}}
	var initial *Version
	if req.GetInitialScope() != nil {
		doc, err := convert.ScopeDoc(req.GetInitialScope())
		if err != nil {
			return nil, err
		}
		doc.ProgramID, doc.Version = p.ID.String(), 1
		initial = &Version{Document: doc, EffectiveFrom: now, CreatedBy: id.Actor, CreatedAt: now}
		evs = append(evs, Event{ID: s.newID(), Type: EventVersionPublished, Tenant: id.Tenant, Program: p.ID, At: now,
			Payload: map[string]any{"program_id": convert.ID(core.PrefixProgram, p.ID), "version": 1}})
	}
	p, err = s.o.Repo.CreateProgram(ctx, p, initial, evs)
	if err != nil {
		return nil, repoErr("program", err)
	}
	return ProgramProto(p), nil
}

// GetProgram implements ScopeService.
func (s *Server) GetProgram(ctx context.Context, req *pb.GetProgramRequest) (*pb.Program, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	pid, err := convert.ParseID(core.PrefixProgram, "program_id", req.GetProgramId())
	if err != nil {
		return nil, err
	}
	p, err := s.o.Repo.GetProgram(ctx, id.Tenant, pid)
	if err != nil {
		return nil, repoErr("program", err)
	}
	return ProgramProto(p), nil
}

// ListPrograms implements ScopeService.
func (s *Server) ListPrograms(ctx context.Context, req *pb.ListProgramsRequest) (*pb.ListProgramsResponse, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	n, tok, err := convert.Page(req.GetPage())
	if err != nil {
		return nil, err
	}
	after, err := convert.OptionalID(core.PrefixProgram, "page.page_token", tok)
	if err != nil {
		return nil, err
	}
	ps, err := s.o.Repo.ListPrograms(ctx, id.Tenant, after, n+1)
	if err != nil {
		return nil, repoErr("programs", err)
	}
	out := &pb.ListProgramsResponse{}
	for i, p := range ps {
		if i == n {
			out.Page = convert.PageResp(convert.ID(core.PrefixProgram, ps[n-1].ID))
			break
		}
		out.Programs = append(out.Programs, ProgramProto(p))
	}
	return out, nil
}

// PutScope implements ScopeService: version base_version+1, or ABORTED
// VERSION_CONFLICT when another version was published since.
func (s *Server) PutScope(ctx context.Context, req *pb.PutScopeRequest) (*pb.ScopeVersion, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	pid, err := convert.ParseID(core.PrefixProgram, "program_id", req.GetProgramId())
	if err != nil {
		return nil, err
	}
	if req.GetBaseVersion() < 0 {
		return nil, convert.Invalid("base_version", "base_version must not be negative")
	}
	doc, err := convert.ScopeDoc(req.GetDocument())
	if err != nil {
		return nil, err
	}
	now := s.o.Clock.Now().UTC()
	eff := convert.Time(req.GetEffectiveFrom())
	if eff.IsZero() {
		eff = now
	}
	doc.ProgramID, doc.Version, doc.EffectiveFrom = pid.String(), int(req.GetBaseVersion()+1), eff
	v := Version{Program: pid, Document: doc, EffectiveFrom: eff, CreatedBy: id.Actor, CreatedAt: now}
	// A future-dated version is announced as scheduled; ActivateDue
	// publishes scope.version_published once effective_from passes, so no
	// consumer applies a widening early (G-CORE-01).
	typ := EventVersionPublished
	if eff.After(now) {
		typ = EventVersionScheduled
	}
	evs := []Event{{ID: s.newID(), Type: typ, Tenant: id.Tenant, Program: pid, At: now,
		Payload: map[string]any{"program_id": convert.ID(core.PrefixProgram, pid), "version": req.GetBaseVersion() + 1,
			"effective_from": core.FormatTime(eff)}}}
	v, err = s.o.Repo.PutVersion(ctx, id.Tenant, req.GetBaseVersion(), v, evs)
	if err != nil {
		return nil, repoErr("program", err)
	}
	return VersionProto(v), nil
}

// GetScope implements ScopeService. Version 0 is the version in effect now
// (the highest one whose effective_from has passed, spec 4.1); a pending
// future-dated version is returned only when asked for by number, and
// ListScopeVersions lists it with its effective_from.
func (s *Server) GetScope(ctx context.Context, req *pb.GetScopeRequest) (*pb.ScopeVersion, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	pid, err := convert.ParseID(core.PrefixProgram, "program_id", req.GetProgramId())
	if err != nil {
		return nil, err
	}
	n := req.GetVersion()
	if n == 0 {
		if n, err = s.o.Repo.EffectiveVersion(ctx, id.Tenant, pid, s.o.Clock.Now().UTC()); err != nil {
			return nil, repoErr("program", err)
		}
		if n == 0 {
			return nil, core.Errorf(core.KindNotFound, core.ReasonNotFound, "no scope version is in effect yet")
		}
	}
	v, err := s.o.Repo.GetVersion(ctx, id.Tenant, pid, n)
	if err != nil {
		return nil, repoErr("scope version", err)
	}
	return VersionProto(v), nil
}

// ListScopeVersions implements ScopeService.
func (s *Server) ListScopeVersions(ctx context.Context, req *pb.ListScopeVersionsRequest) (*pb.ListScopeVersionsResponse, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	pid, err := convert.ParseID(core.PrefixProgram, "program_id", req.GetProgramId())
	if err != nil {
		return nil, err
	}
	n, tok, err := convert.Page(req.GetPage())
	if err != nil {
		return nil, err
	}
	after := 0
	if tok != "" {
		if after, err = strconv.Atoi(tok); err != nil {
			return nil, convert.Invalid("page.page_token", "invalid page token")
		}
	}
	vs, err := s.o.Repo.ListVersions(ctx, id.Tenant, pid, int32(after), n+1)
	if err != nil {
		return nil, repoErr("program", err)
	}
	out := &pb.ListScopeVersionsResponse{}
	for i, v := range vs {
		if i == n {
			out.Page = convert.PageResp(strconv.Itoa(int(vs[n-1].Version)))
			break
		}
		out.Versions = append(out.Versions, VersionProto(v))
	}
	return out, nil
}

// SetKillSwitch implements ScopeService. Authorization is by level and
// fails closed (GAPS G-SEC-02; spec 13.6 "authorization is evaluated in
// each service from the internal token"):
//
//   - global: only the platform operator (platform.Identity.IsOperator);
//     every tenant member, owners included, and every other service is
//     PERMISSION_DENIED FORBIDDEN_ROLE, since the switch stops every
//     tenant's runs (spec 4.4, 18.2).
//   - tenant: a member (user or API key) holding programs:admin (admin or
//     owner, spec 13.6 "use kill switch"); target_id, when given, must be
//     the caller's tenant (NOT_FOUND otherwise, as for any foreign id).
//   - program: the same permission; target_id must be one of the caller's
//     programs (NOT_FOUND otherwise).
//
// Clearing a switch (enabled false) needs the same grant as setting it.
// An unknown level is INVALID_ARGUMENT.
func (s *Server) SetKillSwitch(ctx context.Context, req *pb.SetKillSwitchRequest) (*pb.KillSwitch, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	ks := KillSwitch{Level: req.GetLevel(), Enabled: req.GetEnabled(), Reason: req.GetReason(), SetBy: id.Actor, SetAt: s.o.Clock.Now().UTC()}
	var program core.UUID
	switch ks.Level {
	case LevelGlobal:
		if !id.IsOperator() {
			return nil, core.Errorf(core.KindPermissionDenied, core.ReasonForbiddenRole, "the global kill switch needs the platform operator")
		}
	case LevelTenant, LevelProgram:
		if !id.IsMember() || !auth.Can(id.Role, auth.PermProgramsAdmin) {
			return nil, core.Errorf(core.KindPermissionDenied, core.ReasonForbiddenRole, "a %s kill switch needs %s (admin or owner)", ks.Level, auth.PermProgramsAdmin)
		}
	default:
		return nil, convert.Invalid("level", "level must be global, tenant or program")
	}
	switch ks.Level {
	case LevelTenant:
		t, err := convert.OptionalID(core.PrefixTenant, "target_id", req.GetTargetId())
		if err != nil {
			return nil, err
		}
		if !t.IsZero() && t != id.Tenant {
			return nil, core.Errorf(core.KindNotFound, core.ReasonNotFound, "tenant not found")
		}
		ks.Target = id.Tenant
	case LevelProgram:
		if program, err = convert.ParseID(core.PrefixProgram, "target_id", req.GetTargetId()); err != nil {
			return nil, err
		}
		if _, err := s.o.Repo.GetProgram(ctx, id.Tenant, program); err != nil {
			return nil, repoErr("program", err)
		}
		ks.Target = program
	}
	// The event carries the caller's tenant, the nil tenant for the
	// operator. Consumers must not scope a global switch by it: the engine
	// cancels every tenant's runs at that level (services/engine
	// HandleScopeEvent).
	evs := []Event{{ID: s.newID(), Type: EventKillSwitchSet, Tenant: id.Tenant, Program: program, At: ks.SetAt,
		Payload: map[string]any{"level": ks.Level, "target_id": killProto(ks).TargetId, "enabled": ks.Enabled, "reason": ks.Reason}}}
	ks, err = s.o.Repo.SetKillSwitch(ctx, ks, evs)
	if err != nil {
		return nil, repoErr("kill switch", err)
	}
	// The log line is the audit record of who set it (spec 18.2): the
	// set_by column keeps only user uuids, so a service or operator actor
	// is visible only here.
	s.o.Logger.WarnContext(ctx, "scope.kill_switch", "level", ks.Level, "target_id", killProto(ks).TargetId, "enabled", ks.Enabled,
		"set_by", id.Actor, "tenant_id", id.TenantPublicID())
	return killProto(ks), nil
}

// GetKillSwitches implements ScopeService.
func (s *Server) GetKillSwitches(ctx context.Context, _ *pb.GetKillSwitchesRequest) (*pb.GetKillSwitchesResponse, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	kss, err := s.o.Repo.KillSwitches(ctx, id.Tenant)
	if err != nil {
		return nil, repoErr("kill switches", err)
	}
	out := &pb.GetKillSwitchesResponse{}
	for _, k := range kss {
		out.Switches = append(out.Switches, killProto(k))
	}
	return out, nil
}

// Check implements ScopeService: the hot path (spec 4.3). Every decision is
// recorded before it is returned; any internal failure is an error, which
// every caller treats as deny (fail closed, spec 2.4).
func (s *Server) Check(ctx context.Context, req *pb.CheckRequest) (*pb.Decision, error) {
	// Includes recording the decision: Check answers only after the decision
	// log accepted it (record fails closed), so callers wait for both.
	defer s.o.Metrics.ScopeCheck.Since(time.Now())
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := s.check(ctx, id, req)
	if err != nil {
		return nil, err
	}
	if err := s.record(ctx, []DecisionRecord{rec}); err != nil {
		return nil, err
	}
	return convert.DecisionProto(rec.Decision), nil
}

// CheckBatch implements ScopeService.
func (s *Server) CheckBatch(ctx context.Context, req *pb.CheckBatchRequest) (*pb.CheckBatchResponse, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	if len(req.GetRequests()) > 500 {
		return nil, convert.Invalid("requests", "at most 500 requests per batch")
	}
	recs := make([]DecisionRecord, 0, len(req.GetRequests()))
	for _, r := range req.GetRequests() {
		rec, err := s.check(ctx, id, r)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
	}
	if err := s.record(ctx, recs); err != nil {
		return nil, err
	}
	out := &pb.CheckBatchResponse{}
	for _, r := range recs {
		out.Decisions = append(out.Decisions, convert.DecisionProto(r.Decision))
	}
	return out, nil
}

func (s *Server) record(ctx context.Context, recs []DecisionRecord) error {
	if err := s.o.Repo.RecordDecisions(ctx, recs); err != nil {
		s.o.Logger.ErrorContext(ctx, "scope.record_failed", "error", err.Error())
		return &core.Error{Kind: core.KindUnavailable, Reason: core.ReasonUnavailable, Message: "decision log unavailable; denying", Err: err}
	}
	for _, r := range recs {
		d := r.Decision
		s.o.Metrics.ScopeDecisions.Inc(d.Result.String(), d.Reason)
		s.o.Logger.InfoContext(platform.WithLogFields(ctx, convert.ID(core.PrefixRun, r.RunID), r.EffectPath), "scope.decision",
			"decision", d.Result.String(), "reason", d.Reason, "rule_id", d.RuleID, "decision_id", d.ID,
			"program_id", convert.ID(core.PrefixProgram, r.Program), "scope_version", r.ScopeVersion, "class", r.Class.String())
	}
	return nil
}

func (s *Server) check(ctx context.Context, id platform.Identity, req *pb.CheckRequest) (rec DecisionRecord, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = core.Errorf(core.KindInternal, core.ReasonInternal, "scope check panicked: %v", r)
		}
	}()
	pid, err := convert.ParseID(core.PrefixProgram, "program_id", req.GetProgramId())
	if err != nil {
		return rec, err
	}
	run, err := convert.OptionalID(core.PrefixRun, "run_id", req.GetRunId())
	if err != nil {
		return rec, err
	}
	now := s.o.Clock.Now().UTC()
	version := req.GetScopeVersion()
	if version == 0 {
		// The version in effect at decision time, never a pending one
		// (spec 4.1 effective_from; G-CORE-01).
		if version, err = s.o.Repo.EffectiveVersion(ctx, id.Tenant, pid, now); err != nil {
			return rec, repoErr("program", err)
		}
		if version == 0 {
			return rec, core.Errorf(core.KindFailedPrecondition, core.ReasonValidationFailed, "program has no scope document in effect")
		}
	} else if _, err := s.o.Repo.GetProgram(ctx, id.Tenant, pid); err != nil {
		// The policy cache is keyed by program only: check tenancy first.
		return rec, repoErr("program", err)
	}
	c, err := s.policy(ctx, id.Tenant, pid, version)
	if err != nil {
		return rec, err
	}
	if c.eff.After(now) {
		// A pinned version that is not in effect yet cannot authorize
		// anything (fail closed); runs are only ever pinned to effective
		// versions, so this is a caller asking ahead of time.
		return rec, core.Errorf(core.KindFailedPrecondition, core.ReasonValidationFailed,
			"scope version %d is not in effect until %s", version, core.FormatTime(c.eff))
	}
	pol := c.pol
	kss, err := s.o.Repo.KillSwitches(ctx, id.Tenant)
	if err != nil {
		return rec, repoErr("kill switches", err)
	}
	_, killed := Engaged(kss, id.Tenant, pid)
	class := convert.ClassFrom(req.GetActivityClass())
	d := pol.Check(ctx, scope.Request{Target: req.GetTarget(), Kind: convert.TargetKind(req.GetKind()), Class: class,
		Now: now, KillSwitch: killed, SkipResolution: !req.GetResolve(),
		ResolvePassive: req.GetResolve()})
	did := s.newID()
	d.ID = core.PublicID(core.PrefixDecision, did)
	if d.DecidedAt.IsZero() {
		d.DecidedAt = s.o.Clock.Now().UTC()
	}
	return DecisionRecord{ID: did, Tenant: id.Tenant, Program: pid, ScopeVersion: version, RunID: run,
		EffectPath: req.GetEffectPath(), Class: class, Decision: d}, nil
}

// policy returns the compiled policy of (program, version), cached; the
// caller has already checked that the program belongs to the tenant.
func (s *Server) policy(ctx context.Context, tenant, program core.UUID, version int32) (compiled, error) {
	k := policyKey{program, version}
	s.mu.Lock()
	c, ok := s.policies[k]
	s.mu.Unlock()
	if ok {
		return c, nil
	}
	v, err := s.o.Repo.GetVersion(ctx, tenant, program, version)
	if err != nil {
		return compiled{}, repoErr("scope version", err)
	}
	p, err := scope.Compile(v.Document, scope.WithResolver(s.o.Resolver))
	if err != nil {
		return compiled{}, core.Errorf(core.KindInternal, core.ReasonInternal, "stored scope version %d does not compile: %v", version, err)
	}
	c = compiled{pol: p, eff: v.EffectiveFrom}
	s.mu.Lock()
	if len(s.policies) > 4096 {
		clear(s.policies)
	}
	s.policies[k] = c
	s.mu.Unlock()
	return c, nil
}

// ActivationLookback is how far back ActivationLoop looks on start, so a
// version that took effect while the service was down is still announced.
const ActivationLookback = 24 * time.Hour

// ActivateDue publishes scope.version_published for every future-dated
// version whose effective_from lies in (after, upTo], and returns the
// effective_from of the last one announced (after when none, or on an
// error, so the caller retries the whole window; re-announcing is
// harmless, see below). It pages
// through them by (effective_from, program, version), so any number of
// versions sharing one effective_from (round times such as midnight are
// common) are all announced (G-CORE-10). Event ids are derived from
// (program, version), so a repeated window re-emits the same ids and the
// outboxes and consumer inboxes drop the duplicates.
func (s *Server) ActivateDue(ctx context.Context, after, upTo time.Time) (time.Time, error) {
	const page = 100
	last := after
	var from *DueVersion
	for {
		due, err := s.o.Repo.DueVersions(ctx, after, from, upTo, page)
		if err != nil {
			return after, err
		}
		if len(due) == 0 {
			return last, nil
		}
		evs := make([]Event, 0, len(due))
		for _, d := range due {
			evs = append(evs, Event{ID: activationID(d.Program, d.Version), Type: EventVersionPublished, Tenant: d.Tenant, Program: d.Program,
				At: d.EffectiveFrom, Payload: map[string]any{"program_id": convert.ID(core.PrefixProgram, d.Program), "version": d.Version,
					"effective_from": core.FormatTime(d.EffectiveFrom)}})
		}
		if err := s.o.Repo.AppendEvents(ctx, evs); err != nil {
			return after, err
		}
		end := due[len(due)-1]
		last = end.EffectiveFrom
		if len(due) < page {
			return last, nil
		}
		from = &end
	}
}

// ActivationLoop calls ActivateDue every interval (default 10 s) until ctx
// ends, starting ActivationLookback in the past. Errors are logged and the
// window retried, so a storage outage delays activation notices but never
// loses them while the process runs.
func (s *Server) ActivationLoop(interval time.Duration) func(context.Context) error {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	return func(ctx context.Context) error {
		after := s.o.Clock.Now().UTC().Add(-ActivationLookback)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			next, err := s.ActivateDue(ctx, after, s.o.Clock.Now().UTC())
			if err != nil {
				s.o.Logger.WarnContext(ctx, "scope.activation_failed", "error", err.Error())
			}
			after = next
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
		}
	}
}

// activationID is a name-based UUID (version 8, RFC 9562 5.8) over
// (program, version): the same activation always gets the same event id.
func activationID(program core.UUID, version int32) core.UUID {
	h := sha256.Sum256([]byte("scope.version_effective\x00" + program.String() + "\x00" + strconv.Itoa(int(version))))
	var u core.UUID
	copy(u[:], h[:16])
	u[6] = (u[6] & 0x0f) | 0x80
	u[8] = (u[8] & 0x3f) | 0x80
	return u
}

// DecisionLog looks up recorded decisions; the egress proxy validates
// pinned decisions with it (in process, or over the scope schema).
type DecisionLog struct{ Repo Repo }

// Decision returns a recorded decision of tenant by its id (dec_... or a
// UUID).
func (l DecisionLog) Decision(ctx context.Context, tenant core.UUID, id string) (DecisionRecord, error) {
	u, err := core.ParsePublicID(core.PrefixDecision, id)
	if err != nil {
		return DecisionRecord{}, fmt.Errorf("%w: malformed decision id", ErrNotFound)
	}
	d, err := l.Repo.GetDecision(ctx, tenant, u)
	if err == nil && d.Decision.ID == "" {
		d.Decision.ID = core.PublicID(core.PrefixDecision, u)
	}
	return d, err
}
