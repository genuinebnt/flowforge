package cssparse

import (
	"encoding/json"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

// upstreamFiles are the vendored css-parsing-tests files and the "parse a
// ..." entry point each one exercises.
var upstreamFiles = []string{
	"component_value_list", "one_component_value", "declaration_list",
	"one_declaration", "blocks_contents", "rule_list", "one_rule", "stylesheet",
}

// TestCSSConformanceUpstream runs the vendored css-parsing-tests suite
// (CC0, github.com/SimonSapin/css-parsing-tests). The suite predates the
// removal of unicode-range and match tokens, so it runs with
// LegacyTokens; every case must pass.
func TestCSSConformanceUpstream(t *testing.T) {
	total := 0
	for _, name := range upstreamFiles {
		raw := testkit.ReadFile(t, "third_party/css-parsing-tests", name+".json")
		var items []any
		if err := json.Unmarshal(raw, &items); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(items)%2 != 0 {
			t.Fatalf("%s: odd number of items", name)
		}
		for i := 0; i < len(items); i += 2 {
			input, ok := items[i].(string)
			if !ok {
				t.Fatalf("%s[%d]: input is not a string", name, i/2)
			}
			total++
			got, _, err := runParser(name, []byte(input), Options{LegacyTokens: true})
			if err != nil {
				t.Errorf("%s[%d] %q: %v", name, i/2, input, err)
				continue
			}
			if !jsonEqual(got, items[i+1]) {
				g, _ := json.Marshal(got)
				w, _ := json.Marshal(items[i+1])
				t.Errorf("%s[%d] %q:\n got  %s\n want %s", name, i/2, input, g, w)
			}
		}
	}
	if total < 100 {
		t.Fatalf("only %d upstream cases ran", total)
	}
	t.Logf("%d upstream css-parsing-tests cases", total)
}

// ownCase is one case of conformance/cssparse/*.json.
type ownCase struct {
	Name    string `json:"name"`
	Parser  string `json:"parser"`
	Input   string `json:"input"`
	Options struct {
		Legacy        bool `json:"legacy"`
		MaxDepth      int  `json:"max_depth"`
		MaxInputBytes int  `json:"max_input_bytes"`
		MaxURLs       int  `json:"max_urls"`
	} `json:"options"`
	// Expected is the css-parsing-tests representation of the result
	// (when Parser is set).
	Expected any `json:"expected"`
	// URLs lists the expected URL references (when Parser is "urls").
	URLs []struct {
		URL     string `json:"url"`
		Form    string `json:"form"`
		Context string `json:"context"`
		Text    string `json:"text"`
	} `json:"urls"`
	// Diags, when present, is the exact list of diagnostic codes.
	Diags []string `json:"diags"`
	// Grade, when present, is the expected grade.
	Grade string `json:"grade"`
}

type ownFile struct {
	Description string    `json:"description"`
	Cases       []ownCase `json:"cases"`
}

func (c *ownCase) opts() Options {
	return Options{LegacyTokens: c.Options.Legacy, MaxDepth: c.Options.MaxDepth,
		MaxInputBytes: c.Options.MaxInputBytes, MaxURLs: c.Options.MaxURLs}
}

// TestCSSConformanceOwn runs conformance/cssparse: current-spec tokens
// (no legacy tokens), URL extraction, limits and grades.
func TestCSSConformanceOwn(t *testing.T) {
	n := 0
	for _, f := range testkit.LoadCases[ownFile](t, "cssparse") {
		for _, c := range f.Data.Cases {
			n++
			t.Run(f.Name+"/"+c.Name, func(t *testing.T) {
				src := []byte(c.Input)
				var diags []Diagnostic
				var grade Grade
				if c.Parser == "urls" {
					res := ExtractURLs(src, c.opts())
					diags, grade = res.Diags, res.Grade
					if len(res.Refs) != len(c.URLs) {
						t.Fatalf("got %d refs %+v, want %d", len(res.Refs), res.Refs, len(c.URLs))
					}
					for i, w := range c.URLs {
						g := res.Refs[i]
						if g.URL != w.URL || g.Form.String() != w.Form || g.Context.String() != w.Context {
							t.Errorf("ref %d: got %q %s %s, want %q %s %s", i, g.URL, g.Form, g.Context, w.URL, w.Form, w.Context)
						}
						if w.Text != "" && string(src[g.Start:g.End]) != w.Text {
							t.Errorf("ref %d: span text %q, want %q", i, src[g.Start:g.End], w.Text)
						}
					}
				} else {
					got, d, err := runParser(c.Parser, src, c.opts())
					if err != nil {
						t.Fatal(err)
					}
					if !jsonEqual(got, c.Expected) {
						g, _ := json.Marshal(got)
						w, _ := json.Marshal(c.Expected)
						t.Errorf("got  %s\nwant %s", g, w)
					}
					diags, grade = d.Diags, d.Grade()
				}
				if c.Diags != nil {
					var codes []string
					for _, d := range diags {
						codes = append(codes, d.Code.String())
					}
					if len(codes) == 0 {
						codes = []string{}
					}
					if g, w := mustJSON(codes), mustJSON(c.Diags); g != w {
						t.Errorf("diags %s, want %s", g, w)
					}
				}
				if c.Grade != "" && grade.String() != c.Grade {
					t.Errorf("grade %s, want %s", grade, c.Grade)
				}
			})
		}
	}
	if n < 30 {
		t.Fatalf("only %d own cases", n)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
