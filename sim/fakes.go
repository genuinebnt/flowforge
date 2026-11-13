package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/core/netnorm"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
)

// hashResolver resolves every name to a stable address in 203.0.113.0/24
// derived from the name, so scope decisions are deterministic.
type hashResolver struct{}

func (hashResolver) LookupAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	return []netip.Addr{netip.AddrFrom4([4]byte{203, 0, 113, byte(1 + fnv32(host)%250)})}, nil
}

func fnv32(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

func fnv64(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return h.Sum64()
}

// errNotFound is returned by the fakes for unknown programs or versions.
var errNotFound = engine.ErrNotFound

// scopeService is the fake scope service (spec 4, 11.3): versioned scope
// documents per program, compiled with scope.Compile, and kill switches.
type scopeService struct {
	mu       sync.Mutex
	programs map[core.UUID]*scopeProgram
	kills    map[string]engine.KillSwitchState
	history  []ScopeVersion
}

type scopeProgram struct {
	tenant   core.UUID
	versions []*scope.Policy
}

func newScopeService() *scopeService {
	return &scopeService{programs: map[core.UUID]*scopeProgram{}, kills: map[string]engine.KillSwitchState{}}
}

// put publishes doc as the next version of the program.
func (s *scopeService) put(tenant, program core.UUID, doc scope.Document) (int32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.programs[program]
	if p == nil {
		p = &scopeProgram{tenant: tenant}
		s.programs[program] = p
	}
	doc.ProgramID = program.String()
	doc.Version = len(p.versions) + 1
	pol, err := scope.Compile(doc, scope.WithResolver(hashResolver{}))
	if err != nil {
		return 0, err
	}
	p.versions = append(p.versions, pol)
	s.history = append(s.history, ScopeVersion{Tenant: tenant.String(), Program: program.String(), Version: int32(doc.Version), Doc: doc})
	return int32(doc.Version), nil
}

func killKey(level string, id core.UUID) string {
	if level == engine.KillGlobal {
		return level
	}
	return level + ":" + id.String()
}

func (s *scopeService) setKill(level string, id core.UUID, engaged bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if engaged {
		s.kills[killKey(level, id)] = engine.KillSwitchState{Engaged: true, Level: level, Reason: "sim"}
	} else {
		delete(s.kills, killKey(level, id))
	}
}

func (s *scopeService) program(tenant, program core.UUID) (*scopeProgram, error) {
	p := s.programs[program]
	if p == nil || (!tenant.IsZero() && p.tenant != tenant) || len(p.versions) == 0 {
		return nil, errNotFound
	}
	return p, nil
}

func (s *scopeService) CurrentVersion(ctx context.Context, tenant, program core.UUID) (int32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.program(tenant, program)
	if err != nil {
		return 0, err
	}
	return int32(len(p.versions)), nil
}

func (s *scopeService) Policy(ctx context.Context, tenant, program core.UUID, version int32) (*scope.Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.program(tenant, program)
	if err != nil {
		return nil, err
	}
	if version < 1 || int(version) > len(p.versions) {
		return nil, errNotFound
	}
	return p.versions[version-1], nil
}

func (s *scopeService) killLocked(tenant, program core.UUID) engine.KillSwitchState {
	for _, k := range []string{engine.KillGlobal, killKey(engine.KillTenant, tenant), killKey(engine.KillProgram, program)} {
		if ks, ok := s.kills[k]; ok {
			return ks
		}
	}
	return engine.KillSwitchState{}
}

func (s *scopeService) Check(ctx context.Context, req engine.ScopeCheck) (scope.Decision, error) {
	s.mu.Lock()
	ks := s.killLocked(req.Tenant, req.Program)
	p, err := s.program(req.Tenant, req.Program)
	s.mu.Unlock()
	if err != nil {
		return scope.Decision{}, err
	}
	if req.ScopeVersion < 1 || int(req.ScopeVersion) > len(p.versions) {
		return scope.Decision{}, errNotFound
	}
	d := p.versions[req.ScopeVersion-1].Check(ctx, scope.Request{Target: req.Target, Kind: req.Kind, Class: req.Class, Now: req.Now, KillSwitch: ks.Engaged})
	d.ID = req.DecisionID
	return d, nil
}

func (s *scopeService) KillSwitch(ctx context.Context, tenant, program core.UUID) (engine.KillSwitchState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killLocked(tenant, program), nil
}

// docAllowing returns a scope document allowing the domains (and their
// subdomains) up to intrusive activity.
func docAllowing(domains ...string) scope.Document {
	doc := scope.Document{Activity: scope.Activity{MaxClass: core.ClassIntrusive, Windows: []scope.Window{}}}
	for i, d := range domains {
		doc.Rules = append(doc.Rules,
			scope.Rule{ID: fmt.Sprintf("w%d", i), Effect: scope.EffectAllow, Kind: scope.RuleWildcardDomain, Value: "*." + d},
			scope.Rule{ID: fmt.Sprintf("d%d", i), Effect: scope.EffectAllow, Kind: scope.RuleDomain, Value: d})
	}
	return doc
}

// toolRegistry is the fake tool registry (engine.tools).
type toolRegistry struct {
	specs map[string]engine.ToolSpec
}

func newToolRegistry(specs []engine.ToolSpec) *toolRegistry {
	r := &toolRegistry{specs: map[string]engine.ToolSpec{}}
	for _, s := range specs {
		s.SpecSHA256 = s.ComputeSHA256()
		r.specs[s.Name] = s
	}
	return r
}

func (r *toolRegistry) list() []engine.ToolSpec {
	out := make([]engine.ToolSpec, 0, len(r.specs))
	for _, s := range r.specs {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b engine.ToolSpec) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (r *toolRegistry) ListTools(ctx context.Context) ([]engine.ToolSpec, error) {
	return r.list(), nil
}

func (r *toolRegistry) ResolveTool(ctx context.Context, name string, sha [32]byte) (engine.ToolSpec, error) {
	if s, ok := r.specs[name]; ok && s.SpecSHA256 == sha {
		return s, nil
	}
	return engine.ToolSpec{}, errNotFound
}

// toolExec is the fake tool runner (spec 6.3). It records every request
// with its idempotency key, answers deterministically from the request
// (so a re-run with the same key returns the same result, as a real
// runner that de-duplicates by key does), and injects the profile's tool
// faults. Concurrent executions of one par_map round draw their faults
// from a generator seeded by (scenario seed, key, attempt, delivery), so
// the outcome does not depend on goroutine order.
type toolExec struct {
	c *cluster

	mu      sync.Mutex
	seq     uint64
	count   map[string]int       // deliveries per key and attempt
	firstAt map[string]time.Time // first delivery per key: the observations' observed_at
}

func newToolExec(c *cluster) *toolExec {
	return &toolExec{c: c, count: map[string]int{}, firstAt: map[string]time.Time{}}
}

// errToolTimeout is what a caller sees for a lost request or reply.
var errToolTimeout = context.DeadlineExceeded

func (x *toolExec) Execute(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
	a := actorOf(ctx)
	if a != nil && a.crashed.Load() {
		return engine.ToolResult{}, errCrashed
	}
	c := x.c
	spec := c.tools.specs[call.Tool]
	now := c.clock.Now()
	rec := ToolCall{
		At: now, Run: call.RunID.String(), Path: call.Path, Attempt: call.Attempt, Tool: call.Tool,
		Class: spec.EffectClass, Key: call.IdempotencyKey, Input: append(json.RawMessage(nil), call.Input...),
		ScopeVersion: call.ScopeVersion, DecisionIDs: slices.Clone(call.DecisionIDs),
	}
	if a != nil {
		rec.Actor = a.id
		if a.w != nil {
			rec.Worker = a.w.id
		}
	}
	if spec.InputType == "InScope<Host>" {
		var host string
		if json.Unmarshal(call.Input, &host) == nil {
			if t, err := scope.Normalize(host, scope.TargetHost); err == nil {
				rec.Target = t.String()
				program := "none"
				if !call.ProgramID.IsZero() {
					program = call.ProgramID.String()
				}
				tgt := t.String()
				if n := t.Name(); n != "" {
					tgt = netnorm.RegistrableDomain(n)
				}
				rec.LimitKey = limiter.HostKey(program, tgt)
			}
		}
	}

	x.mu.Lock()
	ck := fmt.Sprintf("%s/%d", call.IdempotencyKey, call.Attempt)
	n := x.count[ck]
	x.count[ck] = n + 1
	if _, ok := x.firstAt[call.IdempotencyKey]; !ok {
		x.firstAt[call.IdempotencyKey] = now
	}
	first := x.firstAt[call.IdempotencyKey]
	x.mu.Unlock()

	r := core.NewSplitMix64(c.seed ^ fnv64(ck) ^ uint64(n)<<48)
	prof := c.prof
	faulty := c.faulty()
	timeout := time.Duration(spec.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = prof.Timeout
	}
	lat := between(r, 20*time.Millisecond, 400*time.Millisecond)
	if faulty && chance(r, prof.SpikeP) {
		lat = between(r, prof.SpikeMin, prof.SpikeMax)
	}
	busy := func(d time.Duration) {
		if a != nil {
			a.addBusy(d)
		}
	}
	record := func(r ToolCall) {
		x.mu.Lock()
		x.seq++
		r.Seq = x.seq
		c.hist.ToolCalls = append(c.hist.ToolCalls, r)
		x.mu.Unlock()
	}
	if faulty {
		switch {
		case chance(r, prof.ToolFailP):
			record(rec)
			busy(lat / 4)
			return engine.ToolResult{}, &engine.ToolError{Code: "unavailable", Message: "sim: transient tool failure", Retryable: true}
		case chance(r, prof.ToolDropP):
			record(rec)
			busy(timeout)
			return engine.ToolResult{}, errToolTimeout
		}
	}
	rec.Delivered = true
	record(rec)
	if faulty && chance(r, prof.ToolDupP) {
		dup := rec
		dup.Duplicate = true
		record(dup)
	}
	if lat >= timeout {
		busy(timeout)
		return engine.ToolResult{}, errToolTimeout
	}
	busy(lat)
	res, err := toolOutput(call, first)
	if faulty && chance(r, prof.ToolLostReplyP) {
		busy(timeout)
		return engine.ToolResult{}, errToolTimeout
	}
	return res, err
}

// toolOutput is the deterministic behaviour of the simulated tools.
func toolOutput(call engine.ToolCall, first time.Time) (engine.ToolResult, error) {
	h := fnv64(call.RunID.String() + "/" + call.Path)
	out := func(v any) (engine.ToolResult, error) {
		b, err := json.Marshal(v)
		return engine.ToolResult{Output: b}, err
	}
	switch call.Tool {
	case toolResolve:
		var name string
		if err := json.Unmarshal(call.Input, &name); err != nil {
			return engine.ToolResult{}, &engine.ToolError{Code: "invalid_input", Message: err.Error()}
		}
		if fnv32(name)%5 == 0 {
			return out(nil)
		}
		ip, _ := hashResolver{}.LookupAddrs(context.Background(), name)
		obs, err := resolveObservation(name, ip[0], first)
		if err != nil {
			return engine.ToolResult{}, err
		}
		res, err := out(ip[0].String())
		res.Observations = []json.RawMessage{obs}
		return res, err
	case toolHTTPX:
		switch h % 4 {
		case 0:
			return out(nil)
		case 1:
			return out(404)
		}
		return out(200)
	case toolReserve:
		return out(int64(1 + h%997))
	case toolRelease:
		return out(nil)
	case toolCharge:
		var n int64
		if err := json.Unmarshal(call.Input, &n); err != nil {
			return engine.ToolResult{}, &engine.ToolError{Code: "invalid_input", Message: err.Error()}
		}
		if n%4 == 0 {
			return engine.ToolResult{}, &engine.ToolError{Code: "card_declined", Message: "sim: declined", Retryable: false}
		}
		return out(n * 10)
	case toolFlaky:
		var n int64
		if err := json.Unmarshal(call.Input, &n); err != nil {
			return engine.ToolResult{}, &engine.ToolError{Code: "invalid_input", Message: err.Error()}
		}
		if call.Attempt <= int(h%3) {
			return engine.ToolResult{}, &engine.ToolError{Code: "flaky", Message: "sim: try again", Retryable: true}
		}
		return out(n + 1)
	}
	return engine.ToolResult{}, &engine.ToolError{Code: "unknown_tool", Message: call.Tool}
}

// resolveObservation is the observation resolve reports: the name, its
// address and the A record between them.
func resolveObservation(name string, ip netip.Addr, at time.Time) (json.RawMessage, error) {
	o := oam.Observation{
		Tool: oam.Tool{Name: toolResolve, Version: "1.0.0"}, ObservedAt: at.UTC().Truncate(time.Millisecond), Confidence: 0.9,
		Entities: []oam.ObservedEntity{{Ref: "n", Asset: oam.FQDN{Name: name}}, {Ref: "a", Asset: oam.IPAddress{Address: ip.String(), IPVersion: "IPv4"}}},
		Edges: []oam.ObservedEdge{{From: "n", To: "a", Relation: oam.BasicDNSRelation{Label: "dns_record",
			Header: oam.DNSHeader{RRType: 1, Class: 1, TTL: 300}}}},
	}
	return json.Marshal(o)
}

// errUnavailable is the transient error of a faulted call.
var errUnavailable = core.Wrap(errors.New("sim: injected failure"), core.KindUnavailable, core.ReasonUnavailable)

// errTimeout is what a caller sees when its request or reply was lost.
var errTimeout = core.Wrap(context.DeadlineExceeded, core.KindDeadlineExceeded, core.ReasonDeadlineExceeded)

// errPartitioned is what a partitioned worker sees for store calls.
var errPartitioned = core.Wrap(errors.New("sim: partitioned from the store"), core.KindUnavailable, core.ReasonUnavailable)
