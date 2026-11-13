package alerts_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestRegress_GLIM06_StateSurvivesRestart: the state a processor reports
// in Outcome.State, restored into a new processor (a restart), keeps the
// throttle window closed and the open digest's matches. The state is sent
// through JSON as the notifier persists it.
func TestRegress_GLIM06_StateSurvivesRestart(t *testing.T) {
	rules := map[string]string{
		"alr_t": `alert failures when event.type == "run.run_failed" -> slack("ops") group_by run.code throttle 1h`,
		"alr_d": `alert digest when event.type == "run.run_failed" -> webhook("bridge") digest 15m`,
	}
	clock := testkit.NewFakeClock(conformanceStart)
	first := newProc(t, clock, rules)
	out, err := first.Process(context.Background(), runEvent("e1", clock.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Notifications) != 1 || len(out.State.Throttle) != 1 || len(out.State.Digests) != 1 {
		t.Fatalf("first event: %d notifications, state %+v", len(out.Notifications), out.State)
	}
	persisted, err := json.Marshal(alerts.ProcessorState{Throttle: out.State.Throttle, Digests: out.State.Digests})
	if err != nil {
		t.Fatal(err)
	}

	clock.Advance(5 * time.Minute)
	second := newProc(t, clock, rules) // the restart
	var st alerts.ProcessorState
	if err := json.Unmarshal(persisted, &st); err != nil {
		t.Fatal(err)
	}
	second.Restore(st)
	out, err = second.Process(context.Background(), runEvent("e2", clock.Now()))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range out.Alerts {
		if a.RuleID == "alr_t" && a.Status != alerts.StatusSuppressed {
			t.Fatalf("throttled key fired again after restart: %+v", a)
		}
	}
	if len(out.Notifications) != 0 {
		t.Fatalf("notifications after restart: %+v", out.Notifications)
	}
	clock.Advance(15 * time.Minute)
	ns := second.Flush()
	if len(ns) != 1 || ns[0].Digest.Count != 2 || len(ns[0].Actions) != 1 || !ns[0].Digest.WindowEnd.Equal(conformanceStart.Add(15*time.Minute)) {
		t.Fatalf("digest after restart: %+v", ns)
	}

	// A window that ended while the process was down is not restored.
	third := newProc(t, clock, rules)
	clock.Advance(2 * time.Hour)
	third.Restore(st)
	if s := third.State(); len(s.Throttle) != 0 {
		t.Fatalf("expired window restored: %+v", s.Throttle)
	}
}
