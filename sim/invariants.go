package sim

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/scope"
)

// Invariant ids (spec 25.7).
const (
	Inv01 = "inv_01" // per-run seq contiguity
	Inv02 = "inv_02" // one writer per fence, fences increase
	Inv03 = "inv_03" // no commit from a superseded fence
	Inv04 = "inv_04" // one recorded result per effect; re-runs keep the idempotency key
	Inv05 = "inv_05" // replay reproduces terminal state and effect order
	Inv06 = "inv_06" // no active call without a prior allow decision
	Inv07 = "inv_07" // failed sagas compensated in reverse or marked for an operator
	Inv08 = "inv_08" // exactly one delivered alert per rule and dedupe window
	Inv09 = "inv_09" // graph equals the recorded observations applied once each
	Inv10 = "inv_10" // per-target rate never above the ceiling
	Inv11 = "inv_11" // liveness after faults stop
)

// CheckInvariants returns every violation of inv_01..inv_11 in h, sorted.
// It is a pure function of the history.
func CheckInvariants(h *History) []Violation {
	var out []Violation
	add := func(inv, run, format string, args ...any) {
		out = append(out, Violation{Inv: inv, Run: run, Msg: fmt.Sprintf(format, args...)})
	}
	logs := map[string]*parsedLog{}
	for i := range h.Runs {
		r := &h.Runs[i]
		l := parseLog(r.Log)
		logs[r.ID] = l
		for _, b := range l.bad {
			add(Inv05, r.ID, "undecodable event: %s", b)
		}
	}
	checkSeq(h, add)
	checkFences(h, add)
	checkEffects(h, logs, add)
	comps := map[string]int{}
	for i := range h.Runs {
		r := &h.Runs[i]
		n, errs := replayRun(h, r, logs[r.ID])
		comps[r.ID] = n
		for _, e := range errs {
			add(Inv05, r.ID, "%s", e)
		}
	}
	checkScope(h, logs, add)
	checkKillSwitches(h, add)
	checkSagas(h, logs, comps, add)
	checkAlerts(h, add)
	checkGraph(h, logs, add)
	checkRate(h, add)
	checkLiveness(h, logs, add)
	slices.SortStableFunc(out, func(a, b Violation) int {
		return cmp.Or(strings.Compare(a.Inv, b.Inv), strings.Compare(a.Run, b.Run), strings.Compare(a.Msg, b.Msg))
	})
	return slices.CompactFunc(out, func(a, b Violation) bool { return a == b })
}

type addFn func(inv, run, format string, args ...any)

// inv_01: the log is seq 1..n without gaps or duplicates, and the store's
// commits tile it exactly.
func checkSeq(h *History, add addFn) {
	commits := map[string][]Commit{}
	for _, c := range h.Commits {
		commits[c.Run] = append(commits[c.Run], c)
	}
	for _, r := range h.Runs {
		for i, e := range r.Log {
			if e.Seq != uint64(i+1) {
				add(Inv01, r.ID, "event %d has seq %d", i+1, e.Seq)
				break
			}
		}
		cs := commits[r.ID]
		slices.SortFunc(cs, func(a, b Commit) int { return cmp.Compare(a.Op, b.Op) })
		next := uint64(1)
		for _, c := range cs {
			if c.FromSeq != next || c.ToSeq < c.FromSeq {
				add(Inv01, r.ID, "commit op %d covers seq %d..%d, expected to start at %d", c.Op, c.FromSeq, c.ToSeq, next)
			}
			next = c.ToSeq + 1
		}
		if len(cs) > 0 && next-1 != uint64(len(r.Log)) {
			add(Inv01, r.ID, "commits end at seq %d, the log at %d", next-1, len(r.Log))
		}
	}
}

// inv_02 and inv_03 over the store's linearization.
func checkFences(h *History, add addFn) {
	type key struct {
		run   string
		fence eventstore.Fence
	}
	grants := map[string][]LeaseGrant{}
	owner := map[key]string{}
	for _, g := range h.Leases {
		k := key{g.Run, g.Fence}
		if o, ok := owner[k]; ok {
			add(Inv02, g.Run, "fence %s granted twice (to %s and %s)", g.Fence, o, g.Owner)
		}
		owner[k] = g.Owner
		if gs := grants[g.Run]; len(gs) > 0 {
			if last := gs[len(gs)-1]; !last.Fence.Less(g.Fence) || g.Op < last.Op || g.At.Before(last.At) {
				add(Inv02, g.Run, "fence %s (op %d) does not follow fence %s (op %d)", g.Fence, g.Op, last.Fence, last.Op)
			}
		}
		grants[g.Run] = append(grants[g.Run], g)
	}
	writers := map[key]uint64{}
	for _, c := range h.Commits {
		if c.Fence.IsZero() {
			if c.FromSeq != 1 {
				add(Inv02, c.Run, "unfenced commit of seq %d..%d", c.FromSeq, c.ToSeq)
			}
			continue
		}
		k := key{c.Run, c.Fence}
		o, ok := owner[k]
		switch {
		case !ok:
			add(Inv02, c.Run, "commit op %d with fence %s that was never granted", c.Op, c.Fence)
		case o != c.Owner:
			add(Inv02, c.Run, "commit op %d with fence %s by %s, the fence belongs to %s", c.Op, c.Fence, c.Owner, o)
		}
		if w, ok := writers[k]; ok && w != c.Actor {
			add(Inv02, c.Run, "fence %s has two writers (actors %d and %d)", c.Fence, w, c.Actor)
		}
		writers[k] = c.Actor
		for _, g := range grants[c.Run] {
			if g.Op < c.Op && c.Fence.Less(g.Fence) {
				add(Inv03, c.Run, "commit op %d (seq %d..%d) with fence %s after fence %s was granted at op %d",
					c.Op, c.FromSeq, c.ToSeq, c.Fence, g.Fence, g.Op)
				break
			}
		}
	}
}

// inv_04: at most one recorded result per effect; the executor re-ran a
// call only with the effect's idempotency key; a result recorded for a
// tool call came from the tool. Also: one run per submission key.
func checkEffects(h *History, logs map[string]*parsedLog, add addFn) {
	for _, r := range h.Runs {
		l := logs[r.ID]
		for _, p := range l.order {
			if e := l.effects[p]; e.terminals > 1 {
				add(Inv04, r.ID, "effect %s has %d recorded results", p, e.terminals)
			}
		}
	}
	keyOwner := map[string]string{}
	delivered := map[string]bool{}
	for _, c := range h.ToolCalls {
		run, err := core.ParseUUID(c.Run)
		if err != nil {
			add(Inv04, c.Run, "tool call with malformed run id")
			continue
		}
		if want := engine.IdempotencyKey(run, c.Path); c.Key != want {
			add(Inv04, c.Run, "call of %s at %s attempt %d used key %s, want %s", c.Tool, c.Path, c.Attempt, short(c.Key), short(want))
		}
		rp := c.Run + "/" + c.Path
		if o, ok := keyOwner[c.Key]; ok && o != rp {
			add(Inv04, c.Run, "idempotency key %s used by %s and %s", short(c.Key), o, rp)
		}
		keyOwner[c.Key] = rp
		if c.Delivered {
			delivered[rp] = true
		}
	}
	for _, r := range h.Runs {
		l := logs[r.ID]
		for _, p := range l.order {
			e := l.effects[p]
			if e.kind == "tool_call" && e.done && !e.failed && !e.resolved && !delivered[r.ID+"/"+p] {
				add(Inv04, r.ID, "effect %s (%s) has a recorded result but the tool never ran", p, e.tool)
			}
		}
	}
	runsByKey := map[string]string{}
	for _, r := range h.Runs {
		k := r.Tenant + "/" + r.IdempotencyKey
		if o, ok := runsByKey[k]; ok {
			add(Inv04, r.ID, "submission key %s created runs %s and %s", r.IdempotencyKey, o, r.ID)
		}
		runsByKey[k] = r.ID
	}
	answered := map[string]string{}
	for _, s := range h.Submissions {
		if s.Run == "" {
			continue
		}
		if o, ok := answered[s.Key]; ok && o != s.Run {
			add(Inv04, s.Run, "submission key %s answered with runs %s and %s", s.Key, o, s.Run)
		}
		answered[s.Key] = s.Run
	}
}

// inv_06: every delivered call of an active or intrusive tool names a
// target for which the run recorded an allow decision under the call's
// scope version before the call started, and the scope document of that
// version (an independent oracle) does allow the target for the tool's
// class.
func checkScope(h *History, logs map[string]*parsedLog, add addFn) {
	type pv struct {
		program string
		version int32
	}
	policies := map[pv]*scope.Policy{}
	for _, s := range h.Scopes {
		if p, err := scope.Compile(s.Doc, scope.WithResolver(hashResolver{})); err == nil {
			policies[pv{s.Program, s.Version}] = p
		}
	}
	programOf := map[string]string{}
	for _, r := range h.Runs {
		programOf[r.ID] = r.Program
	}
	// committedAt[run][seq] is the store time the event was committed: a
	// decision is "prior" to a call if it was committed before the call
	// reached the executor (a call re-executed after a crash reuses the
	// original effect_started, so seq order alone is not enough).
	committedAt := map[string]map[uint64]time.Time{}
	for _, c := range h.Commits {
		m := committedAt[c.Run]
		if m == nil {
			m = map[uint64]time.Time{}
			committedAt[c.Run] = m
		}
		for s := c.FromSeq; s <= c.ToSeq; s++ {
			m[s] = c.At
		}
	}
	prior := func(run string, seq uint64, at time.Time) bool {
		t, ok := committedAt[run][seq]
		return ok && !t.After(at)
	}
	for _, c := range h.ToolCalls {
		class, err := core.ParseActivityClass(c.Class)
		if err != nil || class < core.ClassActive || !c.Delivered {
			continue
		}
		if c.Target == "" {
			add(Inv06, c.Run, "%s call at %s carries no scope-checked target", c.Tool, c.Path)
			continue
		}
		l := logs[c.Run]
		if l == nil {
			add(Inv06, c.Run, "%s call at %s for a run that does not exist", c.Tool, c.Path)
			continue
		}
		e := l.effects[c.Path]
		var startSeq uint64
		if e != nil {
			startSeq = e.started[c.Attempt]
		}
		if startSeq == 0 {
			add(Inv06, c.Run, "%s call at %s attempt %d has no effect_started", c.Tool, c.Path, c.Attempt)
			continue
		}
		ok := false
		for _, d := range l.decisions {
			if prior(c.Run, d.seq, c.At) && d.decision.Allowed() && d.decision.Target == c.Target && int32(d.decision.ScopeVersion) == c.ScopeVersion {
				ok = true
				break
			}
		}
		if !ok {
			add(Inv06, c.Run, "%s call of %s at %s under scope v%d without a prior allow decision", c.Tool, c.Target, c.Path, c.ScopeVersion)
		}
		for _, id := range c.DecisionIDs {
			found := false
			for _, d := range l.decisions {
				if d.decision.ID == id && prior(c.Run, d.seq, c.At) {
					found = d.decision.Allowed()
				}
			}
			if !found {
				add(Inv06, c.Run, "%s call at %s cites decision %s, which the run did not record as an allow", c.Tool, c.Path, id)
			}
		}
		pol := policies[pv{programOf[c.Run], c.ScopeVersion}]
		if pol == nil {
			add(Inv06, c.Run, "%s call at %s under unknown scope version %d", c.Tool, c.Path, c.ScopeVersion)
			continue
		}
		d := pol.Check(context.Background(), scope.Request{Target: c.Target, Kind: scope.TargetHost, Class: class, Now: c.At})
		if !d.Allowed() {
			add(Inv06, c.Run, "%s call of %s at %s is out of scope v%d: %s", c.Tool, c.Target, c.Path, c.ScopeVersion, d.Reason)
		}
	}
}

// inv_07: a failed or cancelled run that had registered compensations
// ran all of them, newest first, and finished with completed, or is
// marked for an operator (compensation_finished failed). The undo calls
// the executor saw are the forward results in reverse order.
func checkSagas(h *History, logs map[string]*parsedLog, comps map[string]int, add addFn) {
	for _, r := range h.Runs {
		l := logs[r.ID]
		if l.terminal != engine.EvRunFailed && l.terminal != engine.EvRunCancelled {
			continue
		}
		c := l.comp
		if c.started == nil {
			if comps[r.ID] > 0 {
				add(Inv07, r.ID, "run ended %s with %d registered compensations and none ran", l.terminal, comps[r.ID])
			}
			continue
		}
		if c.finished == nil {
			add(Inv07, r.ID, "compensation started but never finished")
			continue
		}
		allOK := len(c.steps) == c.started.Count
		for i, s := range c.steps {
			if s.Index != i {
				add(Inv07, r.ID, "compensation step %d recorded as index %d", i, s.Index)
			}
			if s.Result != "ok" {
				allOK = false
				if s.Error != nil && s.Error.Code == "cancelled" {
					// Compensations run to the end: nothing may cancel one.
					add(Inv07, r.ID, "compensation step %d was cancelled: %s", i, s.Error.Message)
				}
			}
		}
		if n := comps[r.ID]; n != c.started.Count && c.finished.Outcome != engine.CompensationFailed {
			add(Inv07, r.ID, "compensation_started counts %d, the program registered %d", c.started.Count, n)
		}
		switch {
		case c.finished.Outcome == engine.CompensationCompleted && !allOK:
			add(Inv07, r.ID, "compensation recorded completed but %d/%d steps succeeded", len(c.steps), c.started.Count)
		case c.finished.Outcome != engine.CompensationCompleted && c.finished.Outcome != engine.CompensationFailed:
			add(Inv07, r.ID, "compensation finished with %q", c.finished.Outcome)
		}
		if c.finished.Outcome != engine.CompensationCompleted {
			continue // marked for an operator
		}
		// Reverse order at the executor.
		for _, pair := range h.Compensations {
			var forward []string
			for _, p := range l.order {
				e := l.effects[p]
				if e.tool == pair.Forward && e.done && !e.failed && !strings.HasPrefix(p, l.compPrefix()) {
					forward = append(forward, string(canonical(e.result)))
				}
			}
			type undo struct {
				seq   uint64
				input string
			}
			first := map[string]undo{}
			for _, tc := range h.ToolCalls {
				if tc.Run != r.ID || tc.Tool != pair.Undo || !tc.Delivered || !strings.HasPrefix(tc.Path, l.compPrefix()) {
					continue
				}
				if u, ok := first[tc.Path]; !ok || tc.Seq < u.seq {
					first[tc.Path] = undo{tc.Seq, string(canonical(tc.Input))}
				}
			}
			us := make([]undo, 0, len(first))
			for _, u := range first {
				us = append(us, u)
			}
			slices.SortFunc(us, func(a, b undo) int { return cmp.Compare(a.seq, b.seq) })
			want := slices.Clone(forward)
			slices.Reverse(want)
			j := 0
			for _, u := range us {
				for j < len(want) && want[j] != u.input {
					j++
				}
				if j == len(want) {
					add(Inv07, r.ID, "%s calls %v are not the %s results %v in reverse order", pair.Undo, us, pair.Forward, forward)
					break
				}
				j++
			}
			if len(us) != len(want) {
				add(Inv07, r.ID, "compensation completed but %s ran for %d of %d %s results", pair.Undo, len(us), len(want), pair.Forward)
			}
		}
	}
}

func canonical(b json.RawMessage) []byte {
	if c, err := core.CanonicalJSON(b); err == nil {
		return c
	}
	return b
}

// inv_08: every alert-worthy event (a run event a rule matches) produced
// exactly one alert record per rule, delivered unless an earlier delivery
// of the same dedupe key is within the rule's throttle window, and no two
// deliveries of one key fall in one window, however often the transport
// redelivered the event.
func checkAlerts(h *History, add addFn) {
	type ev struct {
		run string
		seq uint64
	}
	for _, rule := range h.AlertRules {
		want := map[ev]bool{}
		for _, r := range h.Runs {
			for _, e := range r.Log {
				if e.Type == rule.EventType {
					want[ev{r.ID, e.Seq}] = true
				}
			}
		}
		got := map[ev]int{}
		delivered := map[string][]time.Time{}
		for _, a := range h.Alerts {
			if a.Rule != rule.ID {
				continue
			}
			k := ev{a.Run, a.Seq}
			got[k]++
			if !want[k] {
				add(Inv08, a.Run, "rule %s alerted on seq %d, which is not a %s event", rule.ID, a.Seq, rule.EventType)
			}
			if a.Suppressed && rule.Throttle == 0 {
				add(Inv08, a.Run, "rule %s suppressed the alert of seq %d without a throttle", rule.ID, a.Seq)
			}
			if !a.Suppressed {
				delivered[a.DedupeKey] = append(delivered[a.DedupeKey], a.At)
			}
		}
		for k := range want {
			if n := got[k]; n != 1 {
				add(Inv08, k.run, "rule %s produced %d alerts for seq %d (want exactly 1)", rule.ID, n, k.seq)
			}
		}
		if rule.Throttle > 0 {
			for key, ts := range delivered {
				slices.SortFunc(ts, time.Time.Compare)
				for i := 1; i < len(ts); i++ {
					if ts[i].Sub(ts[i-1]) < rule.Throttle {
						add(Inv08, "", "rule %s delivered key %s twice within %s", rule.ID, short(key), rule.Throttle)
					}
				}
			}
		}
	}
}

// inv_09: the graph after the simulation equals the recorded
// observations applied once each, in observation id order, to an empty
// graph; every observation a run recorded was accepted by the ingester.
func checkGraph(h *History, logs map[string]*parsedLog, add addFn) {
	obs := slices.Clone(h.Observations)
	slices.SortStableFunc(obs, func(a, b Observation) int { return strings.Compare(a.ID, b.ID) })
	byID := map[string]Observation{}
	var once []Observation
	for _, o := range obs {
		if prev, ok := byID[o.ID]; ok {
			if string(prev.Raw) != string(o.Raw) {
				add(Inv09, o.Run, "observation %s was accepted with two different contents", o.ID)
			}
			continue
		}
		byID[o.ID] = o
		once = append(once, o)
	}
	for _, r := range h.Runs {
		for _, id := range logs[r.ID].observed {
			if _, ok := byID[id]; !ok {
				add(Inv09, r.ID, "observation %s is recorded in the log but the ingester never accepted it", id)
			}
		}
	}
	ref := graphstore.NewMem(graphstore.Config{Clock: frozenClock{}})
	c := &cluster{ctx: context.Background()}
	for _, o := range once {
		c.applyObservation(ref, o)
	}
	want := graphStates(context.Background(), ref, once)
	if len(want) != len(h.Graphs) {
		add(Inv09, "", "%d programs in the graph, %d expected", len(h.Graphs), len(want))
		return
	}
	for i := range want {
		if want[i] != h.Graphs[i] {
			add(Inv09, "", "graph of program %s differs from the canonical application of its %d observations", want[i].Program, len(once))
		}
	}
}

// inv_10, measured at the network edge (G-ENG-09): per rl:h key,
// (a) every request the executor received was admitted by the engine's
// limiter (matched in order per actor; transport duplicates are one
// request there), (b) every request delivered to the tool, duplicates
// included, passed the edge's limiter at its arrival time, and (c) in
// every window of delivery times the count stays within the GCRA bound
// burst + rate*window. The bound has no allowance for dispatch delay: the
// edge admits at arrival, so how long a call spent between the engine's
// admission and the edge does not matter.
func checkRate(h *History, add addFn) {
	type ak struct {
		actor uint64
		key   string
	}
	adm := map[ak][]time.Time{}
	for _, a := range h.Admissions {
		k := ak{a.Actor, a.Key}
		adm[k] = append(adm[k], a.At)
	}
	type ek struct {
		key string
		at  time.Time
	}
	edge := map[ek]int{}
	for _, a := range h.EdgeAdmissions {
		edge[ek{a.Key, a.At}]++
	}
	calls := slices.Clone(h.ToolCalls)
	slices.SortStableFunc(calls, compareCalls)
	byKey := map[string][]time.Time{}
	used := map[ak]int{}
	for _, c := range calls {
		if c.LimitKey == "" {
			continue
		}
		if !c.Duplicate {
			k := ak{c.Actor, c.LimitKey}
			i := used[k]
			if i >= len(adm[k]) {
				add(Inv10, c.Run, "%s call at %s (%s) was never admitted by the limiter", c.Tool, c.Path, c.LimitKey)
				continue
			}
			used[k] = i + 1
		}
		if !c.Delivered {
			continue
		}
		if e := (ek{c.LimitKey, c.At}); edge[e] > 0 {
			edge[e]--
		} else {
			add(Inv10, c.Run, "%s call at %s (%s) reached the tool without passing the edge's limiter", c.Tool, c.Path, c.LimitKey)
		}
		byKey[c.LimitKey] = append(byKey[c.LimitKey], c.At)
	}
	ceiling := func(key string) (RateCeiling, bool) {
		for _, rc := range h.RateCeilings {
			if strings.HasPrefix(key, rc.Prefix) {
				return rc, true
			}
		}
		return RateCeiling{}, false
	}
	keys := make([]string, 0, len(byKey))
	for k := range byKey {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, key := range keys {
		rc, ok := ceiling(key)
		if !ok {
			continue
		}
		as := byKey[key]
		slices.SortFunc(as, time.Time.Compare)
		// The densest windows start and end at arrivals, so checking every
		// pair (i, j) covers every window.
		for i := range as {
			for j := i; j < len(as); j++ {
				w := as[j].Sub(as[i])
				if limit := float64(rc.Burst) + rc.Rate*w.Seconds() + 1e-9; float64(j-i+1) > limit {
					add(Inv10, "", "%s: %d requests within %s at the edge, ceiling %d+%.1f/s", key, j-i+1, w, rc.Burst, rc.Rate)
					return
				}
			}
		}
	}
}

// inv_06, kill switches: no target-facing call (passive and above) was
// delivered while a kill switch covering its run's tenant or program was
// engaged throughout the call's instant (an engaged kill switch turns
// every decision into a deny, spec 4.3). Calls the edge refused in such a
// race are History.KillSwitchRaces, reported but not violations.
func checkKillSwitches(h *History, add addFn) {
	if len(h.KillSwitches) == 0 {
		return
	}
	runs := map[string]RunRecord{}
	for _, r := range h.Runs {
		runs[r.ID] = r
	}
	// engaged reports whether a switch covering (tenant, program) is on
	// after the changes with At < t (strict) or At <= t (!strict).
	engaged := func(tenant, program string, t time.Time, strict bool) bool {
		on := map[string]bool{}
		for _, k := range h.KillSwitches {
			if k.At.After(t) || (strict && k.At.Equal(t)) {
				break
			}
			on[k.Level+":"+k.ID] = k.Engaged
		}
		return on[engine.KillGlobal+":"] || on[engine.KillTenant+":"+tenant] || on[engine.KillProgram+":"+program]
	}
	for _, c := range h.ToolCalls {
		cls, err := core.ParseActivityClass(c.Class)
		if err != nil || cls == core.ClassNone || !c.Delivered {
			continue
		}
		r := runs[c.Run]
		if engaged(r.Tenant, r.Program, c.At, true) && engaged(r.Tenant, r.Program, c.At, false) {
			add(Inv06, c.Run, "%s call at %s was delivered at %s while a kill switch was engaged", c.Tool, c.Path, c.At.Format(time.RFC3339Nano))
		}
	}
}

// inv_11: every run is terminal, and became terminal at most
// LivenessBound after faults stopped, unless the workload left it open.
func checkLiveness(h *History, logs map[string]*parsedLog, add addFn) {
	deadline := h.FaultsEnd.Add(h.LivenessBound)
	committed := map[string]time.Time{} // run -> store time of its last commit
	for _, c := range h.Commits {
		committed[c.Run] = c.At
	}
	for _, r := range h.Runs {
		if r.OpenEnded {
			continue
		}
		if logs[r.ID].terminal == "" {
			status := r.Status
			if n := len(r.Log); n > 0 {
				status += ", last event " + r.Log[n-1].Type
			}
			add(Inv11, r.ID, "%s run not terminal %s after faults stopped (%s)", r.Workflow, h.End.Sub(h.FaultsEnd), status)
			continue
		}
		if at := committed[r.ID]; at.After(deadline) {
			add(Inv11, r.ID, "run became terminal at %s, after the liveness bound %s", at.Format(time.RFC3339), deadline.Format(time.RFC3339))
		}
	}
}
