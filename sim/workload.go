package sim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/scope"
)

// Workload names.
const (
	WorkloadMixed       = "mixed"
	WorkloadLinear      = "linear"
	WorkloadFanout      = "fanout"
	WorkloadSaga        = "saga"
	WorkloadRetry       = "retry"
	WorkloadCancel      = "cancel"
	WorkloadKillSwitch  = "killswitch"
	WorkloadScopeChange = "scopechange"
	// WorkloadEdge exercises the network edge (G-ENG-09): many concurrent
	// fan-outs probe one registrable domain of program B, so the edge's
	// per-target limit is contended, and a kill switch on program B is
	// flipped on and off without the operator's cancel call, so calls the
	// engine re-checked before a flip reach the edge after it (kill-switch
	// races).
	WorkloadEdge = "edge"
)

// WorkloadNames lists the workloads in a fixed order.
var WorkloadNames = []string{WorkloadMixed, WorkloadLinear, WorkloadFanout, WorkloadSaga, WorkloadRetry,
	WorkloadCancel, WorkloadKillSwitch, WorkloadScopeChange, WorkloadEdge}

// workload is what the simulated clients and operators do: which
// workflows they submit (with idempotency keys, some submitted twice),
// and whether they cancel runs, flip a kill switch or publish a new scope
// version mid-run.
type workload struct {
	kinds       []string
	runs        int
	cancel      bool
	killSwitch  bool
	scopeChange bool
	edge        bool
}

func lookupWorkload(name string) (workload, error) {
	switch name {
	case WorkloadMixed:
		return workload{kinds: []string{wfScan, wfFanout, wfSaga, wfRetry, wfLongrun}, runs: 20, cancel: true, killSwitch: true, scopeChange: true}, nil
	case WorkloadLinear:
		return workload{kinds: []string{wfScan}, runs: 12}, nil
	case WorkloadFanout:
		return workload{kinds: []string{wfFanout}, runs: 10}, nil
	case WorkloadSaga:
		return workload{kinds: []string{wfSaga}, runs: 12}, nil
	case WorkloadRetry:
		return workload{kinds: []string{wfRetry}, runs: 12}, nil
	case WorkloadCancel:
		return workload{kinds: []string{wfLongrun, wfSaga}, runs: 10, cancel: true}, nil
	case WorkloadKillSwitch:
		return workload{kinds: []string{wfLongrun, wfScan}, runs: 10, killSwitch: true}, nil
	case WorkloadScopeChange:
		return workload{kinds: []string{wfLongrun, wfFanout}, runs: 10, scopeChange: true}, nil
	case WorkloadEdge:
		return workload{kinds: []string{wfFanout}, runs: 16, edge: true}, nil
	}
	return workload{}, fmt.Errorf("sim: unknown workload %q (want one of %v)", name, WorkloadNames)
}

var (
	acmeNames    = []string{"www.acme.com", "api.acme.com", "mail.acme.com", "vpn.acme.com", "dev.acme.com"}
	exampleNames = []string{"shop.example.com", "beta.example.com"}
	outNames     = []string{"evil.other.org"}
)

func (c *cluster) pickNames(pool []string, lo, hi int) []string {
	p := slices.Clone(pool)
	core.Shuffle(c.rng, len(p), func(i, j int) { p[i], p[j] = p[j], p[i] })
	n := lo + core.IntN(c.rng, hi-lo+1)
	return p[:min(n, len(p))]
}

// input returns the program and JSON input of a submission.
func (c *cluster) input(kind string, wl workload) (core.UUID, json.RawMessage) {
	all := slices.Concat(acmeNames, exampleNames, outNames)
	var v any
	program := programA
	switch kind {
	case wfScan:
		v = map[string]any{"roots": c.pickNames(all, 2, 4)}
	case wfFanout:
		if wl.edge {
			program = programB
			v = map[string]any{"names": c.pickNames(acmeNames, 3, 5)}
		} else {
			v = map[string]any{"names": c.pickNames(all, 3, 6)}
		}
	case wfSaga:
		v = map[string]any{"amount": 1 + core.IntN(c.rng, 40)}
	case wfRetry:
		v = map[string]any{"n": core.IntN(c.rng, 100)}
	case wfLongrun:
		if wl.killSwitch && (!wl.scopeChange || core.IntN(c.rng, 2) == 0) {
			program = programB
			v = map[string]any{"names": c.pickNames(acmeNames, 2, 4)}
		} else {
			v = map[string]any{"names": c.pickNames(slices.Concat(acmeNames, exampleNames), 2, 4)}
		}
	}
	b, _ := json.Marshal(v)
	return program, b
}

// plan schedules the workload's client and operator actions.
func (c *cluster) plan(wl workload) {
	d := c.sc.Duration
	n := wl.runs
	if c.sc.Runs > 0 {
		n = c.sc.Runs
	}
	for i := range n {
		kind := wl.kinds[core.IntN(c.rng, len(wl.kinds))]
		program, input := c.input(kind, wl)
		req := engine.SubmitRunRequest{WorkflowName: kind, ProgramID: program, Input: input, IdempotencyKey: fmt.Sprintf("sub-%03d", i)}
		at := between(c.rng, 0, d*6/10)
		c.s.after(at, func() { c.submit(req, wl, 0) })
		if chance(c.rng, 0.3) { // a client that resends its request
			c.s.after(at+between(c.rng, time.Second, 90*time.Second), func() { c.submit(req, wl, 0) })
		}
	}
	if wl.killSwitch {
		c.s.after(d*4/10, func() {
			c.setKill(engine.KillProgram, programB, true)
			c.control("kill switch", func(ctx context.Context) error {
				return c.api.HandleKillSwitch(ctx, engine.KillProgram, programB)
			}, 0)
		})
		c.s.after(d*4/10+3*time.Minute, func() { c.setKill(engine.KillProgram, programB, false) })
	}
	if wl.edge {
		// Short kill-switch pulses while the fan-outs run; the engine
		// learns of each at its next advance, the edge at once.
		for at := d / 10; at < d*7/10; at += between(c.rng, time.Minute, 3*time.Minute) {
			on := between(c.rng, 2*time.Second, 20*time.Second)
			c.s.after(at, func() { c.setKill(engine.KillProgram, programB, true) })
			c.s.after(at+on, func() { c.setKill(engine.KillProgram, programB, false) })
		}
	}
	if wl.scopeChange {
		c.s.after(d*3/10, func() { c.publishScope(docAllowing("acme.com")) })
		c.s.after(d*3/10+4*time.Minute, func() { c.publishScope(docAllowing("acme.com", "example.com")) })
	}
	c.s.after(20*time.Second, c.approverTick)
	c.s.after(time.Minute, c.operatorTick)
}

// setKill engages or releases a kill switch and records the change.
func (c *cluster) setKill(level string, id core.UUID, engaged bool) {
	c.scope.setKill(level, id, engaged)
	ch := KillSwitchChange{At: c.clock.Now(), Level: level, Engaged: engaged}
	if level != engine.KillGlobal {
		ch.ID = id.String()
	}
	c.hist.KillSwitches = append(c.hist.KillSwitches, ch)
}

func (c *cluster) publishScope(doc scope.Document) {
	v, err := c.scope.put(tenantA, programA, doc)
	if err != nil {
		return
	}
	c.control(fmt.Sprintf("scope v%d", v), func(ctx context.Context) error {
		return c.api.HandleScopeChanged(ctx, tenantA, programA, v)
	}, 0)
}

// control runs an operator action, retrying it while it fails.
func (c *cluster) control(name string, f func(ctx context.Context) error, attempt int) {
	c.pendingClient++
	c.apiCall(name, func(ctx context.Context) {
		err := f(ctx)
		if err != nil && attempt < 10 {
			c.s.after(between(c.rng, 2*time.Second, 15*time.Second), func() { c.control(name, f, attempt+1) })
		}
		c.pendingClient--
	})
}

func permanent(err error) bool {
	var ce *core.Error
	if errors.As(err, &ce) {
		switch ce.Reason {
		case core.ReasonKillSwitchActive, core.ReasonIdempotencyKeyReused, core.ReasonRunNotCancellable,
			core.ReasonApprovalAlreadyDecided, core.ReasonValidationFailed, core.ReasonScopeDenied, core.ReasonNotFound:
			return true
		}
		return ce.Kind == core.KindInvalidArgument || ce.Kind == core.KindPermissionDenied || ce.Kind == core.KindNotFound
	}
	return false
}

// submit is one SubmitRun call of a client that retries on failure with
// the same idempotency key.
func (c *cluster) submit(req engine.SubmitRunRequest, wl workload, attempt int) {
	c.pendingClient++
	c.apiCall("submit "+req.IdempotencyKey, func(ctx context.Context) {
		defer func() { c.pendingClient-- }()
		run, err := c.api.SubmitRun(ctx, clientC, req)
		sub := Submission{At: c.clock.Now(), Key: req.IdempotencyKey, Workflow: req.WorkflowName}
		if err != nil {
			sub.Err = err.Error()
			c.hist.Submissions = append(c.hist.Submissions, sub)
			if !permanent(err) && attempt < 10 {
				c.s.after(between(c.rng, 2*time.Second, 20*time.Second), func() { c.submit(req, wl, attempt+1) })
			}
			return
		}
		sub.Run = run.ID.String()
		c.hist.Submissions = append(c.hist.Submissions, sub)
		if wl.cancel && req.WorkflowName != wfScan && !c.cancelled[run.ID] && chance(c.rng, 0.4) {
			c.cancelled[run.ID] = true
			id := run.ID
			c.s.after(between(c.rng, 10*time.Second, 3*time.Minute), func() {
				c.control("cancel", func(ctx context.Context) error {
					_, err := c.api.CancelRun(ctx, clientC, id, "sim: client cancel")
					if permanent(err) {
						return nil
					}
					return err
				}, 0)
			})
		}
	})
}

// approverTick is the simulated approver: every pending approval is
// decided after a delay, mostly approved, sometimes denied.
func (c *cluster) approverTick() {
	as, _ := c.repo.ListApprovals(c.ctx, tenantA, engine.ApprovalFilter{Status: engine.ApprovalPending}, core.Nil, 1000)
	for _, a := range as {
		if c.deciding[a.ID] {
			continue
		}
		c.deciding[a.ID] = true
		id := a.ID
		decision := engine.DecisionApprove
		if chance(c.rng, 0.25) {
			decision = engine.DecisionDeny
		}
		c.s.after(between(c.rng, 10*time.Second, 5*time.Minute), func() {
			c.control("decide", func(ctx context.Context) error {
				_, err := c.api.DecideApproval(ctx, approverC, id, decision, "sim approver")
				if permanent(err) {
					return nil
				}
				return err
			}, 0)
		})
	}
	c.s.after(20*time.Second, c.approverTick)
}

// operatorTick is the simulated operator: an effect whose outcome is
// unknown after a crash (a non-idempotent call cut by the crash) is
// resolved with retry, which re-runs it with the same idempotency key.
func (c *cluster) operatorTick() {
	runs, _ := c.repo.ListActiveRuns(c.ctx, tenantA, core.Nil)
	for _, r := range runs {
		id := r.ID
		c.apiCall("operator", func(ctx context.Context) {
			run, err := c.api.GetRun(ctx, operatorC, id)
			if err != nil || run.WaitingOn == nil || run.WaitingOn.Kind != "operator" || run.WaitingOn.Path == "" {
				return
			}
			_, _ = c.api.ResolveUnknownEffect(ctx, operatorC, id, run.WaitingOn.Path, engine.ResolveRetry, nil)
		})
	}
	c.s.after(time.Minute, c.operatorTick)
}
