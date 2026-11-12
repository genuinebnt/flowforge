package alerts_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	_ "time/tzdata" // deterministic zone data for quiet-hours tests

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/testkit"
)

func newProc(t *testing.T, clock *testkit.FakeClock, rules map[string]string) *alerts.Processor {
	t.Helper()
	rs := alerts.NewRuleSet(alerts.RenderOptions{ConsoleBaseURL: "https://console.example.com"})
	for id, src := range rules {
		r, ds := alerts.Compile(src)
		if r == nil {
			t.Fatalf("%s: %s", id, renderDiags(ds))
		}
		if err := rs.Add(id, r); err != nil {
			t.Fatal(err)
		}
	}
	p, err := alerts.NewProcessor(rs, alerts.ProcessorConfig{Clock: clock, Store: alerts.NewMemProcessedStore()})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func runEvent(id string, at time.Time) *alerts.Event {
	return &alerts.Event{ID: id, Type: "run.run_failed", TenantID: "ten_1", OccurredAt: at,
		Run: &alerts.Run{ID: "run_" + id, Type: "run_failed", Code: "TOOL_TIMEOUT"}}
}

func TestAlrDigest_CountAndFirstTwenty(t *testing.T) {
	clock := testkit.NewFakeClock(conformanceStart)
	p := newProc(t, clock, map[string]string{
		"alr_d": `alert failures when event.type == "run.run_failed" -> slack("ops"), webhook("bridge") digest 15m`,
	})
	for i := range 25 {
		out, err := p.Process(context.Background(), runEvent(fmt.Sprint("e", i), clock.Now()))
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Alerts) != 1 || !out.Alerts[0].Digested || out.Alerts[0].Status != alerts.StatusPending || len(out.Notifications) != 0 {
			t.Fatalf("event %d: %+v", i, out)
		}
		clock.Advance(20 * time.Second)
	}
	if n := p.Flush(); len(n) != 0 {
		t.Fatalf("flushed before the window ended: %+v", n)
	}
	if at, ok := p.NextFlush(); !ok || !at.Equal(conformanceStart.Add(15*time.Minute)) {
		t.Fatalf("NextFlush = %v %v", at, ok)
	}
	clock.SetWall(conformanceStart.Add(15 * time.Minute))
	ns := p.Flush()
	if len(ns) != 1 || ns[0].Digest == nil || ns[0].Payload != nil {
		t.Fatalf("want one digest notification, got %+v", ns)
	}
	d := ns[0].Digest
	if d.Count != 25 || len(d.Items) != alerts.DigestMaxItems || d.Items[0].EventID != "e0" || d.Items[19].EventID != "e19" {
		t.Fatalf("digest count %d items %d first %s", d.Count, len(d.Items), d.Items[0].EventID)
	}
	if len(ns[0].Actions) != 2 || !ns[0].DeliverAt.Equal(clock.Now()) {
		t.Fatalf("notification %+v", ns[0])
	}
	// The window is closed: the next match opens a new one.
	if _, err := p.Process(context.Background(), runEvent("late", clock.Now())); err != nil {
		t.Fatal(err)
	}
	clock.Advance(15 * time.Minute)
	if ns := p.Flush(); len(ns) != 1 || ns[0].Digest.Count != 1 {
		t.Fatalf("second window: %+v", ns)
	}
}

func TestAlrQuietHours(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	src := func(sev string) string {
		return `alert q_` + sev + ` when event.type == "run.run_failed" -> slack() severity = ` + sev + ` quiet_hours "22:00-07:00 Asia/Kolkata"`
	}
	night := time.Date(2026, 11, 12, 23, 30, 0, 0, ist)
	morning := time.Date(2026, 11, 13, 7, 0, 0, 0, ist)
	cases := []struct {
		sev     string
		at      time.Time
		deliver time.Time
	}{
		{"Info", night, morning},
		{"Low", night, morning},
		{"Medium", time.Date(2026, 11, 13, 6, 59, 0, 0, ist), morning},
		{"Medium", morning, morning}, // window end is exclusive
		{"Low", time.Date(2026, 11, 12, 21, 59, 0, 0, ist), time.Date(2026, 11, 12, 21, 59, 0, 0, ist)},
		{"High", night, night},     // below High is delayed; High is not
		{"Critical", night, night}, // Critical ignores quiet hours
	}
	for i, c := range cases {
		clock := testkit.NewFakeClock(c.at)
		p := newProc(t, clock, map[string]string{"alr_q": src(c.sev)})
		out, err := p.Process(context.Background(), runEvent(fmt.Sprint("q", i), c.at))
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Notifications) != 1 {
			t.Fatalf("case %d: %+v", i, out)
		}
		n := out.Notifications[0]
		if !n.DeliverAt.Equal(c.deliver) || n.Delayed != c.deliver.After(c.at) {
			t.Errorf("case %d (%s at %s): deliver %s delayed %v, want %s", i, c.sev, c.at, n.DeliverAt.In(ist), n.Delayed, c.deliver)
		}
	}

	t.Run("non-wrapping window in UTC by default", func(t *testing.T) {
		r := alerts.MustCompile(`alert w when true -> slack() severity = Low quiet_hours "09:00-17:00"`)
		q := r.QuietHours
		in := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
		if got := q.DeliverAt(alerts.SeverityLow, in); !got.Equal(time.Date(2026, 11, 12, 17, 0, 0, 0, time.UTC)) {
			t.Fatalf("deliver %s", got)
		}
		if out := time.Date(2026, 11, 12, 18, 0, 0, 0, time.UTC); !q.DeliverAt(alerts.SeverityInfo, out).Equal(out) || q.Contains(out) {
			t.Fatal("outside the window must not delay")
		}
	})

	t.Run("digest flush respects quiet hours", func(t *testing.T) {
		clock := testkit.NewFakeClock(night)
		p := newProc(t, clock, map[string]string{
			"alr_qd": `alert qd when event.type == "run.run_failed" -> slack() digest 15m quiet_hours "22:00-07:00 Asia/Kolkata"`,
		})
		if _, err := p.Process(context.Background(), runEvent("x", night)); err != nil {
			t.Fatal(err)
		}
		clock.Advance(15 * time.Minute)
		ns := p.Flush()
		if len(ns) != 1 || !ns[0].Delayed || !ns[0].DeliverAt.Equal(morning) {
			t.Fatalf("%+v", ns)
		}
	})
}

func TestAlrIdempotentProcessing(t *testing.T) {
	// inv_08: transport duplicates never produce duplicate alerts.
	clock := testkit.NewFakeClock(conformanceStart)
	rs := alerts.NewRuleSet(alerts.RenderOptions{})
	for i := range 3 {
		if err := rs.Add(fmt.Sprint("alr_", i), alerts.MustCompile(`alert r when event.type == "run.run_failed" -> slack()`)); err != nil {
			t.Fatal(err)
		}
	}
	store := alerts.NewMemProcessedStore()
	p, err := alerts.NewProcessor(rs, alerts.ProcessorConfig{Clock: clock, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	alertsPerRule := map[string]int{}
	duplicates := 0
	for range 4 { // each event delivered four times, concurrently
		for e := range 50 {
			wg.Go(func() {
				out, err := p.Process(context.Background(), runEvent(fmt.Sprint("evt_", e), conformanceStart))
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if out.Duplicate {
					duplicates++
				}
				for _, a := range out.Alerts {
					alertsPerRule[a.RuleID+"/"+a.TriggerEventID]++
				}
			})
		}
	}
	wg.Wait()
	if duplicates != 150 || len(alertsPerRule) != 150 || store.Len() != 50 {
		t.Fatalf("duplicates %d distinct alerts %d store %d", duplicates, len(alertsPerRule), store.Len())
	}
	for k, n := range alertsPerRule {
		if n != 1 {
			t.Fatalf("%s produced %d alerts", k, n)
		}
	}
}

type failingStore struct{}

var errStoreDown = errors.New("store down")

func (failingStore) MarkProcessed(context.Context, string, string, time.Time) (bool, error) {
	return false, errStoreDown
}

func TestAlrProcessorErrors(t *testing.T) {
	clock := testkit.NewFakeClock(conformanceStart)
	rs := alerts.NewRuleSet(alerts.RenderOptions{})
	if _, err := alerts.NewProcessor(rs, alerts.ProcessorConfig{Store: failingStore{}}); !errors.Is(err, alerts.ErrMissingClock) {
		t.Fatal(err)
	}
	if _, err := alerts.NewProcessor(nil, alerts.ProcessorConfig{Clock: clock, Store: failingStore{}}); !errors.Is(err, alerts.ErrMissingRules) {
		t.Fatal(err)
	}
	p, err := alerts.NewProcessor(rs, alerts.ProcessorConfig{Clock: clock, Store: failingStore{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Process(context.Background(), nil); !errors.Is(err, alerts.ErrNilEvent) {
		t.Fatal(err)
	}
	if _, err := p.Process(context.Background(), &alerts.Event{Type: "x"}); !errors.Is(err, alerts.ErrMissingEventID) {
		t.Fatal(err)
	}
	if _, err := p.Process(context.Background(), runEvent("a", conformanceStart)); !errors.Is(err, errStoreDown) {
		t.Fatalf("store error not wrapped: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := alerts.NewMemProcessedStore().MarkProcessed(ctx, "c", "e", conformanceStart); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestAlrRuleSet(t *testing.T) {
	rs := alerts.NewRuleSet(alerts.RenderOptions{})
	r := alerts.MustCompile(`alert a when event.type == "run.run_failed" -> slack()`)
	if err := rs.Add("", r); !errors.Is(err, alerts.ErrEmptyRuleID) {
		t.Fatal(err)
	}
	if err := rs.Add("x", nil); !errors.Is(err, alerts.ErrRuleInvalid) {
		t.Fatal(err)
	}
	if err := rs.Add("alr_a", r); err != nil {
		t.Fatal(err)
	}
	if err := rs.Add("alr_a", r); !errors.Is(err, alerts.ErrDuplicateRule) {
		t.Fatal(err)
	}
	if err := rs.Add("alr_b", alerts.MustCompile(`alert b when run.code == "TOOL_TIMEOUT" -> slack()`)); err != nil {
		t.Fatal(err)
	}
	if err := rs.Add("alr_c", alerts.MustCompile(`alert c when event.type == "run.run_failed" || event.type == "run.x" -> slack()`)); err != nil {
		t.Fatal(err)
	}
	ev := runEvent("e", conformanceStart)
	var got []string
	for _, m := range rs.Evaluate(ev).Matches {
		got = append(got, m.RuleID)
	}
	if strings.Join(got, ",") != "alr_a,alr_b,alr_c" {
		t.Fatalf("matches in rule order: %v", got)
	}
	if !rs.Remove("alr_b") || rs.Remove("alr_b") || rs.Len() != 2 {
		t.Fatal("remove")
	}
	if n := len(rs.Evaluate(ev).Matches); n != 2 {
		t.Fatalf("after remove: %d", n)
	}
	if ev := rs.Evaluate(nil); len(ev.Matches) != 0 {
		t.Fatal("nil event matched")
	}

	// Run-time failures are reported, not matched.
	bad := alerts.MustCompile(`alert ovf when run.seq * 9223372036854775807 > 1 -> slack()`)
	_, ok, err := alerts.EvaluateRule("alr_ovf", bad, &alerts.Event{ID: "e", Run: &alerts.Run{Seq: 3}}, alerts.RenderOptions{})
	if ok || !errors.Is(err, alerts.ErrArithOverflow) {
		t.Fatalf("overflow: ok=%v err=%v", ok, err)
	}
}

func TestAlrPayloadShape(t *testing.T) {
	r := alerts.MustCompile(`alert new_admin_host when entity.name matches "^jenkins\\." -> slack("sec-recon") severity = High group_by entity.name`)
	ev := &alerts.Event{
		ID: "evt_1", Type: "graph.entity_added", TenantID: "ten_01JD8K0B", ProgramID: "prg_01JD8K2XH3",
		OccurredAt: time.Date(2026, 11, 12, 14, 2, 12, 311e6, time.UTC),
		Entity:     &alerts.Entity{ID: "ent_01JD8K3F", AssetType: "FQDN", Content: map[string]any{"name": "jenkins.acme.com"}},
	}
	m, ok, err := alerts.EvaluateRule("alr_01JD8K8C", r, ev, alerts.RenderOptions{ConsoleBaseURL: "https://console.example.com"})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if m.DedupeKey != alerts.DedupeKey("alr_01JD8K8C", []any{"jenkins.acme.com"}) || len(m.DedupeKey) != 64 {
		t.Fatalf("dedupe key %s", m.DedupeKey)
	}
	if alerts.DedupeKey("alr_01JD8K8C", nil) == m.DedupeKey || alerts.DedupeKey("alr_other", []any{"jenkins.acme.com"}) == m.DedupeKey {
		t.Fatal("dedupe key ignores rule id or values")
	}
	body, err := json.Marshal(alerts.AlertBody{AlertID: "ale_01JD8KBA", Payload: m.Payload})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"alert_id": "ale_01JD8KBA", "rule": "new_admin_host", "severity": "high",
		"tenant_id": "ten_01JD8K0B", "program_id": "prg_01JD8K2XH3", "occurred_at": "2026-11-12T14:02:12.311Z",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	ent, _ := got["entity"].(map[string]any)
	links, _ := got["links"].(map[string]any)
	if ent["id"] != "ent_01JD8K3F" || ent["asset_type"] != "FQDN" || ent["name"] != "jenkins.acme.com" {
		t.Errorf("entity = %v", ent)
	}
	if links["console"] != "https://console.example.com/graph/entities/ent_01JD8K3F" {
		t.Errorf("links = %v", links)
	}
	if s, _ := got["summary"].(string); !strings.Contains(s, "jenkins.acme.com") {
		t.Errorf("summary = %q", s)
	}
}

func TestAlrDiagnosticFormat(t *testing.T) {
	src := "alert x\n  when entity.nmae == \"é\" && entity.name == \"a\"\n  -> slack()"
	_, ds := alerts.Compile(src)
	if len(ds) != 1 {
		t.Fatalf("%s", renderDiags(ds))
	}
	d := ds[0]
	if d.Code != alerts.CodeUnknownField || d.Primary.Start != (alerts.Position{Line: 2, Col: 15, Offset: 22}) || d.Primary.End.Offset != 26 || d.Help == "" {
		t.Fatalf("%+v", d)
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"code":"A0202"`, `"severity":"error"`, `"primary":{"file":"rule","start":{"line":2,"col":15,"offset":22}`, `"help":`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("JSON %s lacks %s", b, k)
		}
	}
	// Columns count code points, not bytes.
	_, ds = alerts.Compile("alert x when \"é\" == entity.nope -> slack()")
	if len(ds) != 1 || ds[0].Primary.Start.Col != 28 || ds[0].Primary.Start.Offset != 28 {
		t.Fatalf("%+v", ds)
	}
	// Secondary labels point at the first occurrence.
	_, ds = alerts.Compile("alert x when true -> slack() throttle 1h throttle 2h")
	if len(ds) != 1 || len(ds[0].Secondary) != 1 || ds[0].Secondary[0].Start.Offset != 29 {
		t.Fatalf("%+v", ds)
	}
	if _, ds := alerts.Compile(string([]byte{'a', 0xff})); len(ds) != 1 || ds[0].Code != alerts.CodeInvalidUTF8 {
		t.Fatalf("%+v", ds)
	}
	if _, ds := alerts.Compile(strings.Repeat(" ", alerts.MaxSourceLen+1)); len(ds) != 1 || ds[0].Code != alerts.CodeTooLarge {
		t.Fatalf("%+v", ds)
	}
}

func TestAlrDefaultRules(t *testing.T) {
	rs := alerts.NewRuleSet(alerts.RenderOptions{})
	for id, src := range alerts.DefaultRules() {
		r, ds := alerts.Compile(src)
		if r == nil {
			t.Fatalf("%s: %s", id, renderDiags(ds))
		}
		if r.Name != id {
			t.Fatalf("rule %s is named %s", id, r.Name)
		}
		if err := rs.Add(id, r); err != nil {
			t.Fatal(err)
		}
	}
	at := conformanceStart
	events := map[string]*alerts.Event{
		"ops_run_failed": runEvent("a", at),
		"ops_approval_waiting": {ID: "b", Type: "approval.requested", OccurredAt: at,
			Approval: &alerts.Approval{ID: "apr_1", ExpiresAt: at.Add(24 * time.Hour)}},
		"ops_tool_rate_limited": {ID: "c", Type: "run.effect_failed", Run: &alerts.Run{ID: "run_1", Code: "RATE_LIMITED", Path: "main/0"}},
		"ops_budget_exhausted":  {ID: "d", Type: "run.budget_exceeded", Run: &alerts.Run{ID: "run_1", Resource: "tokens"}},
		"ops_signup_abuse":      {ID: "e", Type: "auth.signup_throttled"},
	}
	for want, ev := range events {
		ms := rs.Evaluate(ev).Matches
		if len(ms) != 1 || ms[0].RuleID != want {
			t.Errorf("%s: matches %+v", want, ms)
		}
	}
}
