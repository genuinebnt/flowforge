package alerts_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/testkit"
)

// evalFile is the shape of every conformance/alerts/eval/*.json file.
type evalFile struct {
	Description string        `json:"description"`
	Event       *alerts.Event `json:"event"` // default event for cases and steps
	Cases       []evalCase    `json:"cases"`
}

type evalCase struct {
	Name        string        `json:"name"`
	Rule        string        `json:"rule"` // full rule source
	When        string        `json:"when"` // or just a condition
	Event       *alerts.Event `json:"event"`
	Match       *bool         `json:"match"`       // single-step shortcut
	Diagnostics []string      `json:"diagnostics"` // expected error codes, source order
	Severity    string        `json:"severity"`
	Actions     []string      `json:"actions"` // "kind:target"
	Steps       []evalStep    `json:"steps"`
}

type evalStep struct {
	Advance         string        `json:"advance"`
	Event           *alerts.Event `json:"event"`
	Outcome         string        `json:"outcome"` // alert | suppressed | none | duplicate
	Key             string        `json:"key"`
	SuppressedCount *int          `json:"suppressed_count"`
	EntityName      string        `json:"entity_name"`
}

var conformanceStart = time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)

const evalSuite = "alerts/eval"

func loadEval(t *testing.T, include func(name string) bool) []testkit.CaseFile[evalFile] {
	t.Helper()
	var out []testkit.CaseFile[evalFile]
	for _, f := range testkit.LoadCases[evalFile](t, evalSuite) {
		if include(f.Name) {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		t.Fatal("no conformance files selected")
	}
	return out
}

func runEvalFile(t *testing.T, f testkit.CaseFile[evalFile]) {
	for _, c := range f.Data.Cases {
		t.Run(strings.TrimSuffix(f.Name, ".json")+"/"+c.Name, func(t *testing.T) {
			runEvalCase(t, f.Data, c)
		})
	}
}

func runEvalCase(t *testing.T, file evalFile, c evalCase) {
	src := c.Rule
	if c.When != "" {
		src = "alert op_case when " + c.When + " -> slack()"
	}
	rule, diags := alerts.Compile(src)
	checkSpans(t, src, diags)
	if len(c.Diagnostics) > 0 {
		if rule != nil {
			t.Fatalf("rule compiled; want diagnostics %v", c.Diagnostics)
		}
		var got []string
		for _, d := range diags {
			got = append(got, d.Code)
		}
		if !slices.Equal(got, c.Diagnostics) {
			t.Fatalf("diagnostic codes = %v, want %v\n%s", got, c.Diagnostics, renderDiags(diags))
		}
		if err := alerts.NewRuleSet(alerts.RenderOptions{}).Add("alr_bad", rule); err == nil {
			t.Fatal("a rule that failed to compile was enabled")
		}
		return
	}
	if rule == nil {
		t.Fatalf("compile failed:\n%s", renderDiags(diags))
	}
	if c.Severity != "" && rule.Severity.String() != c.Severity {
		t.Errorf("severity = %s, want %s", rule.Severity, c.Severity)
	}
	if c.Actions != nil {
		var got []string
		for _, a := range rule.Actions {
			got = append(got, string(a.Kind)+":"+a.Target)
		}
		if !slices.Equal(got, c.Actions) {
			t.Errorf("actions = %v, want %v", got, c.Actions)
		}
	}
	steps := c.Steps
	if c.Match != nil {
		outcome := "none"
		if *c.Match {
			outcome = "alert"
		}
		steps = []evalStep{{Outcome: outcome}}
	}
	defaultEvent := c.Event
	if defaultEvent == nil {
		defaultEvent = file.Event
	}

	rs := alerts.NewRuleSet(alerts.RenderOptions{ConsoleBaseURL: "https://console.example.com"})
	if err := rs.Add("alr_case", rule); err != nil {
		t.Fatal(err)
	}
	clock := testkit.NewFakeClock(conformanceStart)
	proc, err := alerts.NewProcessor(rs, alerts.ProcessorConfig{Clock: clock, Store: alerts.NewMemProcessedStore()})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for i, st := range steps {
		if st.Advance != "" {
			d, err := time.ParseDuration(st.Advance)
			if err != nil {
				t.Fatalf("step %d: bad advance: %v", i, err)
			}
			clock.Advance(d)
		}
		evp := st.Event
		if evp == nil {
			evp = defaultEvent
		}
		if evp == nil {
			t.Fatalf("step %d has no event", i)
		}
		ev := *evp
		if ev.ID == "" {
			ev.ID = fmt.Sprintf("evt_step_%d", i)
		}
		if ev.OccurredAt.IsZero() {
			ev.OccurredAt = clock.Now()
		}
		out, err := proc.Process(context.Background(), &ev)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		checkStep(t, i, st, out, keys)
	}
}

func checkStep(t *testing.T, i int, st evalStep, out alerts.Outcome, keys map[string]string) {
	t.Helper()
	switch st.Outcome {
	case "duplicate":
		if !out.Duplicate || len(out.Alerts) != 0 {
			t.Fatalf("step %d: want duplicate, got %+v", i, out)
		}
		return
	case "none":
		if out.Duplicate || len(out.Alerts) != 0 {
			t.Fatalf("step %d: want no alert, got %+v", i, out)
		}
		return
	case "alert", "suppressed":
	default:
		t.Fatalf("step %d: unknown outcome %q", i, st.Outcome)
	}
	if len(out.Alerts) != 1 {
		t.Fatalf("step %d: want one %s alert, got %d (errors %v)", i, st.Outcome, len(out.Alerts), out.Errors)
	}
	a := out.Alerts[0]
	want := alerts.StatusPending
	if st.Outcome == "suppressed" {
		want = alerts.StatusSuppressed
	}
	if a.Status != want {
		t.Fatalf("step %d: status = %s, want %s", i, a.Status, want)
	}
	if st.Key != "" {
		if prev, ok := keys[st.Key]; ok && prev != a.DedupeKey {
			t.Errorf("step %d: key %q changed: %s vs %s", i, st.Key, prev, a.DedupeKey)
		}
		for label, k := range keys {
			if label != st.Key && k == a.DedupeKey {
				t.Errorf("step %d: key %q collides with %q", i, st.Key, label)
			}
		}
		keys[st.Key] = a.DedupeKey
	}
	if st.SuppressedCount != nil && a.Payload.SuppressedCount != *st.SuppressedCount {
		t.Errorf("step %d: suppressed_count = %d, want %d", i, a.Payload.SuppressedCount, *st.SuppressedCount)
	}
	if st.EntityName != "" && (a.Payload.Entity == nil || a.Payload.Entity.Name != st.EntityName) {
		t.Errorf("step %d: payload entity = %+v, want name %q", i, a.Payload.Entity, st.EntityName)
	}
}

// checkSpans asserts every diagnostic span lies inside the source.
func checkSpans(t testing.TB, src string, diags []alerts.Diagnostic) {
	t.Helper()
	for _, d := range diags {
		labels := append([]alerts.Label{d.Primary}, d.Secondary...)
		for _, l := range labels {
			s, e := l.Start.Offset, l.End.Offset
			if s < 0 || e < s || e > len(src) || l.Start.Line < 1 || l.Start.Col < 1 || l.End.Line < l.Start.Line {
				t.Fatalf("diagnostic %s span out of range [%d,%d) for source of %d bytes: %+v", d.Code, s, e, len(src), l)
			}
		}
		if d.Code == "" || d.Message == "" || d.Severity == "" {
			t.Fatalf("incomplete diagnostic %+v", d)
		}
	}
}

func renderDiags(ds []alerts.Diagnostic) string {
	var b strings.Builder
	for _, d := range ds {
		b.WriteString(d.Error())
		b.WriteByte('\n')
	}
	return b.String()
}

func TestAlr001_Operators(t *testing.T) {
	for _, f := range loadEval(t, func(n string) bool { return n != "throttle.json" }) {
		runEvalFile(t, f)
	}
}

func TestAlr002_DedupeThrottle(t *testing.T) {
	for _, f := range loadEval(t, func(n string) bool { return n == "throttle.json" }) {
		runEvalFile(t, f)
	}

	t.Run("one alert per window, the next after the window", func(t *testing.T) {
		rule, diags := alerts.Compile(`alert host when entity.asset_type == "FQDN" -> slack("x") throttle 1h group_by entity.name`)
		if rule == nil {
			t.Fatal(renderDiags(diags))
		}
		rs := alerts.NewRuleSet(alerts.RenderOptions{})
		if err := rs.Add("alr_1", rule); err != nil {
			t.Fatal(err)
		}
		clock := testkit.NewFakeClock(conformanceStart)
		proc, err := alerts.NewProcessor(rs, alerts.ProcessorConfig{Clock: clock, Store: alerts.NewMemProcessedStore()})
		if err != nil {
			t.Fatal(err)
		}
		fired, suppressed := 0, 0
		// Three windows of an hour, one event a minute for the same host.
		for i := range 180 {
			ev := &alerts.Event{
				ID: fmt.Sprintf("evt_%d", i), Type: "graph.entity_changed", TenantID: "ten_1", OccurredAt: clock.Now(),
				Entity: &alerts.Entity{ID: "ent_1", AssetType: "FQDN", Content: map[string]any{"name": "a.example.com"}},
			}
			out, err := proc.Process(context.Background(), ev)
			if err != nil {
				t.Fatal(err)
			}
			for _, a := range out.Alerts {
				switch a.Status {
				case alerts.StatusPending:
					fired++
					if i%60 != 0 {
						t.Fatalf("alert fired at minute %d, want only at window starts", i)
					}
					if len(out.Notifications) != 1 || !out.Notifications[0].DeliverAt.Equal(clock.Now()) {
						t.Fatalf("minute %d: notifications %+v", i, out.Notifications)
					}
				case alerts.StatusSuppressed:
					suppressed++
				}
			}
			clock.Advance(time.Minute)
		}
		if fired != 3 || suppressed != 177 {
			t.Fatalf("fired %d suppressed %d, want 3 and 177", fired, suppressed)
		}
	})
}
