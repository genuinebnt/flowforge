package enginetest

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/scope"
)

// ErrScopeUnavailable is returned by MemScope.Check while Unavailable is
// set.
var ErrScopeUnavailable = errors.New("enginetest: scope service unavailable")

// MemScope is the in-memory engine.Scope: programs with versioned scope
// documents compiled with scope.Compile, kill switches, and a decision
// log. It is safe for concurrent use.
type MemScope struct {
	mu       sync.Mutex
	programs map[core.UUID]*memProgram
	kills    map[string]engine.KillSwitchState
	resolver scope.Resolver
	// Decisions records every decision Check returned.
	Decisions []scope.Decision
	// Unavailable makes Check fail (the engine must deny).
	Unavailable bool
}

type memProgram struct {
	tenant   core.UUID
	versions []*scope.Policy
}

var _ engine.Scope = (*MemScope)(nil)

// FixedResolver resolves every name to Addr.
type FixedResolver struct{ Addr netip.Addr }

// LookupAddrs implements scope.Resolver.
func (r FixedResolver) LookupAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	return []netip.Addr{r.Addr}, ctx.Err()
}

// NewMemScope returns a scope service whose active checks resolve names
// with resolver (nil: every name resolves to 203.0.113.10).
func NewMemScope(resolver scope.Resolver) *MemScope {
	if resolver == nil {
		resolver = FixedResolver{Addr: netip.MustParseAddr("203.0.113.10")}
	}
	return &MemScope{programs: map[core.UUID]*memProgram{}, kills: map[string]engine.KillSwitchState{}, resolver: resolver}
}

// PutScope publishes doc as the next version of the tenant's program
// (creating the program) and returns the version.
func (s *MemScope) PutScope(tenant, program core.UUID, doc scope.Document) (int32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.programs[program]
	if p == nil {
		p = &memProgram{tenant: tenant}
		s.programs[program] = p
	}
	if p.tenant != tenant {
		return 0, fmt.Errorf("enginetest: program %s belongs to another tenant", program)
	}
	doc.ProgramID = program.String()
	doc.Version = len(p.versions) + 1
	pol, err := scope.Compile(doc, scope.WithResolver(s.resolver))
	if err != nil {
		return 0, err
	}
	p.versions = append(p.versions, pol)
	return int32(doc.Version), nil
}

// SetKillSwitch engages or releases a kill switch (level global, tenant
// or program; id is the tenant or program).
func (s *MemScope) SetKillSwitch(level string, id core.UUID, engaged bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := level + ":" + id.String()
	if level == engine.KillGlobal {
		k = level
	}
	if engaged {
		s.kills[k] = engine.KillSwitchState{Engaged: true, Level: level, Reason: "test"}
	} else {
		delete(s.kills, k)
	}
}

func (s *MemScope) program(tenant, program core.UUID) (*memProgram, error) {
	p := s.programs[program]
	if p == nil || (!tenant.IsZero() && p.tenant != tenant) || len(p.versions) == 0 {
		return nil, engine.ErrNotFound
	}
	return p, nil
}

// CurrentVersion implements engine.Scope.
func (s *MemScope) CurrentVersion(ctx context.Context, tenant, program core.UUID) (int32, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.program(tenant, program)
	if err != nil {
		return 0, err
	}
	return int32(len(p.versions)), nil
}

// Policy implements engine.Scope.
func (s *MemScope) Policy(ctx context.Context, tenant, program core.UUID, version int32) (*scope.Policy, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.program(tenant, program)
	if err != nil {
		return nil, err
	}
	if version < 1 || int(version) > len(p.versions) {
		return nil, engine.ErrNotFound
	}
	return p.versions[version-1], nil
}

func (s *MemScope) killLocked(tenant, program core.UUID) engine.KillSwitchState {
	for _, k := range []string{engine.KillGlobal, engine.KillTenant + ":" + tenant.String(), engine.KillProgram + ":" + program.String()} {
		if ks, ok := s.kills[k]; ok {
			return ks
		}
	}
	return engine.KillSwitchState{}
}

// Check implements engine.Scope with scope.Policy.Check.
func (s *MemScope) Check(ctx context.Context, req engine.ScopeCheck) (scope.Decision, error) {
	s.mu.Lock()
	unavailable := s.Unavailable
	ks := s.killLocked(req.Tenant, req.Program)
	p, err := s.program(req.Tenant, req.Program)
	s.mu.Unlock()
	if unavailable {
		return scope.Decision{}, ErrScopeUnavailable
	}
	if err != nil {
		return scope.Decision{}, err
	}
	if req.ScopeVersion < 1 || int(req.ScopeVersion) > len(p.versions) {
		return scope.Decision{}, engine.ErrNotFound
	}
	pol := p.versions[req.ScopeVersion-1]
	d := pol.Check(ctx, scope.Request{Target: req.Target, Kind: req.Kind, Class: req.Class, Now: req.Now, KillSwitch: ks.Engaged})
	d.ID = req.DecisionID
	s.mu.Lock()
	s.Decisions = append(s.Decisions, d)
	s.mu.Unlock()
	return d, nil
}

// KillSwitch implements engine.Scope.
func (s *MemScope) KillSwitch(ctx context.Context, tenant, program core.UUID) (engine.KillSwitchState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.killLocked(tenant, program), nil
}

// DocAllowing returns a scope document allowing the given wildcard
// domains (each "*.x" also allows x itself through a domain rule) for
// activity up to max.
func DocAllowing(max core.ActivityClass, domains ...string) scope.Document {
	doc := scope.Document{Activity: scope.Activity{MaxClass: max, Windows: []scope.Window{}}}
	for i, d := range domains {
		doc.Rules = append(doc.Rules,
			scope.Rule{ID: fmt.Sprintf("w%d", i), Effect: scope.EffectAllow, Kind: scope.RuleWildcardDomain, Value: "*." + d},
			scope.Rule{ID: fmt.Sprintf("d%d", i), Effect: scope.EffectAllow, Kind: scope.RuleDomain, Value: d},
		)
	}
	return doc
}
