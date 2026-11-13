package htmlparse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// Numbered invariants HTML-INV-NN (listed in README.md "Invariants (port
// these)"). The helpers below are shared with the fuzz targets.

// checkSpansTile verifies HTML-INV-01 for a token stream over an input of
// length n.
func checkSpansTile(toks []Token, n int) error {
	if len(toks) == 0 || toks[len(toks)-1].Kind != EOFToken {
		return fmt.Errorf("token stream does not end with EOF")
	}
	pos := 0
	for i, tk := range toks {
		if tk.Span.Start != pos || tk.Span.End < tk.Span.Start {
			return fmt.Errorf("token %d (%s) span %v does not start at %d", i, tk.Kind, tk.Span, pos)
		}
		if tk.Kind == CharacterToken && tk.Data == "" {
			return fmt.Errorf("token %d is an empty character token", i)
		}
		pos = tk.Span.End
	}
	if pos != n {
		return fmt.Errorf("spans end at %d, input has %d bytes", pos, n)
	}
	return nil
}

// checkArenaIntegrity verifies HTML-INV-05: every link is in range and
// mutually consistent, sibling chains have no cycles, and every node is
// reached exactly once by walking down from the nodes without a parent
// (the root, template contents fragments, detached nodes).
func checkArenaIntegrity(d *Document) error {
	n := len(d.Nodes)
	if d.Root < 0 || int(d.Root) >= n {
		return fmt.Errorf("root %d out of range", d.Root)
	}
	if d.Nodes[d.Root].Parent != None {
		return fmt.Errorf("root has a parent")
	}
	in := func(id NodeID) bool { return id == None || (id >= 0 && int(id) < n) }
	contentOwner := make([]NodeID, n)
	for i := range contentOwner {
		contentOwner[i] = None
	}
	for i := range d.Nodes {
		nd := &d.Nodes[i]
		for _, l := range []NodeID{nd.Parent, nd.FirstChild, nd.LastChild, nd.PrevSibling, nd.NextSibling, nd.Content} {
			if !in(l) {
				return fmt.Errorf("node %d has out-of-range link %d", i, l)
			}
		}
		if (nd.FirstChild == None) != (nd.LastChild == None) {
			return fmt.Errorf("node %d: first/last child disagree", i)
		}
		if nd.Parent == None && (nd.PrevSibling != None || nd.NextSibling != None) {
			return fmt.Errorf("node %d: siblings without parent", i)
		}
		if nd.Content != None {
			if d.Nodes[nd.Content].Kind != DocumentFragmentNode || d.Nodes[nd.Content].Parent != None {
				return fmt.Errorf("node %d: template contents %d is not a detached fragment", i, nd.Content)
			}
			if contentOwner[nd.Content] != None {
				return fmt.Errorf("fragment %d is the contents of two templates", nd.Content)
			}
			contentOwner[nd.Content] = NodeID(i)
		}
		if nd.Kind == TextNode || nd.Kind == CommentNode || nd.Kind == DoctypeNode || nd.Kind == ProcessingInstructionNode {
			if nd.FirstChild != None {
				return fmt.Errorf("leaf node %d (%s) has children", i, nd.Kind)
			}
		}
	}
	seen := make([]bool, n)
	for i := range d.Nodes {
		if d.Nodes[i].Parent != None {
			continue
		}
		// Walk this tree iteratively.
		stack := []NodeID{NodeID(i)}
		for len(stack) > 0 {
			id := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if seen[id] {
				return fmt.Errorf("node %d reached twice (cycle or shared child)", id)
			}
			seen[id] = true
			prev := None
			steps := 0
			for c := d.Nodes[id].FirstChild; c != None; c = d.Nodes[c].NextSibling {
				if steps++; steps > n {
					return fmt.Errorf("sibling cycle under %d", id)
				}
				if d.Nodes[c].Parent != id {
					return fmt.Errorf("child %d of %d has parent %d", c, id, d.Nodes[c].Parent)
				}
				if d.Nodes[c].PrevSibling != prev {
					return fmt.Errorf("child %d of %d: prev link %d, want %d", c, id, d.Nodes[c].PrevSibling, prev)
				}
				prev = c
				stack = append(stack, c)
			}
			if d.Nodes[id].LastChild != prev {
				return fmt.Errorf("node %d: last child %d, want %d", id, d.Nodes[id].LastChild, prev)
			}
		}
	}
	for i, s := range seen {
		if !s {
			return fmt.Errorf("node %d unreachable", i)
		}
	}
	return nil
}

// checkCoalesced verifies HTML-INV-10: no Text node is immediately
// followed by another Text node.
func checkCoalesced(d *Document) error {
	for i := range d.Nodes {
		nd := &d.Nodes[i]
		if nd.Kind == TextNode && nd.NextSibling != None && d.Nodes[nd.NextSibling].Kind == TextNode {
			return fmt.Errorf("adjacent text nodes %d and %d (%q, %q)", i, nd.NextSibling, nd.Data, d.Nodes[nd.NextSibling].Data)
		}
	}
	return nil
}

// checkErrors verifies HTML-INV-08: errors are ordered by offset, within
// the input, and their line/column match the offset.
func checkErrors(input []byte, errs []ParseError) error {
	prev := 0
	for i, e := range errs {
		if e.Offset < prev || e.Offset > len(input) {
			return fmt.Errorf("error %d (%s) offset %d out of order/range (prev %d, len %d)", i, e.Code, e.Offset, prev, len(input))
		}
		prev = e.Offset
		line, col := naiveLineCol(input, e.Offset)
		if line != e.Line || col != e.Col {
			return fmt.Errorf("error %d (%s) at %d: line/col %d:%d, want %d:%d", i, e.Code, e.Offset, e.Line, e.Col, line, col)
		}
	}
	return nil
}

func naiveLineCol(in []byte, off int) (int, int) {
	line, col := 1, 1
	for i := 0; i < off; {
		switch {
		case in[i] == '\r':
			line, col = line+1, 1
			if i+1 < len(in) && in[i+1] == '\n' && i+1 < off {
				i++
			}
			i++
		case in[i] == '\n':
			if i > 0 && in[i-1] == '\r' {
				i++ // second half of CR LF, already counted
				continue
			}
			line, col = line+1, 1
			i++
		default:
			r, w := decodeRune(in[i:])
			col++
			if r >= 0x10000 {
				col++
			}
			i += w
		}
	}
	return line, col
}

// checkAttrs verifies HTML-INV-09: no element has two attributes with the
// same (namespace, name), nor more than max attributes.
func checkAttrs(d *Document, max int) error {
	for i := range d.Nodes {
		as := d.Nodes[i].Attrs
		if len(as) > max && d.Nodes[i].Kind == ElementNode {
			// html/body may gain attributes from later tags; each token
			// is still limited, so allow up to 2x for those.
			if name := d.Nodes[i].Name; name != "html" && name != "body" || len(as) > 2*max {
				return fmt.Errorf("node %d has %d attributes (max %d)", i, len(as), max)
			}
		}
		seen := map[Attr]bool{}
		for _, a := range as {
			k := Attr{Namespace: a.Namespace, Name: a.Name}
			if seen[k] {
				return fmt.Errorf("node %d has duplicate attribute %q", i, a.Name)
			}
			seen[k] = true
		}
	}
	return nil
}

// corpus returns the .dat inputs plus generated soup, the shared input
// set of the invariant tests.
func corpus(t *testing.T, r core.Rng) [][]byte {
	var out [][]byte
	for _, c := range loadDatSuite(t) {
		out = append(out, []byte(c.Data))
	}
	for range testkit.Cases() {
		out = append(out, genTagSoup(r, 20+core.IntN(r, 600)))
	}
	return out
}

// randomBytes draws input biased towards markup-significant bytes.
func randomBytes(r core.Rng, n int) []byte {
	const alphabet = "<>/!?-&#;=\"' \t\n\r\x00abcdeftrsvgmhxyzABCDEF[]CDATA0123456789\xc3\xa9\xed\xa0\x80\xef\xbf\xbe\x80\xff"
	b := make([]byte, n)
	for i := range b {
		if core.IntN(r, 8) == 0 {
			b[i] = byte(r.Uint64())
		} else {
			b[i] = alphabet[core.IntN(r, len(alphabet))]
		}
	}
	return b
}

// TestHTMLInv01_TokenSpansTileInput: the token spans start at 0, are
// contiguous and end at len(input), for every initial state.
func TestHTMLInv01_TokenSpansTileInput(t *testing.T) {
	r := testkit.Rng(t)
	inputs := corpus(t, r)
	for range testkit.Cases() {
		inputs = append(inputs, randomBytes(r, core.IntN(r, 200)))
	}
	states := []TokenizerState{DataState, RCDATAState, RAWTEXTState, ScriptDataState, PLAINTEXTState, CDATASectionState}
	for _, in := range inputs {
		st := states[core.IntN(r, len(states))]
		toks, _ := Tokenize(in, TokenizeOptions{InitialState: st, AllowCDATA: core.IntN(r, 2) == 0})
		if err := checkSpansTile(toks, len(in)); err != nil {
			t.Fatalf("state %d input %q: %v", st, in, err)
		}
	}
}

// TestHTMLInv02_ParsingIsTotal: Tokenize, Parse and ParseFragment accept
// any byte string, and Parse always yields a document element.
func TestHTMLInv02_ParsingIsTotal(t *testing.T) {
	r := testkit.Rng(t)
	ctxs := []ContextElement{{Name: "body"}, {Name: "td"}, {Name: "template"}, {Name: "select"},
		{Name: "textarea"}, {Namespace: SVGNamespace, Name: "svg"}, {Namespace: MathMLNamespace, Name: "annotation-xml"}}
	for range 2 * testkit.Cases() {
		in := randomBytes(r, core.IntN(r, 400))
		if _, errs := Tokenize(in, TokenizeOptions{}); checkErrors(in, errs) != nil {
			t.Fatalf("%q: %v", in, checkErrors(in, errs))
		}
		doc, _ := Parse(in, Options{Scripting: core.IntN(r, 2) == 0})
		if doc.DocumentElement() == None || doc.Nodes[doc.DocumentElement()].Name != "html" {
			t.Fatalf("%q: no html document element", in)
		}
		frag, _ := ParseFragment(in, ctxs[core.IntN(r, len(ctxs))], Options{})
		if frag.Nodes[frag.Root].Kind != DocumentFragmentNode {
			t.Fatalf("%q: fragment root is %s", in, frag.Nodes[frag.Root].Kind)
		}
	}
}

// roundTripSafe reports whether doc is in the class of trees that
// serialize-then-reparse must reproduce (HTML-INV-03): no element whose
// content model the serializer cannot express (raw text, plaintext,
// tables and their foster parenting, forms, select, template, foreign
// content, implied-end-tag and formatting-scope elements), no comment
// containing "--", and no depth-limit truncation.
func roundTripSafe(d *Document, errs []ParseError) bool {
	for _, e := range errs {
		if e.Code == ErrNestingDepthLimit || e.Code == ErrAttributeLimit || e.Code == ErrInputSizeLimit {
			return false
		}
	}
	for i := range d.Nodes {
		nd := &d.Nodes[i]
		// A CR can only come from &#13;; the serializer writes it raw and
		// the reparse normalizes it to LF (a spec non-round-trip case).
		if strings.Contains(nd.Data, "\r") {
			return false
		}
		for _, a := range nd.Attrs {
			if strings.Contains(a.Value, "\r") {
				return false
			}
		}
		switch nd.Kind {
		case CommentNode:
			if strings.Contains(nd.Data, "--") || strings.HasSuffix(nd.Data, "-") || strings.HasPrefix(nd.Data, ">") || strings.HasPrefix(nd.Data, "->") {
				return false
			}
		case ProcessingInstructionNode:
			return false
		case DoctypeNode:
			// The serializer writes only the name (HTML §13.3).
			if nd.PublicID != "" || nd.SystemID != "" {
				return false
			}
		case ElementNode:
			if nd.Namespace != HTMLNamespace {
				return false
			}
			switch nd.Name {
			case "html", "head", "body", "div", "span", "b", "i", "em", "strong", "code", "small", "u", "s",
				"section", "article", "aside", "nav", "main", "header", "footer", "blockquote", "img", "br", "hr",
				"meta", "link", "base", "x", "custom-el", "label", "center", "address", "details", "summary",
				"dialog", "figure", "wbr", "embed", "param", "area", "keygen", "big", "tt", "font", "strike", "ruby":
			default:
				return false
			}
		}
	}
	return true
}

// TestHTMLInv03_SerializeReparseFixpoint: for round-trip-safe trees
// (well-formed generated documents and the safe subset of tag soup),
// Parse(Render(Parse(x))) has the same dump as Parse(x).
func TestHTMLInv03_SerializeReparseFixpoint(t *testing.T) {
	r := testkit.Rng(t)
	check := func(in []byte, mustBeSafe bool) bool {
		d1, errs := Parse(in, Options{})
		if !roundTripSafe(d1, errs) && !mustBeSafe {
			return false
		}
		out := Render(d1, d1.Root)
		d2, _ := Parse(out, Options{})
		if Dump(d1) != Dump(d2) {
			t.Fatalf("not a fixpoint\ninput %q\nrendered %q\n#first\n%s\n#second\n%s", in, out, Dump(d1), Dump(d2))
		}
		return true
	}
	for range testkit.Cases() / 4 {
		check(genWellFormed(r, 200+core.IntN(r, 3000)), true)
	}
	safe := 0
	for range 4 * testkit.Cases() {
		if check(genTagSoup(r, 20+core.IntN(r, 200)), false) {
			safe++
		}
	}
	if safe < testkit.Cases()/20 {
		t.Fatalf("only %d round-trip-safe soup inputs; the class is too small to test", safe)
	}
	t.Logf("%d safe soup inputs checked", safe)
}

// TestHTMLInv04_LinearTimeAndAllocation: parsing 4n bytes costs at most
// 20%% more per byte than n bytes, in allocations (count and bytes) and,
// outside -race and -short, in time (with a looser bound for timer noise).
func TestHTMLInv04_LinearTimeAndAllocation(t *testing.T) {
	r := core.NewSplitMix64(7)
	// Inputs are built by repeating a unit so composition is identical at
	// both sizes: a well-formed block and a tag-soup block (whose unclosed
	// tags drive the stack to the depth limit, the worst realistic case).
	wf := genWellFormed(r, 4096)
	wfBody := wf[bytes.Index(wf, []byte("<body>"))+6 : bytes.LastIndex(wf, []byte("</body>"))]
	soup := genTagSoup(r, 4096)
	units := map[string][]byte{"wellformed": wfBody, "soup": soup}
	for name, unit := range units {
		t.Run(name, func(t *testing.T) {
			mk := func(k int) []byte { return bytes.Repeat(unit, k) }
			small, large := mk(16), mk(64)
			allocs := func(in []byte) (float64, uint64) {
				var ms0, ms1 runtime.MemStats
				runtime.GC()
				runtime.ReadMemStats(&ms0)
				n := testing.AllocsPerRun(3, func() { Parse(in, Options{}) })
				runtime.ReadMemStats(&ms1)
				return n, (ms1.TotalAlloc - ms0.TotalAlloc) / 4 // AllocsPerRun runs once extra
			}
			aS, bS := allocs(small)
			aL, bL := allocs(large)
			ratioA := (aL / float64(len(large))) / (aS / float64(len(small)))
			ratioB := (float64(bL) / float64(len(large))) / (float64(bS) / float64(len(small)))
			t.Logf("allocs/byte ratio %.3f, bytes/byte ratio %.3f", ratioA, ratioB)
			if ratioA > 1.2 || ratioB > 1.2 {
				t.Fatalf("allocation is not linear: allocs ratio %.3f, bytes ratio %.3f (bound 1.2)", ratioA, ratioB)
			}
			if raceEnabled || testing.Short() {
				return
			}
			// Small and large runs alternate, so load from other test
			// processes (go test -p N) slows both alike; the minimum per
			// size over many runs then damps scheduler and GC noise. The
			// bound is on the ratio, so it holds on any machine speed.
			ns := func(in []byte) time.Duration {
				start := time.Now()
				Parse(in, Options{})
				return time.Since(start)
			}
			best := 1e9
			for range 3 {
				bestS, bestL := time.Duration(1<<62), time.Duration(1<<62)
				for range 9 {
					bestS = min(bestS, ns(small))
					bestL = min(bestL, ns(large))
				}
				best = min(best, (float64(bestL)/float64(len(large)))/(float64(bestS)/float64(len(small))))
			}
			t.Logf("time/byte ratio %.3f", best)
			if best > 1.5 {
				t.Fatalf("time is not linear: ratio %.3f (bound 1.5)", best)
			}
		})
	}
}

// TestHTMLInv05_ArenaIntegrity: parser output satisfies checkArenaIntegrity
// for every .dat case (document and fragment) and generated soup.
func TestHTMLInv05_ArenaIntegrity(t *testing.T) {
	for _, c := range loadDatSuite(t) {
		opts := Options{Scripting: c.ScriptOn}
		var doc *Document
		if c.HasFrag {
			doc, _ = ParseFragment([]byte(c.Data), parseContext(c.Fragment), opts)
		} else {
			doc, _ = Parse([]byte(c.Data), opts)
		}
		if err := checkArenaIntegrity(doc); err != nil {
			t.Fatalf("%s: %v", c.ID, err)
		}
	}
	r := testkit.Rng(t)
	for range testkit.Cases() {
		in := genTagSoup(r, 20+core.IntN(r, 800))
		doc, _ := Parse(in, Options{})
		if err := checkArenaIntegrity(doc); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
	}
}

// domDepth returns the maximum depth of the tree under root.
func domDepth(d *Document, root NodeID) int {
	type item struct {
		id    NodeID
		depth int
	}
	best := 0
	stack := []item{{root, 0}}
	for len(stack) > 0 {
		it := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		best = max(best, it.depth)
		for c := range d.Children(it.id) {
			stack = append(stack, item{c, it.depth + 1})
		}
	}
	return best
}

// TestHTMLInv06_DepthLimit: the stack of open elements never exceeds
// Options.MaxDepth; exceeding it reports ErrNestingDepthLimit, keeps
// every element (inserted childless) and keeps parsing.
func TestHTMLInv06_DepthLimit(t *testing.T) {
	const max = 64
	in := []byte(strings.Repeat("<div>", 5000) + "deep" + strings.Repeat("</div>", 5000) + "<p>after")
	doc, errs, b := parseDocument(in, Options{MaxDepth: max})
	if b.maxStack > max {
		t.Fatalf("stack reached %d, limit %d", b.maxStack, max)
	}
	n := 0
	for range doc.Elements(doc.Root, HTMLNamespace, "div") {
		n++
	}
	if n != 5000 {
		t.Fatalf("%d div elements, want 5000", n)
	}
	limitErrs := 0
	for _, e := range errs {
		if e.Code == ErrNestingDepthLimit {
			limitErrs++
		}
	}
	if limitErrs == 0 {
		t.Fatal("no nesting-depth-limit-exceeded error")
	}
	if d := domDepth(doc, doc.Root); d > max+2 {
		t.Fatalf("DOM depth %d exceeds limit %d (+document, +childless element)", d, max)
	}
	var p NodeID = None
	for id := range doc.Elements(doc.Root, HTMLNamespace, "p") {
		p = id
	}
	if p == None || doc.TextContent(p) != "after" {
		t.Fatal("parsing did not continue after the limit")
	}
	if err := checkArenaIntegrity(doc); err != nil {
		t.Fatal(err)
	}
	r := testkit.Rng(t)
	for range testkit.Cases() {
		lim := 2 + core.IntN(r, 12)
		in := genTagSoup(r, 50+core.IntN(r, 500))
		_, _, b := parseDocument(in, Options{MaxDepth: lim})
		if b.maxStack > lim {
			t.Fatalf("limit %d: stack reached %d on %q", lim, b.maxStack, in)
		}
	}
}

// TestHTMLInv07_DecodeTokenizeEquivalence: for valid UTF-8 x, Decode
// returns x with or without a UTF-8 BOM (and with a utf-8 transport
// label), and tokenizing either result gives identical tokens.
func TestHTMLInv07_DecodeTokenizeEquivalence(t *testing.T) {
	r := testkit.Rng(t)
	for _, x := range corpus(t, r) {
		if !utf8Valid(x) {
			continue
		}
		plain, e1, _ := Decode(x, "utf-8")
		bom, e2, c2 := Decode(append([]byte{0xEF, 0xBB, 0xBF}, x...), "")
		if e1 != EncodingUTF8 || e2 != EncodingUTF8 || c2 != ConfidenceCertain {
			t.Fatalf("encodings %s/%s confidence %s", e1, e2, c2)
		}
		if !bytes.Equal(plain, x) || !bytes.Equal(bom, x) {
			t.Fatalf("decode changed valid UTF-8 input %q", x)
		}
		t1, _ := Tokenize(plain, TokenizeOptions{})
		t2, _ := Tokenize(bom, TokenizeOptions{})
		j1, _ := json.Marshal(t1)
		j2, _ := json.Marshal(t2)
		if !bytes.Equal(j1, j2) {
			t.Fatalf("token streams differ for %q", x)
		}
	}
}

// TestHTMLInv08_ErrorsOrderedAndLocated: parse errors are sorted by
// offset, lie within the input, and carry the line/column of their
// offset.
func TestHTMLInv08_ErrorsOrderedAndLocated(t *testing.T) {
	r := testkit.Rng(t)
	inputs := corpus(t, r)
	for range testkit.Cases() {
		inputs = append(inputs, randomBytes(r, core.IntN(r, 300)))
	}
	for _, in := range inputs {
		_, errs := Parse(in, Options{})
		if err := checkErrors(in, errs); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
	}
}

// TestHTMLInv09_AttributesUniqueAndLimited: elements never carry
// duplicate attributes, and MaxAttributes caps each tag (reporting
// ErrAttributeLimit).
func TestHTMLInv09_AttributesUniqueAndLimited(t *testing.T) {
	var b strings.Builder
	b.WriteString("<a")
	for i := range 3000 {
		fmt.Fprintf(&b, " x%d=%d x%d", i, i, i%7)
	}
	b.WriteString(">")
	doc, errs := Parse([]byte(b.String()), Options{MaxAttributes: 100})
	if err := checkAttrs(doc, 100); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range errs {
		found = found || e.Code == ErrAttributeLimit
	}
	if !found {
		t.Fatal("no attribute-limit-exceeded error")
	}
	r := testkit.Rng(t)
	for _, in := range corpus(t, r) {
		doc, _ := Parse(in, Options{MaxAttributes: 4})
		if err := checkAttrs(doc, 4); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
	}
}

// TestHTMLInv10_TextCoalesced: the parser never leaves two adjacent Text
// siblings ("insert a character" appends to a preceding Text node).
func TestHTMLInv10_TextCoalesced(t *testing.T) {
	r := testkit.Rng(t)
	for _, in := range corpus(t, r) {
		doc, _ := Parse(in, Options{})
		if err := checkCoalesced(doc); err != nil {
			t.Fatalf("%q: %v", in, err)
		}
	}
}

// TestHTMLInv11_NamedReferenceLongestMatch: every lookup strategy returns
// the longest entity name that prefixes the input (brute force over the
// table), and conformance/htmlparse/entities.json equals the table.
func TestHTMLInv11_NamedReferenceLongestMatch(t *testing.T) {
	r := testkit.Rng(t)
	brute := func(b []byte) int {
		best := 0
		for _, n := range entityNames {
			if len(n) > best && bytes.HasPrefix(b, []byte(n)) {
				best = len(n)
			}
		}
		return best
	}
	for range 5 * testkit.Cases() {
		name := entityNames[core.IntN(r, entityCount)]
		cut := core.IntN(r, len(name)+1)
		b := []byte(name[:cut] + string("abc;xyZ19"[core.IntN(r, 9)]) + "rest")
		got, idx := lookupEntity(b)
		if want := brute(b); got != want {
			t.Fatalf("%q: longest match %d, want %d", b, got, want)
		}
		for _, st := range CharRefStrategies() {
			if tl, ti := st.lookup(b); tl != got || ti != idx {
				t.Fatalf("%q: %s strategy gives (%d, %d), sorted search (%d, %d)", b, st, tl, ti, got, idx)
			}
		}
		if got > 0 && entityNames[idx] != string(b[:got]) {
			t.Fatalf("%q: index %d names %q", b, idx, entityNames[idx])
		}
	}
	raw := testkit.ReadFile(t, "htmlparse", "entities.json")
	var m map[string]struct {
		Codepoints []rune `json:"codepoints"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m) != entityCount {
		t.Fatalf("entities.json has %d entries, table %d", len(m), entityCount)
	}
	for i, n := range entityNames {
		cps := m["&"+n].Codepoints
		want := []rune{entityValues[i][0]}
		if entityValues[i][1] != 0 {
			want = append(want, entityValues[i][1])
		}
		if fmt.Sprint(cps) != fmt.Sprint(want) {
			t.Fatalf("%s: json %v, table %v", n, cps, want)
		}
	}
}

func utf8Valid(b []byte) bool {
	for i := 0; i < len(b); {
		r, w := decodeRune(b[i:])
		if r == 0xFFFD && w == 1 || isSurrogate(r) {
			return false
		}
		i += w
	}
	return true
}
