package content

import (
	"bytes"
	"encoding/json"
	"go/parser"
	"go/token"
	"math"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/testkit"
)

// CONTENT-INV-05: the diff of identical normalized artifacts is empty and
// classified "no change": no levels, no hunks, no feature changes, score
// 0, no class.
func TestContentInv05_IdenticalDiffEmpty(t *testing.T) {
	r := testkit.Rng(t)
	cases := []*Processed{spaPage(t, "v1", t0), spaPage(t, "v3", t0), spaBundle(t, 0), spaBundle(t, 2)}
	for range 20 {
		cases = append(cases, mustProcess(t, capture("https://g.example/", []byte(genHTML(r)), nil, t0), ProcessOptions{}))
	}
	for _, p := range cases {
		for _, pol := range []DiffPolicy{DiffBySize, DiffMyers, DiffHistogram, DiffPatience} {
			cs, err := Compare(p, p, DiffOptions{Policy: pol})
			if err != nil {
				t.Fatal(err)
			}
			if cs.Outcome != DiffNoChange || len(cs.Levels) != 0 || cs.Class != "" || cs.Score.Value != 0 || len(cs.Changes) != 0 || cs.IsChange() {
				t.Fatalf("%s (%s): identical captures differ: %+v", p.URL, pol, cs)
			}
			for _, d := range cs.Artifacts {
				if len(d.Hunks) != 0 || len(d.SideBySide.Hunks) != 0 || len(d.FeatureChanges) != 0 || d.Outcome != DiffNoChange {
					t.Fatalf("%s: artifact %s not empty: %+v", p.URL, d.DocID, d)
				}
			}
		}
	}
}

// CONTENT-INV-08: file-level lineage matches a renamed chunk with a few
// edits (and renamed locals) to its predecessor with every matcher, and
// never matches an unrelated bundle.
func TestContentInv08_LineageRenamedMatched(t *testing.T) {
	v1, v2, other := site(t, "spa", "index-a1b2c3d4.js"), site(t, "spa", "app-e5f6a7b8.js"), site(t, "spa", "other-0f0f0f0f.js")
	r := testkit.Rng(t)
	for _, algo := range []LineageAlgo{LineageSimHash, LineageMinHash, LineageJaccard} {
		fp := func(b []byte) Fingerprint { return FingerprintOf(LineageTokens(KindJS, b), len(b), algo) }
		prev := []LineageCandidate{
			{URL: "https://shop.example/assets/index-a1b2c3d4.js", Fingerprint: fp(v1)},
			{URL: "https://shop.example/assets/other-0f0f0f0f.js", Fingerprint: fp(other)},
		}
		m, ok := FindPredecessor("https://shop.example/assets/app-e5f6a7b8.js", fp(v2), prev, LineageOptions{Algo: algo})
		if !ok || m.URL != prev[0].URL || m.By != algo.String() {
			t.Fatalf("%s: renamed chunk not matched to its predecessor: %+v %v", algo, m, ok)
		}
		if m, ok := FindPredecessor("https://shop.example/assets/app-e5f6a7b8.js", fp(v2), prev[1:], LineageOptions{Algo: algo}); ok {
			t.Fatalf("%s: matched an unrelated bundle: %+v", algo, m)
		}
		// Generated bundles: a renamed deploy with k module edits and new
		// local names matches; another app's bundle does not.
		for range 10 {
			seed := r.Uint64()
			n := 60 + core.IntN(r, 60)
			a := genWebpackBundle(seed, n, seed, nil)
			b := []byte(genWebpackBundle(seed, n, seed+1, nil))
			for k := range 1 + core.IntN(r, 2) {
				b = bytes.Replace(b, []byte("credentials:\"include\""), []byte("credentials:\"omit\",mode:\"cors"+strconv.Itoa(k)+"\""), 1)
			}
			c := genWebpackBundle(seed+77, n, seed+5, nil)
			prev := []LineageCandidate{{URL: "https://x.example/js/723.aaaa1111.js", Fingerprint: fp([]byte(a))},
				{URL: "https://x.example/js/9.bbbb2222.js", Fingerprint: fp([]byte(strings.ReplaceAll(c, "fetch", "fetchX") + strings.Repeat("var zz=1;", 400)))}}
			m, ok := FindPredecessor("https://x.example/js/main.cccc3333.js", fp(b), prev, LineageOptions{Algo: algo})
			if !ok || m.URL != prev[0].URL {
				t.Fatalf("%s: generated renamed bundle not matched: %+v (dist to a %d)", algo, m, distance(fp(b), prev[0].Fingerprint, algo))
			}
		}
	}
	// The URL key decides first: one previous script with the same key.
	m, ok := FindPredecessor("https://shop.example/static/js/main.77e0d41a.js", Fingerprint{}, []LineageCandidate{{URL: "https://shop.example/static/js/main.3f9a1c2b.js"}}, LineageOptions{})
	if !ok || m.By != "url" {
		t.Fatalf("lineage key not used: %+v", m)
	}
}

// CONTENT-INV-09: moving or reordering bundler modules, and renaming
// their local identifiers, yields no module change and no semantic
// change; editing one module yields exactly that module as changed.
func TestContentInv09_ModuleMovesNoChange(t *testing.T) {
	r := testkit.Rng(t)
	for i := range 30 {
		seed := r.Uint64()
		n := 5 + core.IntN(r, 40)
		order := make([]int, n)
		for j := range order {
			order[j] = j
		}
		core.Shuffle(r, n, func(a, b int) { order[a], order[b] = order[b], order[a] })
		a := []byte(genWebpackBundle(seed, n, seed, nil))
		b := []byte(genWebpackBundle(seed, n, seed+uint64(i)+1, order))
		pa := mustProcess(t, capture("https://x.example/m.js", a, nil, t0), ProcessOptions{})
		pb := relabel(mustProcess(t, capture("https://x.example/m2.js", b, nil, t0), ProcessOptions{}), "https://x.example/m.js")
		for _, s := range []SplitStrategy{SplitAuto, SplitWebpack} {
			md := DiffModules(SplitModules(parseJS(a, true), SplitOptions{Strategy: s}), SplitModules(parseJS(b, true), SplitOptions{Strategy: s}))
			if len(md.Changes) != 0 {
				t.Fatalf("case %d (%s): reordered modules reported changes %+v", i, s, md.Changes)
			}
		}
		cs, err := Compare(pa, pb, DiffOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(cs.Levels, oam.LevelSemantic) || cs.Outcome == DiffChanged {
			t.Fatalf("case %d: module move is a semantic change: %+v", i, cs)
		}
		// One edited module is one change.
		c := bytes.Replace(b, []byte(`.k=[`), []byte(`.extra=1,`+`$x.k=[`), 1)
		c = bytes.Replace(c, []byte(`,$x.k=[`), []byte(`;`+string(firstLocalBefore(c))+`.k=[`), 1)
		mc := DiffModules(pa.Main().Modules, SplitModules(parseJS(c, true), SplitOptions{}))
		if len(mc.Changes) != 1 || mc.Changes[0].Op != "changed" {
			t.Fatalf("case %d: one edited module gave %+v", i, mc.Changes)
		}
	}
}

// firstLocalBefore returns the receiver name used before the first ".k=["
// of a generated bundle (the module's exports parameter).
func firstLocalBefore(b []byte) []byte {
	i := bytes.Index(b, []byte(".extra=1,"))
	j := i
	for j > 0 && (b[j-1] >= 'a' && b[j-1] <= 'z' || b[j-1] == '_') {
		j--
	}
	return b[j:i]
}

// CONTENT-INV-11: scores lie in [0, 1], equal the sum of their reasons'
// weights clamped to [0, 1], reasons are unique, sorted, named by
// lower-case identifiers with weights in [−1, 1]; removals and returns
// never raise a score. Scoring code imports nothing that can send.
func TestContentInv11_ScoreImportsNoEgress(t *testing.T) {
	r := testkit.Rng(t)
	kinds := []string{"secret", "endpoint", "form", "script", "header", "library", "link", "parameter", "sourcemap", "frame", "unknown"}
	ops := []string{"added", "removed", "modified", "returned"}
	for range testkit.Cases() {
		var cs []ScoreChange
		for range core.IntN(r, 12) {
			k := pick(r, kinds)
			key := pick(r, []string{"ep:POST https://a.example/admin/{id}", "content-security-policy", "set-cookie:sid", "https://cdn.example/x.js", "x-frame-options", "lib"})
			cs = append(cs, ScoreChange{Op: pick(r, ops), Kind: k, Key: key, Value: pick(r, []string{"POST", "1.2.0", "default-src *", ""}),
				OldValue: "default-src 'self'", Attrs: map[string]string{"third_party": pick(r, []string{"true", "false"}), "secure": "false"},
				OldAttrs: map[string]string{"secure": "true"}})
		}
		s := ScoreChanges(cs)
		sum := 0.0
		for i, rs := range s.Reasons {
			sum += rs.Weight
			if rs.Weight < -1 || rs.Weight > 1 || rs.Weight == 0 || i > 0 && s.Reasons[i-1].Name >= rs.Name {
				t.Fatalf("bad reasons %+v", s.Reasons)
			}
		}
		if s.Value < 0 || s.Value > 1 || math.Abs(s.Value-round4(math.Max(0, math.Min(1, sum)))) > 1e-12 {
			t.Fatalf("score %v inconsistent with reasons sum %v", s.Value, sum)
		}
		p := oam.ContentChangePatch{SnapshotTo: "s2", Levels: []string{oam.LevelSemantic}, Score: s.Value}
		for _, rs := range s.Reasons {
			p.ScoreFeatures = append(p.ScoreFeatures, oam.ScoreFeature{Name: rs.Name, Weight: rs.Weight})
		}
		if _, err := p.Normalize(); err != nil {
			t.Fatalf("reasons rejected by oam: %v", err)
		}
		var onlyNeg []ScoreChange
		for _, c := range cs {
			if c.Op == "returned" || c.Op == "removed" && c.Kind != "header" {
				onlyNeg = append(onlyNeg, c)
			}
		}
		if v := ScoreChanges(onlyNeg).Value; v != 0 {
			t.Fatalf("removals/returns alone scored %v", v)
		}
	}
	// Import graph: every content/score*.go imports only pure standard
	// library packages, and none of them (transitively) reaches net, os
	// or syscall.
	files, _ := filepath.Glob("score*.go")
	if len(files) == 0 {
		t.Fatal("no score*.go files")
	}
	allowed := map[string]bool{"cmp": true, "math": true, "slices": true, "strings": true, "strconv": true, "sort": true}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range af.Imports {
			p, _ := strconv.Unquote(im.Path.Value)
			if !allowed[p] {
				t.Fatalf("%s imports %s; scoring may import only %v", f, p, allowed)
			}
			out, err := exec.Command("go", "list", "-deps", p).Output()
			if err != nil {
				t.Fatalf("go list -deps %s: %v", p, err)
			}
			for _, dep := range strings.Fields(string(out)) {
				if dep == "net" || strings.HasPrefix(dep, "net/") || dep == "os" || dep == "syscall" || dep == "os/exec" || dep == "crypto/tls" {
					t.Fatalf("%s: %s reaches %s", f, p, dep)
				}
			}
		}
	}
}

// CONTENT-INV-12: determinism. The same inputs give byte-identical
// processed captures, change sets and observations, run after run.
func TestContentInv12_Determinism(t *testing.T) {
	enc := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	run := func() [][]byte {
		var out [][]byte
		var prev *Processed
		for i, v := range []string{"v1", "v2", "v3"} {
			p := spaPage(t, v, t0.AddDate(0, 0, 7*i))
			out = append(out, enc(p))
			o, err := Observe(p, ObserveOptions{Hasher: testHasher})
			if err != nil {
				t.Fatal(err)
			}
			ob, err := ObservationJSON(o)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, ob)
			if prev != nil {
				cs, err := Compare(prev, p, DiffOptions{})
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, enc(cs))
			}
			prev = p
		}
		for i := range spaBundles {
			out = append(out, enc(spaBundle(t, i)))
		}
		return out
	}
	a, b := run(), run()
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("output %d differs between runs", i)
		}
	}
}

// CONTENT-INV-15: cause. Identical raw bodies with a different
// normalizer version are a normalizer cause; with a different extractor
// set, a parser cause; different bodies are a target cause.
func TestContentInv15_Cause(t *testing.T) {
	a := spaPage(t, "v1", t0)
	b := spaPage(t, "v1", t0.AddDate(0, 0, 1))
	// Simulate a normalizer upgrade: the re-derived version has another
	// rule table, hence other normalized text.
	b.Artifacts[0].Normalized.Version = "html@2"
	b.Artifacts[0].Normalized.RuleTableSHA256 = strings.Repeat("0", 64)
	b.Artifacts[0].Normalized.Text = append(slices.Clone(b.Artifacts[0].Normalized.Text), "<x>\n"...)
	b.Artifacts[0].NormalizedSHA256 = sha256Hex(b.Artifacts[0].Normalized.Text)
	cs, err := Compare(a, b, DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cs.Cause != oam.CauseNormalizer {
		t.Fatalf("normalizer upgrade: cause %q", cs.Cause)
	}
	c := spaPage(t, "v1", t0.AddDate(0, 0, 1))
	c.Artifacts[0].Features.Grades[0].Extractor = "html.links@2"
	c.Artifacts[0].Features.Features = append(c.Artifacts[0].Features.Features, newFeature(FeatLink, "https://shop.example/new", ""))
	if cs, _ := Compare(a, c, DiffOptions{}); cs.Cause != oam.CauseParser {
		t.Fatalf("extractor upgrade: cause %q", cs.Cause)
	}
	d := spaPage(t, "v3", t0.AddDate(0, 0, 1))
	if cs, _ := Compare(a, d, DiffOptions{}); cs.Cause != oam.CauseTarget {
		t.Fatalf("target change: cause %q", cs.Cause)
	}
	if cs, _ := Compare(nil, a, DiffOptions{}); !cs.Baseline || cs.Newness != NewnessPreExisting {
		t.Fatalf("first complete capture is not a baseline: %+v", cs)
	}
}
