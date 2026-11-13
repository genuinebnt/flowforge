package sim_test

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/sim"
)

// The unit tests below feed the checker histories with one hand-made
// violation each and assert it is caught, so a clean simulation run means
// something. They start from a real fault-free history that has every
// feature they need (several leases per run, a saga that compensated,
// alerts, observations, scope-checked calls) and break it.

var (
	baseOnce sync.Once
	baseHist *sim.History
	baseErr  error
)

func hasType(r sim.RunRecord, typ string) bool {
	return slices.ContainsFunc(r.Log, func(e eventstore.Event) bool { return e.Type == typ })
}

func base(t *testing.T) *sim.History {
	t.Helper()
	baseOnce.Do(func() {
		for seed := uint64(1); seed <= 200; seed++ {
			r := sim.Run(sim.Scenario{Seed: seed, Profile: sim.ProfileNone})
			if r.Err != nil {
				baseErr = r.Err
				return
			}
			if !r.OK() {
				continue
			}
			h := r.History
			comp := slices.ContainsFunc(h.Runs, func(r sim.RunRecord) bool {
				return hasType(r, engine.EvCompensationStarted) && hasType(r, engine.EvCompensationFinished)
			})
			failed := slices.ContainsFunc(h.Runs, func(r sim.RunRecord) bool { return hasType(r, engine.EvRunFailed) })
			if comp && failed && len(h.Alerts) > 0 && len(h.Observations) > 0 && len(h.Graphs) > 0 {
				baseHist = h
				return
			}
		}
		baseErr = fmt.Errorf("no seed produced a history with every feature")
	})
	if baseErr != nil {
		t.Fatal(baseErr)
	}
	if v := sim.CheckInvariants(baseHist); len(v) != 0 {
		t.Fatalf("the base history is not clean: %v", v)
	}
	return clone(t, baseHist)
}

// clone deep-copies a history (JSON round trip; modules are shared).
func clone(t *testing.T, h *sim.History) *sim.History {
	t.Helper()
	b, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	var c sim.History
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	c.Modules = h.Modules
	for i := range c.ToolCalls {
		c.ToolCalls[i].Seq = h.ToolCalls[i].Seq
	}
	return &c
}

func wantViolation(t *testing.T, h *sim.History, inv, substr string) {
	t.Helper()
	vs := sim.CheckInvariants(h)
	for _, v := range vs {
		if v.Inv == inv && strings.Contains(v.Msg, substr) {
			return
		}
	}
	t.Fatalf("no %s violation containing %q; got %v", inv, substr, vs)
}

func runWith(t *testing.T, h *sim.History, pred func(sim.RunRecord) bool) *sim.RunRecord {
	t.Helper()
	for i := range h.Runs {
		if pred(h.Runs[i]) {
			return &h.Runs[i]
		}
	}
	t.Fatal("no run matches")
	return nil
}

func eventIndex(r *sim.RunRecord, typ string) int {
	return slices.IndexFunc(r.Log, func(e eventstore.Event) bool { return e.Type == typ })
}

func TestInv01_SeqGap(t *testing.T) {
	h := base(t)
	r := runWith(t, h, func(r sim.RunRecord) bool { return len(r.Log) > 5 })
	r.Log = slices.Delete(r.Log, 3, 4) // seq 4 is missing
	wantViolation(t, h, sim.Inv01, "has seq")
}

func TestInv01_CommitOverlap(t *testing.T) {
	h := base(t)
	c := h.Commits[len(h.Commits)-1]
	c.Op++
	h.Commits = append(h.Commits, c) // the same seqs committed twice
	wantViolation(t, h, sim.Inv01, "expected to start at")
}

func TestInv02_FenceTwoWriters(t *testing.T) {
	h := base(t)
	var c sim.Commit
	for _, x := range h.Commits {
		if !x.Fence.IsZero() {
			c = x
			break
		}
	}
	c.Actor += 1000
	c.Op = h.Commits[len(h.Commits)-1].Op + 1
	h.Commits = append(h.Commits, c)
	wantViolation(t, h, sim.Inv02, "two writers")
}

func TestInv02_FenceNotIncreasing(t *testing.T) {
	h := base(t)
	g := h.Leases[0]
	g.Op = h.Leases[len(h.Leases)-1].Op + 1
	g.Owner = "w-other"
	h.Leases = append(h.Leases, g) // an old fence granted again, later
	wantViolation(t, h, sim.Inv02, "granted twice")
	wantViolation(t, h, sim.Inv02, "does not follow")
}

func TestInv03_SupersededFenceCommits(t *testing.T) {
	h := base(t)
	byRun := map[string][]sim.LeaseGrant{}
	for _, g := range h.Leases {
		byRun[g.Run] = append(byRun[g.Run], g)
	}
	for _, c := range h.Commits {
		gs := byRun[c.Run]
		if c.Fence.IsZero() || len(gs) < 2 || c.Fence != gs[0].Fence {
			continue
		}
		c.Op = gs[len(gs)-1].Op + 1 // the first owner writes after the last grant
		h.Commits = append(h.Commits, c)
		wantViolation(t, h, sim.Inv03, "after fence")
		return
	}
	t.Fatal("no run with two leases")
}

func TestInv04_KeyChangedOnRerun(t *testing.T) {
	h := base(t)
	c := h.ToolCalls[0]
	c.Key = strings.Repeat("0", 64)
	h.ToolCalls = append(h.ToolCalls, c)
	wantViolation(t, h, sim.Inv04, "used key")
}

func TestInv04_TwoRecordedResults(t *testing.T) {
	h := base(t)
	r := runWith(t, h, func(r sim.RunRecord) bool { return hasType(r, engine.EvEffectCompleted) })
	i := eventIndex(r, engine.EvEffectCompleted)
	dup := r.Log[i]
	dup.Seq = uint64(len(r.Log) + 1)
	r.Log = append(r.Log, dup)
	wantViolation(t, h, sim.Inv04, "recorded results")
}

func TestInv05_ReplayDiverges(t *testing.T) {
	h := base(t)
	r := runWith(t, h, func(r sim.RunRecord) bool {
		return hasType(r, engine.EvRunSucceeded) && hasType(r, engine.EvEffectScheduled)
	})
	i := eventIndex(r, engine.EvEffectScheduled)
	var p map[string]any
	_ = json.Unmarshal(r.Log[i].Payload, &p)
	p["args_hash"] = strings.Repeat("ab", 32)
	r.Log[i].Payload, _ = json.Marshal(p)
	wantViolation(t, h, sim.Inv05, "replay asks for")
}

func TestInv05_OutputDiffers(t *testing.T) {
	h := base(t)
	r := runWith(t, h, func(r sim.RunRecord) bool { return hasType(r, engine.EvRunSucceeded) })
	i := eventIndex(r, engine.EvRunSucceeded)
	r.Log[i].Payload = json.RawMessage(`{"output":123456789}`)
	wantViolation(t, h, sim.Inv05, "replay output")
}

func TestInv06_CallWithoutAllow(t *testing.T) {
	h := base(t)
	for i, c := range h.ToolCalls {
		if c.Tool == "httpx" && c.Delivered {
			h.ToolCalls[i].Target = "evil.other.org"
			wantViolation(t, h, sim.Inv06, "without a prior allow decision")
			wantViolation(t, h, sim.Inv06, "out of scope")
			return
		}
	}
	t.Fatal("no httpx call")
}

func TestInv06_WrongScopeVersion(t *testing.T) {
	h := base(t)
	for i, c := range h.ToolCalls {
		if c.Tool == "httpx" && c.Delivered {
			h.ToolCalls[i].ScopeVersion = 77
			wantViolation(t, h, sim.Inv06, "under scope v77")
			return
		}
	}
	t.Fatal("no httpx call")
}

func TestInv07_NotCompensated(t *testing.T) {
	h := base(t)
	r := runWith(t, h, func(r sim.RunRecord) bool { return hasType(r, engine.EvCompensationStarted) })
	var kept []eventstore.Event
	for _, e := range r.Log {
		if !strings.HasPrefix(e.Type, "compensation_") {
			kept = append(kept, e)
		}
	}
	r.Log = kept
	wantViolation(t, h, sim.Inv07, "none ran")
}

func TestInv07_WrongOrder(t *testing.T) {
	h := base(t)
	r := runWith(t, h, func(r sim.RunRecord) bool {
		return hasType(r, engine.EvCompensationStarted) && strings.Contains(string(r.Log[len(r.Log)-2].Payload), `"completed"`)
	})
	var idx []int
	for i, c := range h.ToolCalls {
		if c.Run == r.ID && c.Tool == "release" && c.Delivered {
			idx = append(idx, i)
		}
	}
	if len(idx) < 2 {
		t.Skip("the base saga compensated a single step")
	}
	a, b := idx[0], idx[len(idx)-1]
	h.ToolCalls[a].Seq, h.ToolCalls[b].Seq = h.ToolCalls[b].Seq, h.ToolCalls[a].Seq
	wantViolation(t, h, sim.Inv07, "reverse order")
}

func TestInv07_CompletedWithFailedStep(t *testing.T) {
	h := base(t)
	r := runWith(t, h, func(r sim.RunRecord) bool { return hasType(r, engine.EvCompensationStep) })
	i := eventIndex(r, engine.EvCompensationStep)
	var p engine.CompensationStepPayload
	_ = json.Unmarshal(r.Log[i].Payload, &p)
	p.Result, p.Error = "failed", &engine.EffectError{Code: "cancelled", Message: "cancelled"}
	r.Log[i].Payload, _ = json.Marshal(p)
	wantViolation(t, h, sim.Inv07, "was cancelled")
}

func TestInv08_DuplicateAlert(t *testing.T) {
	h := base(t)
	for _, a := range h.Alerts {
		if !a.Suppressed {
			h.Alerts = append(h.Alerts, a) // a redelivery that slipped through the inbox
			wantViolation(t, h, sim.Inv08, "produced 2 alerts")
			return
		}
	}
	t.Fatal("no delivered alert")
}

func TestInv08_LostAlert(t *testing.T) {
	h := base(t)
	h.Alerts = h.Alerts[1:]
	wantViolation(t, h, sim.Inv08, "produced 0 alerts")
}

func TestInv08_ThrottleWindow(t *testing.T) {
	h := base(t)
	for _, a := range h.Alerts {
		if a.Rule == "alr_ops_run_failed" && !a.Suppressed {
			b := a
			b.At = a.At.Add(time.Minute)
			b.TriggerEventID = "other"
			h.Alerts = append(h.Alerts, b)
			wantViolation(t, h, sim.Inv08, "twice within")
			return
		}
	}
	t.Fatal("no run-failed alert")
}

func TestInv09_GraphDiverges(t *testing.T) {
	h := base(t)
	h.Observations = h.Observations[1:] // the graph holds an observation nobody recorded
	wantViolation(t, h, sim.Inv09, "differs from the canonical application")
}

func TestInv09_ConflictingContent(t *testing.T) {
	h := base(t)
	o := h.Observations[0]
	o.Raw = json.RawMessage(strings.Replace(string(o.Raw), `"confidence":0.9`, `"confidence":0.5`, 1))
	h.Observations = append(h.Observations, o)
	wantViolation(t, h, sim.Inv09, "two different contents")
}

func TestInv10_RateExceeded(t *testing.T) {
	h := base(t)
	r := h.Runs[0]
	run := core.MustParseUUID(r.ID)
	at := h.Start.Add(time.Hour)
	key := "rl:h:test:acme.com"
	for i := range 12 {
		path := fmt.Sprintf("99/%d", i)
		h.Admissions = append(h.Admissions, sim.Admission{At: at, Actor: 9999, Key: key})
		h.EdgeAdmissions = append(h.EdgeAdmissions, sim.Admission{At: at, Key: key})
		h.ToolCalls = append(h.ToolCalls, sim.ToolCall{At: at, Actor: 9999, Run: r.ID, Path: path, Attempt: 1, Tool: "probe",
			Class: "read", Key: engine.IdempotencyKey(run, path), LimitKey: key, Delivered: true})
	}
	wantViolation(t, h, sim.Inv10, "5 requests within 0s at the edge")
}

func TestInv10_NotAdmitted(t *testing.T) {
	h := base(t)
	r := h.Runs[0]
	run := core.MustParseUUID(r.ID)
	h.ToolCalls = append(h.ToolCalls, sim.ToolCall{At: h.Start, Actor: 9998, Run: r.ID, Path: "98", Attempt: 1, Tool: "probe",
		Class: "read", Key: engine.IdempotencyKey(run, "98"), LimitKey: "rl:h:test:acme.com", Delivered: true})
	wantViolation(t, h, sim.Inv10, "never admitted")
}

func TestInv11_Liveness(t *testing.T) {
	h := base(t)
	r := runWith(t, h, func(r sim.RunRecord) bool { return hasType(r, engine.EvRunSucceeded) })
	r.Log = r.Log[:len(r.Log)-1]
	wantViolation(t, h, sim.Inv11, "not terminal")
}

func TestInv11_TooLate(t *testing.T) {
	h := base(t)
	h.LivenessBound = time.Nanosecond
	h.FaultsEnd = h.Start
	wantViolation(t, h, sim.Inv11, "after the liveness bound")
}
