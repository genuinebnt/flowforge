package alerts_test

import (
	"fmt"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestAlrProp_ScoreNeverDecides (V2 §12.4, "scores rank, never send"):
// for rules over every addressable diff field, changing only a change's
// score and score reasons never changes which rules match or their
// dedupe keys; the score reaches the payload, where receivers rank by it.
func TestAlrProp_ScoreNeverDecides(t *testing.T) {
	rs := alerts.NewRuleSet(alerts.RenderOptions{})
	for i, src := range []string{
		`alert a when diff.semantic && diff.alertable -> webhook("w")`,
		`alert b when diff.kinds contains "endpoint_added" -> webhook("w") group_by diff.url`,
		`alert c when diff.class == "semantic_endpoint" && diff.newness != "pre_existing" -> webhook("w")`,
		`alert d when diff.levels contains "text" && !diff.baseline -> webhook("w") group_by diff.snapshot_id`,
		`alert e when diff.summary contains "admin" || diff.excerpt contains "secret" -> webhook("w")`,
	} {
		r, ds := alerts.Compile(src)
		if r == nil {
			t.Fatalf("%s: %s", src, renderDiags(ds))
		}
		if err := rs.Add(fmt.Sprintf("alr_%d", i), r); err != nil {
			t.Fatal(err)
		}
	}
	rng := testkit.Rng(t)
	intn := func(n int) int { return int(rng.Uint64() % uint64(n)) }
	unit := func() float64 { return float64(rng.Uint64()>>11) / (1 << 53) }
	pick := func(xs ...string) string { return xs[intn(len(xs))] }
	for range testkit.Cases() {
		var changes []string
		for range intn(5) {
			changes = append(changes, pick("added", "removed", "modified")+" "+pick("endpoint", "form", "header", "secret")+"|"+pick("ep:GET https://h/admin", "x-frame-options", "k"))
		}
		d := &alerts.Diff{SnapshotID: pick("snp_1", "snp_2"), URL: pick("https://h/", "https://h/a.js"), Outcome: pick("changed", "structural", "masked_only"),
			Levels: [][]string{{"hash"}, {"hash", "text"}, {"hash", "text", "semantic"}}[intn(3)], Class: pick("semantic_endpoint", "metadata", ""),
			Newness: pick("likely_new", "pre_existing"), Cause: pick("target", "parser"), Baseline: intn(4) == 0, Alertable: intn(2) == 0,
			Changes: changes, Excerpt: pick("", "+secret: [REDACTED:jwt:x:10]")}
		ev := &alerts.Event{ID: "evt_1", Type: "content.changed", TenantID: "ten_1", Change: &alerts.Change{Type: "content_changed"}, Diff: d}
		base := rs.Evaluate(ev)
		other := *d
		other.Score = unit()
		other.ScoreFeatures = []alerts.ScoreFeature{{Name: pick("new_endpoint", "exposed_secret"), Weight: unit()}}
		ev2 := *ev
		ev2.Diff = &other
		got := rs.Evaluate(&ev2)
		if len(got.Matches) != len(base.Matches) {
			t.Fatalf("score %v changed the matches: %d vs %d", other.Score, len(got.Matches), len(base.Matches))
		}
		for i := range got.Matches {
			if got.Matches[i].RuleID != base.Matches[i].RuleID || got.Matches[i].DedupeKey != base.Matches[i].DedupeKey {
				t.Fatalf("score changed match %d: %+v vs %+v", i, got.Matches[i], base.Matches[i])
			}
			if got.Matches[i].Payload.Diff.Score != other.Score || len(got.Matches[i].Payload.Diff.ScoreFeatures) != 1 {
				t.Fatalf("payload lost the score: %+v", got.Matches[i].Payload.Diff)
			}
		}
	}
}

// TestAlrDiff_PayloadBounds: a payload lists at most MaxPayloadChanges
// changes and counts the rest; the summary names the first three.
func TestAlrDiff_PayloadBounds(t *testing.T) {
	r, ds := alerts.Compile(`alert many when diff.semantic -> webhook("w")`)
	if r == nil {
		t.Fatal(renderDiags(ds))
	}
	var changes []string
	for i := range alerts.MaxPayloadChanges + 7 {
		changes = append(changes, fmt.Sprintf("added endpoint|ep:GET https://h/p%d", i))
	}
	ev := &alerts.Event{ID: "evt_1", Type: "content.changed", TenantID: "ten_1",
		Diff: &alerts.Diff{URL: "https://h/", Levels: []string{"hash", "text", "semantic"}, Class: "semantic_endpoint", Changes: changes}}
	m, ok, err := alerts.EvaluateRule("alr_1", r, ev, alerts.RenderOptions{})
	if err != nil || !ok {
		t.Fatalf("no match: %v", err)
	}
	pd := m.Payload.Diff
	if len(pd.Changes) != alerts.MaxPayloadChanges || pd.MoreChanges != 7 ||
		pd.Summary != "semantic_endpoint: added endpoint ep:GET https://h/p0; added endpoint ep:GET https://h/p1; added endpoint ep:GET https://h/p2 (+24 more)" {
		t.Fatalf("payload diff %+v", pd)
	}
}

// TestAlrDiff_ScoreNotAddressable pins the reason
// score is missing from the schema: the diagnostic lists the diff fields,
// and none of them is a score.
func TestAlrDiff_ScoreNotAddressable(t *testing.T) {
	_, ds := alerts.Compile(`alert ranked when diff.score > 0.5 -> slack("x")`)
	if len(ds) != 1 || ds[0].Code != alerts.CodeUnknownField || strings.Contains(ds[0].Help, "score") || !strings.Contains(ds[0].Help, "kinds") {
		t.Fatalf("diagnostics %+v", ds)
	}
	// The package that evaluates rules imports nothing that reaches the
	// network: no code path from a score to a request.
	fs := token.NewFileSet()
	for _, name := range []string{"eval.go", "ruleset.go", "schema.go", "event.go"} {
		f, err := parser.ParseFile(fs, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if p := strings.Trim(imp.Path.Value, `"`); p == "net" || strings.HasPrefix(p, "net/") || p == "os" || p == "syscall" {
				t.Fatalf("%s imports %s", name, p)
			}
		}
	}
}
