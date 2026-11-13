package htmlparse

import (
	"bytes"
	"fmt"
	"os"
	"regexp"
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
	trigger func(in []byte, ctx string) bool
}

func containsFold(in []byte, s string) bool {
	return bytes.Contains(bytes.ToLower(in), []byte(s))
}

var oracleClasses = []oracleClass{
	// D1: "<?" is a processing instruction (2025); x/net makes a bogus comment.
	{"D1-processing-instructions", func(in []byte, ctx string) bool { return bytes.Contains(in, []byte("<?")) }},
	// D2: <search> is a special block element; x/net treats it as ordinary.
	{"D2-search-element", func(in []byte, ctx string) bool { return containsFold(in, "search") }},
	// D3: "parsing template contents" includes a template fragment context
	// and forms inside template tables; x/net uses the form pointer there.
	{"D3-template-form-pointer", func(in []byte, ctx string) bool {
		return containsFold(in, "form") && (containsFold(in, "template") || ctx == "template")
	}},
	// D4: NUL in foreign content / CDATA at HTML integration points and its
	// frameset-ok effect.
	{"D4-foreign-nul", func(in []byte, ctx string) bool {
		return bytes.IndexByte(in, 0) >= 0 && (containsFold(in, "svg") || containsFold(in, "math"))
	}},
	// D5: selectedcontent mirrors the selected option (DOM side effect).
	{"D5-selectedcontent", func(in []byte, ctx string) bool { return containsFold(in, "selectedcontent") }},
	// D6: x/net decodes character references in comment and CDATA data.
	{"D6-charref-in-comment-or-cdata", func(in []byte, ctx string) bool { return ampInMarkupDeclaration(in) }},
	// D8: x/net's tokenizer enters RAWTEXT/RCDATA/script data by tag name
	// even where the tree builder ignores the start tag (frameset modes).
	{"D8-rawtext-by-tag-name", func(in []byte, ctx string) bool {
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
	// D10: x/net handles a character token that starts with whitespace and
	// continues with other text as a unit in "in head noscript" (it pops
	// the noscript first); the standard processes each character, so the
	// whitespace stays inside the noscript element.
	{"D10-head-noscript-whitespace", func(in []byte, ctx string) bool { return containsFold(in, "<noscript") }},
	// D11: x/net drops the LF after <pre>/<listing>/<textarea> even when
	// another token (an ignored end tag, a comment) comes first; the
	// standard only drops it if the very next token is the LF.
	{"D11-pre-newline-not-next-token", func(in []byte, ctx string) bool { return preNewlineAfterToken.Match(in) }},
	// D12: x/net does not merge foster-parented (or template-contents)
	// text with an adjacent Text node; "insert a character" appends to the
	// previous sibling Text node.
	{"D12-text-not-coalesced", func(in []byte, ctx string) bool {
		for _, t := range []string{"<table", "<tbody", "<thead", "<tfoot", "<tr", "<td", "<th", "<caption", "<col", "<template"} {
			if containsFold(in, t) {
				return true
			}
		}
		return tableContexts[ctx]
	}},
	// D13: x/net's fragment parser does not use a foreign (SVG/MathML)
	// context element as the adjusted current node: it applies the HTML
	// rules at the top level and treats <![CDATA[ as a bogus comment.
	{"D13-foreign-fragment-context", func(in []byte, ctx string) bool { return foreignContexts[ctx] }},
	// D14: x/net loses <template> start tags processed at SVG/MathML
	// integration points.
	{"D14-template-in-foreign", func(in []byte, ctx string) bool {
		return containsFold(in, "<template") && (containsFold(in, "<svg") || containsFold(in, "<math"))
	}},
	// D15: in "in column group" (also via template contents) x/net drops
	// the whole character token when the first character is not
	// whitespace; the standard ignores only the non-whitespace characters.
	{"D15-colgroup-mixed-text", func(in []byte, ctx string) bool { return containsFold(in, "<col") }},
	// D16: x/net's "generate implied end tags" matches tag names without
	// the HTML-namespace condition, so a foreign <option>, <p>, <li>... is
	// closed like an HTML one.
	{"D16-namespace-insensitive-implied-end-tags", func(in []byte, ctx string) bool {
		if !containsFold(in, "<svg") && !containsFold(in, "<math") {
			return false
		}
		for _, t := range []string{"<option", "<optgroup", "<li", "<dd", "<dt", "<p", "<rb", "<rt", "<rp"} {
			if containsFold(in, t) {
				return true
			}
		}
		return false
	}},
	// D17: with a table-section fragment context, x/net mishandles a cell
	// start tag that closes a caption.
	{"D17-table-fragment-caption", func(in []byte, ctx string) bool {
		return tableContexts[ctx] && containsFold(in, "<caption")
	}},
}

var foreignContexts = map[string]bool{"svg": true, "mi": true}
var tableContexts = map[string]bool{"table": true, "tbody": true, "thead": true, "tfoot": true, "tr": true, "td": true, "th": true, "caption": true, "colgroup": true}

var preNewlineAfterToken = regexp.MustCompile(`(?i)<(pre|listing|textarea)\b[^>]*>(<[^>]*>)+\r?\n`)

// ampInMarkupDeclaration reports a '&' inside a comment, bogus comment or
// CDATA section ("<!...>").
func ampInMarkupDeclaration(in []byte) bool {
	for i := 0; i < len(in); i++ {
		if in[i] != '<' || i+1 >= len(in) || in[i+1] != '!' {
			continue
		}
		rest := in[i:]
		end := []byte(">")
		switch {
		case bytes.HasPrefix(rest, []byte("<!--")):
			end = []byte("-->")
		case bytes.HasPrefix(rest, []byte("<![CDATA[")):
			end = []byte("]]>")
		}
		j := bytes.Index(rest[2:], end)
		if j < 0 {
			j = len(rest) - 2
		}
		if bytes.IndexByte(rest[:2+j], '&') >= 0 {
			return true
		}
	}
	return false
}

func classifyOracleDeviation(in []byte, ctx string) string {
	for _, c := range oracleClasses {
		if c.trigger(in, ctx) {
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
			cls := classifyOracleDeviation([]byte(c.Data), c.Fragment)
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
			cls := classifyOracleDeviation(in, "")
			if verbose() && classes[cls] == 0 {
				t.Logf("example %s: %q\n#ours\n%s\n#oracle\n%s", cls, in, ours, oracle)
			}
			if cls == "" && os.Getenv("FF_HTML_DIFF_OUT") != "" {
				dumpDeviation("", in)
				continue
			}
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
				// D9: x/net/html's fragment parser panics on some inputs
				// (it recovers and returns the panic as an error); there
				// is nothing to compare.
				if !strings.Contains(oracle, "runtime error") {
					t.Fatalf("oracle failed (context %s) on %q: %s", ctx.Name, in, oracle)
				}
				classes["D9-oracle-panic"]++
				continue
			}
			if ours == normalizeForOracle(oracle) {
				agree++
				continue
			}
			cls := classifyOracleDeviation(in, ctx.Name)
			if cls == "" && os.Getenv("FF_HTML_DIFF_OUT") != "" {
				dumpDeviation(ctx.Name, in)
				continue
			}
			if cls == "" {
				t.Fatalf("unclassified deviation (context %s) on %q\n#ours\n%s\n#oracle\n%s", ctx.Name, in, ours, oracle)
			}
			classes[cls]++
		}
		t.Logf("fragment soup: %d/%d identical; deviations by class: %s", agree, n, formatCounts(classes))
	})
}

func verbose() bool { return os.Getenv("FF_HTML_VERBOSE") != "" }

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

// dumpDeviation appends an unclassified deviation to $FF_HTML_DIFF_OUT
// (a triage aid; the test fails without it).
func dumpDeviation(ctx string, in []byte) {
	f, err := os.OpenFile(os.Getenv("FF_HTML_DIFF_OUT"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s\t%q\n", ctx, in)
}
