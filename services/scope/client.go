package scope

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// Client is the client side of ScopeService. It implements engine.Scope
// (so the engine calls the scope service over gRPC) and the per-target
// checks of the tool runner, egress proxy and similarity service. Policies
// are cached per (program, version) (versions are immutable); kill switch
// states for KillSwitchTTL (Invalidate drops them when scope.events
// arrive). Every error is reported as an error, which callers treat as deny.
type Client struct {
	C pb.ScopeServiceClient
	// Service is the calling service's name, used for its service identity.
	Service string
	// KillSwitchTTL defaults to one second.
	KillSwitchTTL time.Duration

	mu       sync.Mutex
	policies map[policyKey]*scope.Policy
	kills    map[core.UUID]killEntry
}

type killEntry struct {
	at  time.Time
	kss []*pb.KillSwitch
}

var _ engine.Scope = (*Client)(nil)

// NewClient returns a client.
func NewClient(c pb.ScopeServiceClient, service string) *Client {
	return &Client{C: c, Service: service, policies: map[policyKey]*scope.Policy{}, kills: map[core.UUID]killEntry{}}
}

func notFound(err error) error {
	if errors.Is(err, core.ErrNotFound) {
		return fmt.Errorf("%w: %v", engine.ErrNotFound, err)
	}
	return err
}

// CurrentVersion implements engine.Scope: the version in effect now
// (GetScope version 0), which is what a run is pinned to at submission. It
// is not Program.current_version, the latest published version, which may
// be future-dated (spec 4.1 effective_from; G-CORE-01).
func (c *Client) CurrentVersion(ctx context.Context, tenant, program core.UUID) (int32, error) {
	v, err := c.C.GetScope(platform.AsService(ctx, tenant, c.Service), &pb.GetScopeRequest{ProgramId: convert.ID(core.PrefixProgram, program)})
	if err != nil {
		return 0, notFound(err)
	}
	if v.GetVersion() == 0 {
		return 0, fmt.Errorf("%w: program has no scope document", engine.ErrNotFound)
	}
	return v.GetVersion(), nil
}

// Policy implements engine.Scope: the version's document compiled locally
// (no resolver: the engine uses it for passive answers and max_class only).
func (c *Client) Policy(ctx context.Context, tenant, program core.UUID, version int32) (*scope.Policy, error) {
	k := policyKey{program, version}
	c.mu.Lock()
	p := c.policies[k]
	c.mu.Unlock()
	if p != nil && version > 0 {
		return p, nil
	}
	v, err := c.C.GetScope(platform.AsService(ctx, tenant, c.Service), &pb.GetScopeRequest{ProgramId: convert.ID(core.PrefixProgram, program), Version: version})
	if err != nil {
		return nil, notFound(err)
	}
	doc, err := convert.ScopeDoc(v.GetDocument())
	if err != nil {
		return nil, err
	}
	doc.ProgramID, doc.Version = program.String(), int(v.GetVersion())
	p, err = scope.Compile(doc)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if len(c.policies) > 4096 {
		clear(c.policies)
	}
	c.policies[policyKey{program, v.GetVersion()}] = p
	c.mu.Unlock()
	return p, nil
}

// Check implements engine.Scope. Engine checks always resolve and pin.
func (c *Client) Check(ctx context.Context, req engine.ScopeCheck) (scope.Decision, error) {
	return c.CheckTarget(ctx, Target{Tenant: req.Tenant, Program: req.Program, Version: req.ScopeVersion, Target: req.Target,
		Kind: req.Kind, Class: req.Class, Run: req.RunID, Path: req.Path, Resolve: true})
}

// Target is one target check.
type Target struct {
	Tenant, Program core.UUID
	Version         int32 // 0: current
	Target          string
	Kind            scope.TargetKind
	Class           core.ActivityClass
	Run             core.UUID
	Path            string
	Resolve         bool
}

// CheckTarget asks the scope service for one decision.
func (c *Client) CheckTarget(ctx context.Context, t Target) (scope.Decision, error) {
	d, err := c.C.Check(platform.AsService(ctx, t.Tenant, c.Service), &pb.CheckRequest{
		ProgramId: convert.ID(core.PrefixProgram, t.Program), ScopeVersion: t.Version, Kind: convert.TargetKindProto(t.Kind),
		Target: t.Target, ActivityClass: convert.Class(t.Class), RunId: convert.ID(core.PrefixRun, t.Run), EffectPath: t.Path, Resolve: t.Resolve,
	})
	if err != nil {
		return scope.Decision{Result: scope.ResultDeny}, notFound(err)
	}
	return convert.Decision(d), nil
}

// CheckBatch decides several targets in one call.
func (c *Client) CheckBatch(ctx context.Context, ts []Target) ([]scope.Decision, error) {
	if len(ts) == 0 {
		return nil, nil
	}
	req := &pb.CheckBatchRequest{}
	for _, t := range ts {
		req.Requests = append(req.Requests, &pb.CheckRequest{ProgramId: convert.ID(core.PrefixProgram, t.Program), ScopeVersion: t.Version,
			Kind: convert.TargetKindProto(t.Kind), Target: t.Target, ActivityClass: convert.Class(t.Class),
			RunId: convert.ID(core.PrefixRun, t.Run), EffectPath: t.Path, Resolve: t.Resolve})
	}
	resp, err := c.C.CheckBatch(platform.AsService(ctx, ts[0].Tenant, c.Service), req)
	if err != nil {
		return nil, err
	}
	if len(resp.GetDecisions()) != len(ts) {
		return nil, errors.New("scope: CheckBatch returned the wrong number of decisions")
	}
	out := make([]scope.Decision, len(ts))
	for i, d := range resp.GetDecisions() {
		out[i] = convert.Decision(d)
	}
	return out, nil
}

// KillSwitch implements engine.Scope.
func (c *Client) KillSwitch(ctx context.Context, tenant, program core.UUID) (engine.KillSwitchState, error) {
	ttl := c.KillSwitchTTL
	if ttl <= 0 {
		ttl = time.Second
	}
	c.mu.Lock()
	e, ok := c.kills[tenant]
	c.mu.Unlock()
	if !ok || time.Since(e.at) > ttl {
		resp, err := c.C.GetKillSwitches(platform.AsService(ctx, tenant, c.Service), &pb.GetKillSwitchesRequest{})
		if err != nil {
			return engine.KillSwitchState{}, err
		}
		e = killEntry{at: time.Now(), kss: resp.GetSwitches()}
		c.mu.Lock()
		c.kills[tenant] = e
		c.mu.Unlock()
	}
	var kss []KillSwitch
	for _, k := range e.kss {
		ks := KillSwitch{Level: k.GetLevel(), Enabled: k.GetEnabled(), Reason: k.GetReason()}
		switch ks.Level {
		case LevelTenant:
			ks.Target, _ = core.ParsePublicID(core.PrefixTenant, k.GetTargetId())
		case LevelProgram:
			ks.Target, _ = core.ParsePublicID(core.PrefixProgram, k.GetTargetId())
		}
		kss = append(kss, ks)
	}
	if k, ok := Engaged(kss, tenant, program); ok {
		return engine.KillSwitchState{Engaged: true, Level: k.Level, Reason: k.Reason}, nil
	}
	return engine.KillSwitchState{}, nil
}

// Invalidate drops cached kill switch states (on scope.events).
func (c *Client) Invalidate() {
	c.mu.Lock()
	clear(c.kills)
	c.mu.Unlock()
}
