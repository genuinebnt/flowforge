package content

import (
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/oam"
)

// TestContentLearnVolatility pins the two detectors and the freezing
// rules: a new set version only when a rule is added, rules sorted, and
// the previous set returned unchanged otherwise.
func TestContentLearnVolatility(t *testing.T) {
	var c VolatilityCounter
	for range 10 {
		c = c.Record(true)
	}
	c.Path = "html:attr:div.data-build"
	c = c.Record(false)
	ratio, streak := VolatilityPolicy{}, VolatilityPolicy{Algo: VolatilityStreak}
	if !ratio.Volatile(c) || streak.Volatile(c) {
		t.Fatalf("10 changes then one stable: ratio %v streak %v", ratio.Volatile(c), streak.Volatile(c))
	}
	set, refused := LearnVolatility(VolatilitySet{}, []VolatilityCounter{c, {Path: "html:attr:script.src", Changes: 20}}, ratio)
	if set.Version != 1 || len(set.Rules) != 1 || set.Rules[0].Path != c.Path || !slices.Equal(refused, []string{"html:attr:script.src"}) {
		t.Fatalf("learned %+v refused %v", set, refused)
	}
	again, _ := LearnVolatility(set, []VolatilityCounter{c}, ratio)
	if again.Version != 1 || len(again.Rules) != 1 {
		t.Fatalf("relearning the same rule bumped the set: %+v", again)
	}
	if err := (VolatilitySet{Version: 1, Rules: []VolatileRule{{Path: "json:/b"}, {Path: "json:/a"}}}).Validate(); !errors.Is(err, ErrInvalidRule) {
		t.Fatalf("unsorted rules accepted: %v", err)
	}
	n := Normalize(KindHTML, []byte(`<div data-build="b-77">x</div>`), NormalizeOptions{Learned: set})
	if !strings.Contains(string(n.Text), maskPlaceholder(MaskLearned)) || n.Version != "html.learned1@1" {
		t.Fatalf("learned rule not applied or not versioned: %s %s", n.Version, n.Text)
	}
}

// TestContentNoiseAndAudit pins the noise metric and the audit sample.
func TestContentNoiseAndAudit(t *testing.T) {
	var n NoiseCounter
	for _, o := range []string{DiffStructural, DiffChanged, DiffMaskedOnly, DiffReordered, DiffNoChange, DiffDegraded} {
		n.Add(ChangeSet{Outcome: o})
	}
	if n.Compared != 5 || n.TextOnly != 1 || n.MaskedOnly != 1 || n.Reordered != 1 || n.FalseChangeRate() != 0.2 {
		t.Fatalf("noise counter %+v rate %v", n, n.FalseChangeRate())
	}
	in := 0
	for i := range 10_000 {
		id := "diff_" + itoa(i)
		if AuditSample(id, 20) != AuditSample(id, 20) {
			t.Fatal("audit sample not deterministic")
		}
		if AuditSample(id, 20) {
			in++
		}
	}
	if in < 120 || in > 280 {
		t.Fatalf("audit sample of 2%% kept %d of 10000", in)
	}
}

// TestContentPatchValid: the change set of a real change is a valid
// content_changed patch.
func TestContentPatchValid(t *testing.T) {
	cs, err := Compare(spaPage(t, "v2", t0), spaPage(t, "v3", t0.AddDate(0, 0, 7)), DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := cs.Patch("snap_2", "snap_3", "diff_23")
	if err != nil {
		t.Fatal(err)
	}
	if p.Class != oam.ClassSemanticEndpoint || p.Cause != oam.CauseTarget || p.Score != cs.Score.Value || len(p.ScoreFeatures) != len(cs.Score.Reasons) {
		t.Fatalf("patch %+v", p)
	}
}

// TestContentProfileNormalize: profiles are sorted and never carry
// credentials.
func TestContentProfileNormalize(t *testing.T) {
	p, err := Profile{Name: "mobile", Method: "get", Headers: []Header{{"X-B", "2"}, {"Accept-Language", "en"}}}.Normalize()
	if err != nil || p.Method != "GET" || p.Headers[0].Name != "accept-language" {
		t.Fatalf("%+v %v", p, err)
	}
	if _, err := (Profile{Name: "x", Method: "GET", Headers: []Header{{"Cookie", "a=b"}}}).Normalize(); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("credential header accepted: %v", err)
	}
}

// TestContentIntegrityFlags: each degradation raises its own flag.
func TestContentIntegrityFlags(t *testing.T) {
	want := map[string]IntegrityFlag{
		"login_redirect": FlagLoginRedirect, "challenge": FlagChallenge, "captcha_widget_blocked": FlagChallenge,
		"error_after_success": FlagErrorAfterSuccess, "soft_404": FlagSoftError, "mass_removal": FlagMassRemoval,
		"catch_all": FlagCatchAll, "truncated": FlagTruncated, "fetch_failed": FlagFetchFailed,
	}
	for name, mutate := range degradations(t) {
		c := capture("https://shop.example/", site(t, "spa", "v1.index.html"), nil, t0)
		var opts ProcessOptions
		mutate(c, &opts)
		p := mustProcess(t, c, opts)
		if f, ok := want[name]; ok && !slices.Contains(p.Integrity.Flags, f) {
			t.Errorf("%s: flags %v, want %s", name, p.Integrity.Flags, f)
		}
	}
	// A real login form with an embedded CAPTCHA widget is not a challenge.
	ok := mustProcess(t, capture("https://shop.example/login", []byte(`<form action="/login" method="post"><div class="g-recaptcha"></div><input name="u"></form>`), nil, t0), ProcessOptions{})
	if ok.Integrity.Outcome != OutcomeComplete {
		t.Fatalf("login page with CAPTCHA widget flagged: %+v", ok.Integrity)
	}
}

// TestContentREADME keeps README.md in step with the code: every
// normalizer version and rule-table hash prefix, extractor, secret rule
// and invariant is listed, and every listed invariant has its test.
func TestContentREADME(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	for _, n := range Normalizers() {
		if !strings.Contains(readme, "`"+n.ID()+"`") || !strings.Contains(readme, n.RuleTableSHA256[:12]) {
			t.Errorf("README lacks normalizer %s (%s)", n.ID(), n.RuleTableSHA256[:12])
		}
	}
	for _, e := range Extractors() {
		if !strings.Contains(readme, "`"+e.ID()+"`") {
			t.Errorf("README lacks extractor %s", e.ID())
		}
	}
	for _, r := range SecretRuleIDs() {
		if !strings.Contains(readme, "`"+r[0]+"`") {
			t.Errorf("README lacks secret rule %s", r[0])
		}
	}
	src := ""
	files, _ := os.ReadDir(".")
	for _, f := range files {
		if strings.HasSuffix(f.Name(), "_test.go") {
			c, _ := os.ReadFile(f.Name())
			src += string(c)
		}
	}
	for _, m := range regexp.MustCompile("`(TestContentInv(\\d\\d)_\\w+)`").FindAllStringSubmatch(readme, -1) {
		if !strings.Contains(src, "func "+m[1]+"(") {
			t.Errorf("README invariant test %s does not exist", m[1])
		}
		if !strings.Contains(readme, "CONTENT-INV-"+m[2]) {
			t.Errorf("README lacks the id CONTENT-INV-%s", m[2])
		}
	}
	for _, m := range regexp.MustCompile(`func (TestContentInv\d\d_\w+)\(`).FindAllStringSubmatch(src, -1) {
		if !strings.Contains(readme, m[1]) {
			t.Errorf("invariant test %s missing from README", m[1])
		}
	}
}
