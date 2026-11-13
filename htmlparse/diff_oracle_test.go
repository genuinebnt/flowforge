package htmlparse

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// Known deviation classes of golang.org/x/net/html v0.62 from the current
// standard (documented in OPEN_QUESTIONS.md, section "Differential
// oracle"). The .dat expectation is the authority: a deviation is only
// acceptable when the oracle is the side that disagrees with it (for .dat
// inputs) and the input exhibits the class's trigger.
type oracleClass struct {
	id      string
	trigger func(in []byte) bool
}

func containsFold(in []byte, s string) bool {
	return bytes.Contains(bytes.ToLower(in), []byte(s))
}

var oracleClasses = []oracleClass{
	// D1: "<?" is a processing instruction (2025); x/net makes a bogus comment.
	{"D1-processing-instructions", func(in []byte) bool { return bytes.Contains(in, []byte("<?")) }},
	// D2: <search> is a special block element; x/net treats it as ordinary.
	{"D2-search-element", func(in []byte) bool { return containsFold(in, "search") }},
	// D3: "parsing template contents" includes a template fragment context
	// and forms inside template tables; x/net uses the form pointer there.
	{"D3-template-form-pointer", func(in []byte) bool {
		return containsFold(in, "form") && containsFold(in, "template")
	}},
	// D4: NUL in foreign content / CDATA at HTML integration points and its
	// frameset-ok effect.
	{"D4-foreign-nul", func(in []byte) bool {
		return bytes.IndexByte(in, 0) >= 0 && (containsFold(in, "svg") || containsFold(in, "math"))
	}},
	// D5: selectedcontent mirrors the selected option (DOM side effect).
	{"D5-selectedcontent", func(in []byte) bool { return containsFold(in, "selectedcontent") }},
	// D6: x/net decodes character references in comment and CDATA data.
	{"D6-charref-in-comment-or-cdata", func(in []byte) bool {
		return bytes.Contains(in, []byte("<!")) && bytes.IndexByte(in, '&') >= 0
	}},
	// D8: x/net's tokenizer enters RAWTEXT/RCDATA/script data by tag name
	// even where the tree builder ignores the start tag (frameset modes).
	{"D8-rawtext-by-tag-name", func(in []byte) bool {
		if !containsFold(in, "frameset") {
			return false
		}
		for _, t := range []string{"<script", "<style", "<xmp", "<iframe", "<noembed", "<noframes", "<textarea", "<title", "<noscript", "<plaintext"} {
			if containsFold(in, t) {
				return true
			}
		}
		return false
	}},
}

func classifyOracleDeviation(in []byte) string {
	for _, c := range oracleClasses {
		if c.trigger(in) {
			return c.id
		}
	}
	return ""
}

// normalizeForOracle removes the representational difference D7: x/net
// keeps the foreign "xmlns" attribute in no namespace, the standard (and
// html5lib's dump) puts it in the XMLNS namespace.
func normalizeForOracle(dump string) string {
	return strings.ReplaceAll(dump, "xmlns xmlns=", "xmlns=")
}

func TestHTMLDiffOracle(t *testing.T) {
	t.Run("dat", func(t *testing.T) {
		classes := map[string]int{}
		agree := 0
		for _, c := range loadDatSuite(t) {
			if c.ScriptOn {
				continue // x/net and we both run script-on cases without executing scripts; covered by the conformance test
			}
			var oracle string
			var ok bool
			if c.HasFrag {
				oracle, ok = oracleParseFragment([]byte(c.Data), parseContext(c.Fragment))
			} else {
				oracle, ok = oracleParse([]byte(c.Data))
			}
			if !ok {
				t.Fatalf("%s: oracle failed", c.ID)
			}
			ours := runDatCase(c)
			known := slicesContains(knownTreeFailures, c.ID)
			if oracle == c.Document && ours != c.Document && !known {
				t.Errorf("%s: oracle matches the expectation but we do not", c.ID)
			}
			if normalizeForOracle(ours) == normalizeForOracle(oracle) {
				agree++
				continue
			}
			if known {
				continue
			}
			cls := classifyOracleDeviation([]byte(c.Data))
			if cls == "" {
				t.Errorf("%s: unclassified oracle deviation\n#data\n%s\n#ours\n%s\n#oracle\n%s", c.ID, c.Data, ours, oracle)
				continue
			}
			classes[cls]++
		}
		t.Logf(".dat: %d agree with x/net/html; deviations by class: %s", agree, formatCounts(classes))
	})
	t.Run("soup", func(t *testing.T) {
		r := testkit.Rng(t)
		classes := map[string]int{}
		n := testkit.Cases() * 3
		agree := 0
		for i := 0; i < n; i++ {
			in := genTagSoup(r, 40+core.IntN(r, 400))
			doc, _ := Parse(in, Options{})
			ours := normalizeForOracle(Dump(doc))
			oracle, ok := oracleParse(in)
			if !ok {
				t.Fatalf("oracle failed on %q", in)
			}
			if ours == normalizeForOracle(oracle) {
				agree++
				continue
			}
			cls := classifyOracleDeviation(in)
			if cls == "" {
				t.Fatalf("unclassified deviation on %q\n#ours\n%s\n#oracle\n%s", in, ours, oracle)
			}
			classes[cls]++
		}
		t.Logf("tag soup: %d/%d identical to x/net/html; deviations by class: %s", agree, n, formatCounts(classes))
	})
	t.Run("soup-fragments", func(t *testing.T) {
		r := testkit.Rng(t)
		contexts := []ContextElement{
			{Namespace: HTMLNamespace, Name: "div"}, {Namespace: HTMLNamespace, Name: "td"},
			{Namespace: HTMLNamespace, Name: "tr"}, {Namespace: HTMLNamespace, Name: "table"},
			{Namespace: HTMLNamespace, Name: "select"}, {Namespace: HTMLNamespace, Name: "ul"},
			{Namespace: SVGNamespace, Name: "svg"}, {Namespace: MathMLNamespace, Name: "mi"},
			{Namespace: HTMLNamespace, Name: "body"}, {Namespace: HTMLNamespace, Name: "html"},
		}
		classes := map[string]int{}
		n := testkit.Cases()
		agree := 0
		for i := 0; i < n; i++ {
			ctx := contexts[core.IntN(r, len(contexts))]
			in := genTagSoup(r, 40+core.IntN(r, 300))
			doc, _ := ParseFragment(in, ctx, Options{})
			ours := normalizeForOracle(Dump(doc))
			oracle, ok := oracleParseFragment(in, ctx)
			if !ok {
				t.Fatalf("oracle failed on %q", in)
			}
			if ours == normalizeForOracle(oracle) {
				agree++
				continue
			}
			cls := classifyOracleDeviation(in)
			if cls == "" {
				t.Fatalf("unclassified deviation (context %s) on %q\n#ours\n%s\n#oracle\n%s", ctx.Name, in, ours, oracle)
			}
			classes[cls]++
		}
		t.Logf("fragment soup: %d/%d identical; deviations by class: %s", agree, n, formatCounts(classes))
	})
}

func slicesContains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func formatCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%d ", k, m[k])
	}
	if b.Len() == 0 {
		return "none"
	}
	return b.String()
}
