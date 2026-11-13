package sim_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/sim"
)

// TestRegress_G_ENG_09_Inv10AtTheEdge: inv_10 is measured at the network
// edge with no allowance for dispatch delay. Twelve calls admitted by the
// engine at its rate (2/s) but delivered together, which the old checker
// excused as dispatch delay (4 + 2*(0 s + 5.5 s) = 15 >= 12), now break
// the bound even though the edge admitted each one; a delivery without an
// edge admission is a violation of its own.
func TestRegress_G_ENG_09_Inv10AtTheEdge(t *testing.T) {
	h := base(t)
	r := h.Runs[0]
	run := core.MustParseUUID(r.ID)
	start := h.Start.Add(2 * time.Hour)
	arrive := start.Add(6 * time.Second)
	key := "rl:h:test:acme.com"
	for i := range 12 {
		path := fmt.Sprintf("97/%d", i)
		h.Admissions = append(h.Admissions, sim.Admission{At: start.Add(time.Duration(i) * 500 * time.Millisecond), Actor: 9997, Key: key})
		h.EdgeAdmissions = append(h.EdgeAdmissions, sim.Admission{At: arrive, Key: key})
		h.ToolCalls = append(h.ToolCalls, sim.ToolCall{At: arrive, Actor: 9997, Run: r.ID, Path: path, Attempt: 1, Tool: "probe",
			Class: "read", Key: engine.IdempotencyKey(run, path), LimitKey: key, Delivered: true})
	}
	wantViolation(t, h, sim.Inv10, "5 requests within 0s at the edge")

	h = base(t)
	h.Admissions = append(h.Admissions, sim.Admission{At: h.Start, Actor: 9996, Key: key})
	h.ToolCalls = append(h.ToolCalls, sim.ToolCall{At: h.Start, Actor: 9996, Run: r.ID, Path: "96", Attempt: 1, Tool: "probe",
		Class: "read", Key: engine.IdempotencyKey(run, "96"), LimitKey: key, Delivered: true})
	wantViolation(t, h, sim.Inv10, "without passing the edge's limiter")
}

// TestRegress_G_ENG_09_DeliveredUnderKillSwitch: a target-facing call
// delivered while a kill switch covers its program violates inv_06; one
// delivered at the instant the switch is released does not.
func TestRegress_G_ENG_09_DeliveredUnderKillSwitch(t *testing.T) {
	h := base(t)
	r := h.Runs[0]
	run := core.MustParseUUID(r.ID)
	on := h.Start.Add(3 * time.Hour)
	h.KillSwitches = append(h.KillSwitches,
		sim.KillSwitchChange{At: on, Level: engine.KillProgram, ID: r.Program, Engaged: true},
		sim.KillSwitchChange{At: on.Add(time.Minute), Level: engine.KillProgram, ID: r.Program, Engaged: false})
	call := sim.ToolCall{At: on.Add(time.Minute), Actor: 9995, Run: r.ID, Path: "95", Attempt: 1, Tool: "resolve",
		Class: "passive", Key: engine.IdempotencyKey(run, "95"), Delivered: true}
	h.ToolCalls = append(h.ToolCalls, call)
	if vs := sim.CheckInvariants(h); len(vs) != 0 {
		t.Fatalf("a call at the release instant: %v", vs)
	}
	call.At, call.Path, call.Key = on.Add(30*time.Second), "94", engine.IdempotencyKey(run, "94")
	h.ToolCalls = append(h.ToolCalls, call)
	wantViolation(t, h, sim.Inv06, "while a kill switch was engaged")
}

// TestRegress_G_ENG_09_EdgeWorkload: under faults the edge workload hits
// the edge's per-target limit and races kill switches; every invariant
// holds, each race is a call the edge refused (never delivered), and the
// engine reports races by cancelling with kill_switch_race.
func TestRegress_G_ENG_09_EdgeWorkload(t *testing.T) {
	denials, races, raceCancels := 0, 0, 0
	for seed := uint64(1); seed <= 20 && (races == 0 || denials == 0); seed++ {
		rep := sim.Run(sim.Scenario{Seed: seed, Profile: sim.ProfileChaos, Workload: sim.WorkloadEdge})
		if !rep.OK() {
			t.Fatalf("%s\n%v", rep.Summary(), rep.Violations)
		}
		h := rep.History
		denials += h.Stats.EdgeDenials
		races += h.Stats.KillSwitchRaces
		refused := 0
		for _, c := range h.ToolCalls {
			if c.EdgeRefused == "kill_switch" {
				refused++
				if c.Delivered {
					t.Fatalf("seed %d: a refused call was delivered: %+v", seed, c)
				}
			}
		}
		if refused != h.Stats.KillSwitchRaces {
			t.Fatalf("seed %d: %d refused calls, %d races", seed, refused, h.Stats.KillSwitchRaces)
		}
		for _, r := range h.Runs {
			for _, e := range r.Log {
				if e.Type == engine.EvRunCancelled && strings.Contains(string(e.Payload), `"kill_switch_race`) {
					raceCancels++
				}
			}
		}
	}
	if denials == 0 || races == 0 || raceCancels == 0 {
		t.Fatalf("the edge workload exercised nothing: %d edge denials, %d races, %d race cancellations", denials, races, raceCancels)
	}
}
