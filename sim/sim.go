package sim

import (
	"fmt"
	"time"
)

// Scenario is one simulation: (seed, fault profile, workload) plus the
// size of the cluster and how long faults are injected (spec 25.7).
type Scenario struct {
	Seed     uint64
	Profile  string // a ProfileNames entry; default none
	Workload string // a WorkloadNames entry; default mixed
	// Workers is the number of worker processes (default 3).
	Workers int
	// Duration is the simulated time during which faults are injected and
	// the workload submits (default 20 minutes).
	Duration time.Duration
	// LivenessBound is how long after Duration every run must have
	// reached a terminal state (default 2 hours, inv_11).
	LivenessBound time.Duration
	// Runs overrides the workload's number of submissions.
	Runs int
}

func (s Scenario) withDefaults() Scenario {
	if s.Profile == "" {
		s.Profile = ProfileNone
	}
	if s.Workload == "" {
		s.Workload = WorkloadMixed
	}
	if s.Workers <= 0 {
		s.Workers = 3
	}
	if s.Duration <= 0 {
		s.Duration = 20 * time.Minute
	}
	if s.LivenessBound <= 0 {
		s.LivenessBound = 2 * time.Hour
	}
	return s
}

// Report is the outcome of a scenario.
type Report struct {
	Scenario   Scenario
	History    *History
	Violations []Violation
	// Hash identifies the history; the same scenario always yields the
	// same hash.
	Hash string
	// Err is set when the scenario could not be set up (it is not a
	// violation).
	Err error
}

// OK reports whether the scenario ran and no invariant was violated.
func (r Report) OK() bool { return r.Err == nil && len(r.Violations) == 0 }

// Repro returns the commands that replay the scenario.
func (r Report) Repro() string {
	s := r.Scenario
	return fmt.Sprintf("go test ./sim -run TestSimProfiles -sim.seed=%d -sim.profile=%s -sim.workload=%s\n"+
		"go run ./cmd/xtask sim --profile %s --workload %s --seed %d", s.Seed, s.Profile, s.Workload, s.Profile, s.Workload, s.Seed)
}

// Summary is a one-line description of the outcome.
func (r Report) Summary() string {
	s := r.Scenario
	if r.Err != nil {
		return fmt.Sprintf("seed=%d profile=%s workload=%s: setup error: %v", s.Seed, s.Profile, s.Workload, r.Err)
	}
	st := r.History.Stats
	return fmt.Sprintf("seed=%d profile=%s workload=%s runs=%d (ok %d, failed %d, cancelled %d) events=%d crashes=%d partitions=%d fenced=%d tool_reruns=%d pub_dups=%d violations=%d",
		s.Seed, s.Profile, s.Workload, st.Runs, st.Succeeded, st.Failed, st.Cancelled, st.Events, st.Crashes, st.Partitions,
		st.FencedAppends, st.ToolReruns, st.PubDuplicates, len(r.Violations))
}

// Run executes a scenario on a deterministic single-process cluster and
// checks the invariants of spec 25.7 on its history. The same scenario
// always produces the same history.
func Run(sc Scenario) Report {
	sc = sc.withDefaults()
	rep := Report{Scenario: sc}
	wl, err := lookupWorkload(sc.Workload)
	if err != nil {
		rep.Err = err
		return rep
	}
	c, err := newCluster(sc)
	if err != nil {
		rep.Err = err
		return rep
	}
	for range sc.Workers {
		c.startWorker()
	}
	c.scheduleFaults()
	c.plan(wl)
	limit := c.faultsEnd.Add(sc.LivenessBound)
	for c.s.next(limit) {
		if c.clock.Now().After(c.faultsEnd) && c.pendingClient == 0 && c.nonterminal == 0 && c.pendingPub == 0 && c.settled() {
			break
		}
	}
	// Pub/Sub keeps redelivering until every message arrived.
	for c.pendingPub > 0 && c.s.next(limit.Add(time.Hour)) {
	}
	c.s.shutdown()
	c.finish()
	rep.History = c.hist
	rep.Violations = CheckInvariants(c.hist)
	rep.Hash = c.hist.Hash()
	return rep
}
