package content

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/testkit"
)

type sniffFile struct {
	Cases []struct {
		Name        string `json:"name"`
		Body        string `json:"body"`
		ContentType string `json:"content_type"`
		URL         string `json:"url"`
		Kind        Kind   `json:"kind"`
		Mismatch    bool   `json:"mismatch"`
		Disclosure  string `json:"disclosure"`
	} `json:"cases"`
}

// TestContentConformanceSniff checks conformance/content/sniff.
func TestContentConformanceSniff(t *testing.T) {
	for _, f := range testkit.LoadCases[sniffFile](t, "content/sniff") {
		for _, c := range f.Data.Cases {
			r := Sniff([]byte(c.Body), c.ContentType, c.URL)
			if r.Kind != c.Kind || r.Mismatch != c.Mismatch || r.Disclosure != c.Disclosure {
				t.Errorf("%s: got %+v, want kind %s mismatch %v disclosure %q", c.Name, r, c.Kind, c.Mismatch, c.Disclosure)
			}
		}
	}
}

type secretsFile struct {
	Cases []struct {
		Name     string `json:"name"`
		Text     string `json:"text"`
		Findings []struct {
			RuleID  string     `json:"rule_id"`
			Tier    SecretTier `json:"tier"`
			Preview string     `json:"preview"`
		} `json:"findings"`
		Redacted string `json:"redacted"`
	} `json:"cases"`
}

// TestContentConformanceSecrets checks conformance/content/secrets.
func TestContentConformanceSecrets(t *testing.T) {
	for _, f := range testkit.LoadCases[secretsFile](t, "content/secrets") {
		for _, c := range f.Data.Cases {
			got := ScanSecrets([]byte(c.Text), "")
			if len(got) != len(c.Findings) {
				t.Errorf("%s: %d findings %+v, want %d", c.Name, len(got), got, len(c.Findings))
				continue
			}
			for i, w := range c.Findings {
				if got[i].RuleID != w.RuleID || got[i].Tier != w.Tier || got[i].Preview != w.Preview {
					t.Errorf("%s[%d]: got %s/%s/%s, want %s/%s/%s", c.Name, i, got[i].RuleID, got[i].Tier, got[i].Preview, w.RuleID, w.Tier, w.Preview)
				}
			}
			if red, _ := Redact([]byte(c.Text)); string(red) != c.Redacted {
				t.Errorf("%s: redacted %q, want %q", c.Name, red, c.Redacted)
			}
		}
	}
}

type normCase struct {
	Name        string      `json:"name"`
	Kind        Kind        `json:"kind"`
	Input       string      `json:"input"`
	Expected    string      `json:"expected"`
	Grade       Grade       `json:"grade"`
	Masks       []MaskCount `json:"masks,omitempty"`
	Derived     []string    `json:"derived,omitempty"` // "suffix kind"
	Diagnostics []string    `json:"diagnostics,omitempty"`
}

type normFile struct {
	Description string     `json:"description"`
	Cases       []normCase `json:"cases"`
}

// TestContentConformanceNormalize checks conformance/content/normalize:
// exact normalized output, grade, mask counts and derived documents.
func TestContentConformanceNormalize(t *testing.T) {
	for _, f := range testkit.LoadCases[normFile](t, "content/normalize") {
		for _, c := range f.Data.Cases {
			got := normCaseOf(c.Name, c.Kind, c.Input)
			if got.Expected != c.Expected || got.Grade != c.Grade || !slices.Equal(got.Masks, c.Masks) ||
				!slices.Equal(got.Derived, c.Derived) || !slices.Equal(got.Diagnostics, c.Diagnostics) {
				t.Errorf("%s:\ngot  %+v\nwant %+v", c.Name, got, c)
			}
		}
	}
}

func normCaseOf(name string, k Kind, input string) normCase {
	n := Normalize(k, []byte(input), NormalizeOptions{})
	c := normCase{Name: name, Kind: k, Input: input, Expected: string(n.Text), Grade: n.Grade, Masks: n.Masks, Diagnostics: n.Diagnostics}
	for _, d := range n.Derived {
		c.Derived = append(c.Derived, d.Suffix+" "+string(d.Kind))
	}
	return c
}

// TestContentConformanceNormalizers pins every normalizer's version and
// rule-table hash (conformance/content/normalizers): a rule change
// without a version bump fails here.
func TestContentConformanceNormalizers(t *testing.T) {
	got, _ := json.MarshalIndent(Normalizers(), "", "  ")
	writeExpected(t, "content/normalizers", "normalizers.json", append(got, '\n'))
	want := testkit.ReadFile(t, "content/normalizers", "normalizers.json")
	if strings.TrimSpace(string(want)) != strings.TrimSpace(string(got)) {
		t.Fatalf("normalizer table changed; bump the version of the changed normalizer and regenerate with FF_CONTENT_UPDATE=1\n%s", got)
	}
}

// spaExpected is conformance/content/spa/expected.json.
type spaExpected struct {
	Description string             `json:"description"`
	Captures    []capExpected      `json:"captures"`
	Transitions []changeExpected   `json:"transitions"`
	Lineage     []lineageExpected  `json:"lineage"`
	Modules     []modulesExpected  `json:"modules"`
	Observation []observationCheck `json:"observations"`
}

type capExpected struct {
	Name         string        `json:"name"`
	Coverage     oam.Coverage  `json:"coverage"`
	CoveredRules []string      `json:"covered_rules"`
	Outcome      Outcome       `json:"outcome"`
	Features     []string      `json:"features"`
	Artifacts    []artExpected `json:"artifacts"`
}

type artExpected struct {
	DocID            string `json:"doc_id"`
	Kind             Kind   `json:"kind"`
	Normalizer       string `json:"normalizer"`
	NormalizedSHA256 string `json:"normalized_sha256"`
	Grade            Grade  `json:"grade"`
}

type changeExpected struct {
	From          string   `json:"from"`
	To            string   `json:"to"`
	Outcome       string   `json:"outcome"`
	Levels        []string `json:"levels"`
	Class         string   `json:"class"`
	Newness       string   `json:"newness"`
	Cause         string   `json:"cause"`
	Score         float64  `json:"score"`
	ScoreFeatures []string `json:"score_features"`
	Changes       []string `json:"changes"`
}

type lineageExpected struct {
	New  string `json:"new"`
	Algo string `json:"algo"`
	URL  string `json:"url"`
	By   string `json:"by"`
}

type modulesExpected struct {
	From             string   `json:"from"`
	To               string   `json:"to"`
	Moved            int      `json:"moved"`
	VendorSuppressed int      `json:"vendor_suppressed"`
	Changes          []string `json:"changes"`
}

type observationCheck struct {
	Name     string       `json:"name"`
	Coverage oam.Coverage `json:"coverage"`
	Edges    []string     `json:"edges"`
	Tags     []string     `json:"tags"`
}

// spaRun processes the fixture site: three page deploys and three bundle
// deploys, and computes everything the expected file pins.
func spaRun(t *testing.T) spaExpected {
	var out spaExpected
	out.Description = "The SPA fixture (conformance/content/sites/spa): page deploys v1→v2 (only volatile values, formatting and build hashes change: masked_only) and v2→v3 (planted: a POST form, a third-party script without SRI, a leaked AWS key pair, CSP loosened, X-Frame-Options removed, HttpOnly dropped); bundle deploys main.3f9a1c2b → main.77e0d41a (modules reordered, locals renamed, re-formatted: reordered) → main.5b6c7d8e (one new admin endpoint in module 101, tinylib 1.2.0 → 1.3.0 in vendor module 104). Generated by TestContentConformanceSPA with FF_CONTENT_UPDATE=1 and reviewed by hand."
	pages := map[string]*Processed{}
	for i, v := range []string{"v1", "v2", "v3"} {
		pages[v] = spaPage(t, v, t0.AddDate(0, 0, 7*i))
	}
	bundles := map[string]*Processed{}
	for i, b := range spaBundles {
		bundles[b] = spaBundle(t, i)
	}
	capOf := func(name string, p *Processed) capExpected {
		c := capExpected{Name: name, Coverage: p.Coverage, CoveredRules: p.CoveredRules, Outcome: p.Integrity.Outcome, Features: p.FeatureIDs()}
		for _, a := range p.Artifacts {
			c.Artifacts = append(c.Artifacts, artExpected{DocID: a.DocID, Kind: a.Kind, Normalizer: a.Normalized.Version,
				NormalizedSHA256: a.NormalizedSHA256, Grade: a.Normalized.Grade})
		}
		return c
	}
	for _, v := range []string{"v1", "v2", "v3"} {
		out.Captures = append(out.Captures, capOf("page "+v, pages[v]))
	}
	for _, b := range spaBundles {
		out.Captures = append(out.Captures, capOf("bundle "+b, bundles[b]))
	}
	change := func(from, to string, a, b *Processed) changeExpected {
		cs, err := Compare(a, b, DiffOptions{})
		if err != nil {
			t.Fatal(err)
		}
		c := changeExpected{From: from, To: to, Outcome: cs.Outcome, Levels: cs.Levels, Class: cs.Class, Newness: cs.Newness, Cause: cs.Cause, Score: cs.Score.Value}
		for _, r := range cs.Score.Reasons {
			c.ScoreFeatures = append(c.ScoreFeatures, r.Name+"="+strconvF(r.Weight))
		}
		for _, ch := range cs.Changes {
			c.Changes = append(c.Changes, ch.Op+" "+ch.Feature.ID())
		}
		return c
	}
	out.Transitions = append(out.Transitions, change("page v1", "page v2", pages["v1"], pages["v2"]),
		change("page v2", "page v3", pages["v2"], pages["v3"]),
		change("bundle "+spaBundles[0], "bundle "+spaBundles[1], bundles[spaBundles[0]], bundles[spaBundles[1]]),
		change("bundle "+spaBundles[1], "bundle "+spaBundles[2], bundles[spaBundles[1]], bundles[spaBundles[2]]))
	for i := 1; i < len(spaBundles); i++ {
		md := DiffModules(bundles[spaBundles[i-1]].Main().Modules, bundles[spaBundles[i]].Main().Modules)
		m := modulesExpected{From: spaBundles[i-1], To: spaBundles[i], Moved: md.Moved, VendorSuppressed: md.VendorSuppressed}
		for _, c := range md.Changes {
			m.Changes = append(m.Changes, c.Op+" "+c.ID)
		}
		out.Modules = append(out.Modules, m)
	}
	vite := map[string][]byte{"index-a1b2c3d4.js": site(t, "spa", "index-a1b2c3d4.js"), "other-0f0f0f0f.js": site(t, "spa", "other-0f0f0f0f.js")}
	for _, algo := range []LineageAlgo{LineageSimHash, LineageMinHash, LineageJaccard} {
		var prev []LineageCandidate
		for _, n := range []string{"index-a1b2c3d4.js", "other-0f0f0f0f.js"} {
			prev = append(prev, LineageCandidate{URL: "https://shop.example/assets/" + n, Fingerprint: FingerprintOf(LineageTokens(KindJS, vite[n]), len(vite[n]), algo)})
		}
		nb := site(t, "spa", "app-e5f6a7b8.js")
		m, _ := FindPredecessor("https://shop.example/assets/app-e5f6a7b8.js", FingerprintOf(LineageTokens(KindJS, nb), len(nb), algo), prev, LineageOptions{Algo: algo})
		out.Lineage = append(out.Lineage, lineageExpected{New: "app-e5f6a7b8.js", Algo: algo.String(), URL: m.URL, By: m.By})
	}
	obs := func(name string, p *Processed, pred *LineageMatch) observationCheck {
		o, err := Observe(p, ObserveOptions{Hasher: testHasher, Predecessor: pred})
		if err != nil {
			t.Fatal(err)
		}
		c := observationCheck{Name: name, Coverage: o.Authority.Coverage}
		key := map[string]string{}
		for _, e := range o.Entities {
			k, _ := e.Asset.Key()
			key[e.Ref] = k
		}
		for _, e := range o.Edges {
			c.Edges = append(c.Edges, key[e.From]+" -"+e.Relation.RelationLabel()+"-> "+key[e.To])
		}
		for _, tg := range o.Tags {
			c.Tags = append(c.Tags, key[tg.Target]+" "+tg.Property.PropertyType()+":"+tg.Property.PropertyName())
		}
		slices.Sort(c.Edges)
		slices.Sort(c.Tags)
		return c
	}
	out.Observation = append(out.Observation, obs("page v3", pages["v3"], nil),
		obs("bundle "+spaBundles[2], relabel(bundles[spaBundles[2]], "https://shop.example/static/js/"+spaBundles[2]),
			&LineageMatch{URL: "https://shop.example/static/js/" + spaBundles[1], By: "url"}))
	return out
}

func strconvF(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// TestContentConformanceSPA checks the fixture site against
// conformance/content/spa/expected.json: features, normalized hashes,
// classifications, scores, module diffs, lineage and observations.
func TestContentConformanceSPA(t *testing.T) {
	got := spaRun(t)
	gb, _ := json.MarshalIndent(got, "", "  ")
	writeExpected(t, "content/spa", "expected.json", append(gb, '\n'))
	want := testkit.LoadCases[spaExpected](t, "content/spa")[0].Data
	wb, _ := json.MarshalIndent(want, "", "  ")
	if string(gb) != string(wb) {
		t.Fatalf("SPA conformance differs:\n%s", firstDiff(wb, gb))
	}
}

// TestContentConformanceNormalizeUpdate regenerates the expected outputs
// of conformance/content/normalize from the reviewed inputs (only with
// FF_CONTENT_UPDATE=1 and FF_CONTENT_NORM_INPUTS set).
func TestContentConformanceNormalizeUpdate(t *testing.T) {
	path := os.Getenv("FF_CONTENT_NORM_INPUTS")
	if os.Getenv("FF_CONTENT_UPDATE") != "1" || path == "" {
		t.Skip("set FF_CONTENT_UPDATE=1 and FF_CONTENT_NORM_INPUTS to regenerate")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var in []normCase
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatal(err)
	}
	f := normFile{Description: "Normalize(kind, input) exact outputs, grades, mask counts and derived documents (V2 §5, §12.7, §12.8; D2). Inputs hand-written; outputs generated and reviewed by hand."}
	for _, c := range in {
		f.Cases = append(f.Cases, normCaseOf(c.Name, c.Kind, c.Input))
	}
	out, _ := json.MarshalIndent(f, "", "  ")
	writeExpected(t, "content/normalize", "cases.json", append(out, '\n'))
}
