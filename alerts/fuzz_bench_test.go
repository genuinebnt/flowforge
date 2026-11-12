package alerts_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/core"
)

// FuzzAlertRule is fuzz_alert_rule (spec 25.5): arbitrary rule text gives
// diagnostics or a rule, never a panic, and every span lies inside the
// source. Compiled rules are also evaluated against a sample event.
func FuzzAlertRule(f *testing.F) {
	seeds := []string{
		"",
		"alert",
		`alert x when true -> slack()`,
		`alert new_admin_host when change.type == "entity_added" && entity.name matches "^(admin|jenkins)\\." -> slack("sec-recon"), email("lead@example.com") severity = High throttle 1h group_by entity.name`,
		`alert w when event.type == "approval.requested" && approval.expires_at - event.occurred_at > 6h -> slack("approvals") digest 15m`,
		`alert r when event.type == "run.run_failed" -> webhook("pagerduty-bridge") severity = Critical`,
		`alert q when change.seq in 1..10 && "a" in ["a"] || len(entity.name) % 0 == 1 -> slack() quiet_hours "22:00-07:00 Asia/Kolkata"`,
		`alert e when "\u{1F600}" == "{{" -> slack(`,
		`alert c when /* /* */ 0x_ == 1_2.3e -> slack() throttle 99999999999999999999d`,
		`alert p when entity.name |> starts_with("a") |> len -> x(severity = Nope)`,
		`alert d when ((((((((((((((((((((true)))))))))))))))))))) -> slack() group_by change.patch, entity.status`,
	}
	for _, s := range seeds {
		f.Add(s)
	}
	ev := sampleEvent(0, time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC))
	f.Fuzz(func(t *testing.T, src string) {
		opts := alerts.Options{LoadLocation: func(name string) (*time.Location, error) {
			if name == "UTC" || name == "Asia/Kolkata" {
				return time.FixedZone(name, 0), nil
			}
			return nil, fmt.Errorf("zone %q not available in fuzzing", name)
		}}
		rule, diags := alerts.CompileWith(src, opts)
		checkSpans(t, src, diags)
		if rule == nil && !alerts.HasErrors(diags) {
			t.Fatalf("no rule and no errors for %q", src)
		}
		if rule != nil {
			if alerts.HasErrors(diags) {
				t.Fatalf("rule returned with errors for %q", src)
			}
			_, _, _ = alerts.EvaluateRule("alr_fuzz", rule, ev, alerts.RenderOptions{})
		}
	})
}

func sampleEvent(i int, at time.Time) *alerts.Event {
	types := []string{"graph.entity_added", "graph.entity_changed", "run.run_failed", "run.effect_failed", "approval.requested"}
	ev := &alerts.Event{
		ID: fmt.Sprintf("evt_%d", i), Type: types[i%len(types)], TenantID: "ten_1", ProgramID: "prg_1", OccurredAt: at,
	}
	switch ev.Type {
	case "graph.entity_added", "graph.entity_changed":
		ev.Change = &alerts.Change{Type: ev.Type[len("graph."):], Seq: int64(i), RunID: "run_1", At: at, Patch: map[string]any{"name": "x"}}
		ev.Entity = &alerts.Entity{
			ID: fmt.Sprintf("ent_%d", i), AssetType: "FQDN", ScopeStatus: "in_scope", Confidence: 0.9, State: "active",
			Content: map[string]any{"name": fmt.Sprintf("host%d.svc%d.acme.com", i%97, i%13)},
		}
	case "approval.requested":
		ev.Approval = &alerts.Approval{ID: "apr_1", RunID: "run_1", Role: "lead", Status: "pending", ExpiresAt: at.Add(time.Duration(i%48) * time.Hour)}
	default:
		ev.Run = &alerts.Run{ID: "run_1", Seq: int64(i), Type: ev.Type[len("run."):], WorkflowName: "recon", Code: []string{"TOOL_TIMEOUT", "RATE_LIMITED"}[i%2]}
	}
	return ev
}

// benchRuleSources returns n varied rules: a quarter keyed on the
// envelope type (indexed), the rest over entity and run fields.
func benchRuleSources(n int) []string {
	out := make([]string, n)
	for i := range n {
		switch i % 4 {
		case 0:
			out[i] = fmt.Sprintf(`alert r%d when event.type == "run.run_failed" && run.code == "TOOL_TIMEOUT" && run.seq %% %d == 0 -> slack() throttle 1h group_by run.id`, i, i%7+1)
		case 1:
			out[i] = fmt.Sprintf(`alert r%d when change.type == "entity_added" && entity.asset_type == "FQDN" && entity.name matches "^host%d\\." -> slack("sec") severity = High group_by entity.name`, i, i%97)
		case 2:
			out[i] = fmt.Sprintf(`alert r%d when entity.scope_status == "in_scope" && entity.name contains "svc%d." && entity.confidence > 0.5 -> email("a@b.example")`, i, i%13)
		default:
			out[i] = fmt.Sprintf(`alert r%d when event.type in ["approval.requested", "approval.expired"] && approval.expires_at - event.occurred_at > %dh -> slack() digest 15m`, i, i%48)
		}
	}
	return out
}

// BenchmarkAlr01_Evaluate is b_alr_01 (spec 25.8): 1,000 rules evaluated
// against a stream of mixed events; reports events/s.
func BenchmarkAlr01_Evaluate(b *testing.B) {
	rs := alerts.NewRuleSet(alerts.RenderOptions{})
	for i, src := range benchRuleSources(1000) {
		r, ds := alerts.Compile(src)
		if r == nil {
			b.Fatalf("rule %d: %v", i, ds)
		}
		if err := rs.Add(fmt.Sprintf("alr_%d", i), r); err != nil {
			b.Fatal(err)
		}
	}
	const pool = 1000
	at := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	events := make([]*alerts.Event, pool)
	rng := core.NewSplitMix64(1)
	for i := range events {
		events[i] = sampleEvent(core.IntN(rng, 1<<20), at)
	}
	b.ReportAllocs()
	b.ResetTimer()
	n, matches := 0, 0
	for b.Loop() {
		matches += len(rs.Evaluate(events[n%pool]).Matches)
		n++
	}
	if s := b.Elapsed().Seconds(); s > 0 {
		b.ReportMetric(float64(n)/s, "events/s")
	}
	b.ReportMetric(float64(matches)/float64(max(n, 1)), "matches/event")
}
