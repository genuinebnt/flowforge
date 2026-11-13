package xmlparse

import (
	"bytes"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// --- document generator -------------------------------------------------

type gattr struct {
	prefix, local, uri, value string
}

type gnode struct {
	kind           NodeKind
	prefix, local  string
	uri            string
	decls          []gattr // namespace declarations (prefix "" = default)
	attrs          []gattr
	children       []*gnode
	text           string
	target, pidata string
}

var (
	gLocals   = []string{"a", "b", "item", "x-y", "z.1", "\u00e9l", "\u4e2d", "_k"}
	gPrefixes = []string{"p", "q", "ns1"}
	gURIs     = []string{"urn:a", "urn:b", "http://example.com/ns", ""}
	gChars    = []string{"a", " ", "&", "<", ">", "\"", "'", "\t", "\n", "\r", "\u00e9", "\U0001F600", "]]>", "]", "-", "?", "x"}
)

type gen struct {
	r core.Rng
}

func (g *gen) pick(xs []string) string { return xs[core.IntN(g.r, len(xs))] }

func (g *gen) str(n int) string {
	var b strings.Builder
	for i := core.IntN(g.r, n+1); i > 0; i-- {
		b.WriteString(g.pick(gChars))
	}
	return b.String()
}

// element builds a random element under the in-scope bindings.
func (g *gen) element(scope map[string]string, depth int) *gnode {
	n := &gnode{kind: NodeElement, local: g.pick(gLocals)}
	inner := map[string]string{}
	for k, v := range scope {
		inner[k] = v
	}
	// Maybe declare namespaces here.
	for i := core.IntN(g.r, 3); i > 0; i-- {
		p := ""
		if core.IntN(g.r, 3) > 0 {
			p = g.pick(gPrefixes)
		}
		uri := g.pick(gURIs)
		if p != "" && uri == "" {
			uri = "urn:c"
		}
		dup := false
		for _, d := range n.decls {
			dup = dup || d.prefix == p
		}
		if dup {
			continue
		}
		n.decls = append(n.decls, gattr{prefix: p, uri: uri})
		inner[p] = uri
	}
	// Element prefix: one in scope, or none.
	var bound []string
	for k := range inner {
		if k != "" {
			bound = append(bound, k)
		}
	}
	sort.Strings(bound)
	if len(bound) > 0 && core.IntN(g.r, 2) == 0 {
		n.prefix = bound[core.IntN(g.r, len(bound))]
	}
	n.uri = inner[n.prefix]
	// Attributes with distinct expanded names.
	seen := map[string]bool{}
	for i := core.IntN(g.r, 4); i > 0; i-- {
		a := gattr{local: g.pick(gLocals), value: g.str(4)}
		if len(bound) > 0 && core.IntN(g.r, 2) == 0 {
			a.prefix = bound[core.IntN(g.r, len(bound))]
			a.uri = inner[a.prefix]
		}
		key := a.uri + "|" + a.local
		if a.prefix == "" {
			key = "|" + a.local
		}
		if seen[key] || (a.prefix == "" && a.local == "xmlns") {
			continue
		}
		seen[key] = true
		n.attrs = append(n.attrs, a)
	}
	if depth > 0 {
		for i := core.IntN(g.r, 4); i > 0; i-- {
			switch core.IntN(g.r, 6) {
			case 0, 1:
				n.children = append(n.children, g.element(inner, depth-1))
			case 2, 3:
				n.children = append(n.children, &gnode{kind: NodeText, text: g.str(6)})
			case 4:
				n.children = append(n.children, g.comment())
			default:
				n.children = append(n.children, g.pi())
			}
		}
	}
	return n
}

func (g *gen) comment() *gnode {
	s := strings.NewReplacer("-", "_", "\r", "").Replace(g.str(5))
	return &gnode{kind: NodeComment, text: s}
}

func (g *gen) pi() *gnode {
	s := strings.NewReplacer("?", "", "\r", "").Replace(g.str(5))
	s = strings.TrimLeft(s, " \t\n")
	return &gnode{kind: NodePI, target: "t" + g.pick([]string{"", "x", "-1"}), pidata: s}
}

// write serializes the model with random but well-formed choices.
func (g *gen) write(b *bytes.Buffer, n *gnode) {
	switch n.kind {
	case NodeElement:
		q := n.local
		if n.prefix != "" {
			q = n.prefix + ":" + n.local
		}
		b.WriteString("<" + q)
		var all []gattr
		for _, d := range n.decls {
			if d.prefix == "" {
				all = append(all, gattr{local: "xmlns", value: d.uri})
			} else {
				all = append(all, gattr{prefix: "xmlns", local: d.prefix, value: d.uri})
			}
		}
		all = append(all, n.attrs...)
		core.Shuffle(g.r, len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
		for _, a := range all {
			b.WriteString(g.pick([]string{" ", "\n", " \t"}))
			if a.prefix != "" {
				b.WriteString(a.prefix + ":")
			}
			b.WriteString(a.local)
			b.WriteString(g.pick([]string{"=", " = ", "=\n"}))
			quote := g.pick([]string{"\"", "'"})
			b.WriteString(quote)
			g.escapeAttr(b, a.value, quote)
			b.WriteString(quote)
		}
		if len(n.children) == 0 && core.IntN(g.r, 2) == 0 {
			b.WriteString(g.pick([]string{"/>", " />"}))
			return
		}
		b.WriteString(">")
		for _, c := range n.children {
			g.write(b, c)
		}
		b.WriteString("</" + q + g.pick([]string{">", " >"}))
	case NodeText:
		if !strings.ContainsAny(n.text, "\r") && !strings.Contains(n.text, "]]>") && core.IntN(g.r, 3) == 0 {
			b.WriteString("<![CDATA[" + n.text + "]]>")
			return
		}
		for i := 0; i < len(n.text); i++ {
			c := n.text[i]
			switch {
			case c == '&':
				b.WriteString(g.pick([]string{"&amp;", "&#38;", "&#x26;"}))
			case c == '<':
				b.WriteString(g.pick([]string{"&lt;", "&#60;"}))
			case c == '>':
				b.WriteString(g.pick([]string{"&gt;", "&#62;"}))
			case c == '\r':
				b.WriteString("&#13;")
			default:
				b.WriteByte(c)
			}
		}
	case NodeComment:
		b.WriteString("<!--" + n.text + "-->")
	case NodePI:
		b.WriteString("<?" + n.target)
		if n.pidata != "" {
			b.WriteString(" " + n.pidata)
		}
		b.WriteString("?>")
	}
}

func (g *gen) escapeAttr(b *bytes.Buffer, v, quote string) {
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c == '&':
			b.WriteString("&amp;")
		case c == '<':
			b.WriteString("&lt;")
		case string(c) == quote:
			if quote == "\"" {
				b.WriteString(g.pick([]string{"&quot;", "&#34;"}))
			} else {
				b.WriteString(g.pick([]string{"&apos;", "&#39;"}))
			}
		case c == '\t':
			b.WriteString("&#9;")
		case c == '\n':
			b.WriteString("&#10;")
		case c == '\r':
			b.WriteString("&#xD;")
		default:
			b.WriteByte(c)
		}
	}
}

// canonical writes the model's expected canonical form.
func canonicalModel(b *strings.Builder, n *gnode) {
	switch n.kind {
	case NodeElement:
		q := n.local
		if n.prefix != "" {
			q = n.prefix + ":" + n.local
		}
		b.WriteString("<" + q)
		decls := append([]gattr(nil), n.decls...)
		sort.Slice(decls, func(i, j int) bool {
			if (decls[i].prefix == "") != (decls[j].prefix == "") {
				return decls[i].prefix == ""
			}
			return decls[i].prefix < decls[j].prefix
		})
		for _, d := range decls {
			if d.prefix == "" {
				b.WriteString(" xmlns=\"")
			} else {
				b.WriteString(" xmlns:" + d.prefix + "=\"")
			}
			escapeAttr(b, d.uri)
			b.WriteString("\"")
		}
		attrs := append([]gattr(nil), n.attrs...)
		sort.SliceStable(attrs, func(i, j int) bool {
			if attrs[i].uri != attrs[j].uri {
				return attrs[i].uri < attrs[j].uri
			}
			return attrs[i].local < attrs[j].local
		})
		for _, a := range attrs {
			b.WriteString(" ")
			if a.prefix != "" {
				b.WriteString(a.prefix + ":")
			}
			b.WriteString(a.local + "=\"")
			escapeAttr(b, a.value)
			b.WriteString("\"")
		}
		b.WriteString(">")
		for _, c := range n.children {
			canonicalModel(b, c)
		}
		b.WriteString("</" + q + ">")
	case NodeText:
		escapeText(b, n.text)
	case NodeComment:
		b.WriteString("<!--" + n.text + "-->")
	case NodePI:
		b.WriteString("<?" + n.target)
		if n.pidata != "" {
			b.WriteString(" " + n.pidata)
		}
		b.WriteString("?>")
	}
}

// genDoc returns a well-formed document and its expected canonical form.
func genDoc(r core.Rng, depth int) ([]byte, string) {
	g := &gen{r: r}
	var b bytes.Buffer
	var c strings.Builder
	if core.IntN(r, 2) == 0 {
		b.WriteString(g.pick([]string{`<?xml version="1.0"?>`, `<?xml version='1.0' encoding='UTF-8' standalone='no'?>`}))
	}
	var top []*gnode
	for i := core.IntN(r, 2); i > 0; i-- {
		top = append(top, g.comment())
	}
	if core.IntN(r, 3) == 0 {
		b.WriteString("\n<!DOCTYPE " + g.pick(gLocals) + ">")
	}
	top = append(top, g.element(map[string]string{}, depth))
	if core.IntN(r, 2) == 0 {
		top = append(top, g.pi())
	}
	for i, n := range top {
		b.WriteString(g.pick([]string{"", "\n", " \r\n"}))
		g.write(&b, n)
		if i > 0 {
			c.WriteString("\n")
		}
		canonicalModel(&c, n)
	}
	b.WriteString(g.pick([]string{"", "\n"}))
	return b.Bytes(), c.String()
}

// mutate applies one random byte edit.
func mutate(r core.Rng, src []byte) []byte {
	out := append([]byte(nil), src...)
	if len(out) == 0 {
		return []byte{byte(r.Uint64())}
	}
	i := core.IntN(r, len(out))
	ins := []byte{'<', '>', '&', '/', '"', '\'', '=', ':', '!', '?', '[', ']', '-', ';', '#', 'x', ' ', 0x01, 0xff, 0xc3, 0xe2}
	switch core.IntN(r, 4) {
	case 0:
		out[i] = ins[core.IntN(r, len(ins))]
	case 1:
		out = append(out[:i], out[i+1:]...)
	case 2:
		out = append(out[:i], append([]byte{ins[core.IntN(r, len(ins))]}, out[i:]...)...)
	default:
		j := core.IntN(r, len(out))
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// --- invariants ---------------------------------------------------------

func checkDoc(t testing.TB, src []byte, doc *Document, err error) {
	t.Helper()
	if err != nil {
		se, ok := err.(*SyntaxError)
		if !ok {
			t.Fatalf("error %T is not *SyntaxError", err)
		}
		if se.Offset < 0 || (!doc.UTF16 && se.Offset > len(src)) {
			t.Fatalf("offset %d outside input of %d bytes", se.Offset, len(src))
		}
		if se.Code.EOFClass() && !doc.UTF16 && se.Offset != len(src) {
			t.Fatalf("EOF-class %s at %d, input %d", se.Code, se.Offset, len(src))
		}
	}
	if doc.Grade != GradeOf(err) {
		t.Fatalf("grade %s for error %v", doc.Grade, err)
	}
	for id := range doc.Nodes {
		n := &doc.Nodes[id]
		last := NoNode
		for c := n.FirstChild; c != NoNode; c = doc.Nodes[c].NextSibling {
			if doc.Nodes[c].Parent != NodeID(id) {
				t.Fatalf("bad parent link")
			}
			last = c
		}
		if last != n.LastChild {
			t.Fatalf("bad last child")
		}
	}
}

// TestXMLInv01_Totality: any byte string (random, or a mutated
// well-formed document) yields a tree and nil or a *SyntaxError with an
// in-range offset, EOF-class errors at the input length, and a grade that
// matches the error.
func TestXMLInv01_Totality(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		src, _ := genDoc(r, 3)
		for k := core.IntN(r, 4); k >= 0; k-- {
			src = mutate(r, src)
		}
		doc, err := Parse(src, Options{})
		checkDoc(t, src, doc, err)
		raw := make([]byte, core.IntN(r, 64))
		for j := range raw {
			raw[j] = byte(r.Uint64())
		}
		doc, err = Parse(raw, Options{})
		checkDoc(t, raw, doc, err)
	}
}

func parseErr(src []byte) *SyntaxError {
	_, err := Parse(src, Options{})
	if err == nil {
		return nil
	}
	return err.(*SyntaxError)
}

// TestXMLInv02_FirstOffendingByte: errors land on the first offending
// byte. Every prefix of a well-formed document is error-free or ends in
// an EOF-class error; for a mutated document whose error is at k, the
// prefix [:k] has no non-EOF error, and [:k+1] (alone or followed by
// garbage) gives the same code at k.
func TestXMLInv02_FirstOffendingByte(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		src, _ := genDoc(r, 3)
		for _, cut := range []int{core.IntN(r, len(src)+1), core.IntN(r, len(src)+1), len(src) - 1} {
			if cut < 0 {
				continue
			}
			if e := parseErr(src[:cut]); e != nil && !e.Code.EOFClass() {
				t.Fatalf("prefix %q of well-formed doc: %v", src[:cut], e)
			}
		}
		m := src
		for k := core.IntN(r, 3); k >= 0; k-- {
			m = mutate(r, m)
		}
		e := parseErr(m)
		if e == nil || e.Code.EOFClass() || len(m) >= 2 && (m[0] == 0xFE || m[0] == 0xFF) {
			continue
		}
		k := e.Offset
		if p := parseErr(m[:k]); p != nil && !p.Code.EOFClass() {
			t.Fatalf("%q: error %v at %d, but prefix [:%d] already fails with %v", m, e.Code, k, k, p)
		}
		p := parseErr(m[:k+1])
		if p == nil || p.Code != e.Code || p.Offset != k {
			t.Fatalf("%q: error %v at %d, prefix [:%d] gives %v", m, e.Code, k, k+1, p)
		}
		g := append(append([]byte(nil), m[:k+1]...), "<x a='1'>&amp;</x>"...)
		if q := parseErr(g); q == nil || q.Code != e.Code || q.Offset != k {
			t.Fatalf("%q: error %v at %d not independent of what follows: %v", m, e.Code, k, q)
		}
	}
}

// TestXMLInv03_DepthCap: nesting deeper than MaxDepth stops with
// depth_limit (grade partial) and no tree path is longer than MaxDepth.
func TestXMLInv03_DepthCap(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		depth := core.IntN(r, 12) + 1
		max := core.IntN(r, 12) + 1
		var b bytes.Buffer
		for j := 0; j < depth; j++ {
			b.WriteString("<e>")
		}
		for j := 0; j < depth; j++ {
			b.WriteString("</e>")
		}
		doc, err := Parse(b.Bytes(), Options{MaxDepth: max})
		if (depth > max) != (err != nil) {
			t.Fatalf("depth %d max %d: err %v", depth, max, err)
		}
		if err != nil && (err.(*SyntaxError).Code != ErrDepthLimit || doc.Grade != GradePartial) {
			t.Fatalf("depth %d max %d: %v grade %s", depth, max, err, doc.Grade)
		}
		for id := range doc.Nodes {
			d := 0
			for p := NodeID(id); p > 0; p = doc.Nodes[p].Parent {
				d++
			}
			if d > max {
				t.Fatalf("node at depth %d > %d", d, max)
			}
		}
	}
	// Generated documents with random structure keep the bound as well.
	for i := 0; i < testkit.Cases()/10; i++ {
		src, _ := genDoc(r, 6)
		doc, err := Parse(src, Options{MaxDepth: 3})
		if err != nil && err.(*SyntaxError).Code != ErrDepthLimit {
			t.Fatalf("%q: %v", src, err)
		}
		_ = doc
	}
}

// TestXMLInv04_CanonicalRoundTrip: a generated well-formed document,
// written with random escaping, quoting, CDATA and whitespace choices,
// parses to the model's canonical form, and the canonical form is a fixed
// point of parse-then-canonicalize.
func TestXMLInv04_CanonicalRoundTrip(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		src, want := genDoc(r, 4)
		doc, err := Parse(src, Options{})
		if err != nil {
			t.Fatalf("%q: %v", src, err)
		}
		got := string(doc.Canonical(true))
		if got != want {
			t.Fatalf("%q:\n got  %q\n want %q", src, got, want)
		}
		doc2, err := Parse([]byte(got), Options{})
		if err != nil {
			t.Fatalf("canonical form %q does not parse: %v", got, err)
		}
		if again := string(doc2.Canonical(true)); again != got {
			t.Fatalf("canonical not a fixed point:\n %q\n %q", got, again)
		}
	}
}

// eventBytes sums the decoded text, attribute values and names of all
// events.
func eventBytes(src []byte) (int, int, error) {
	d := NewDecoder(src, Options{})
	total := 0
	for {
		ev, err := d.Next()
		if err != nil {
			if err == io.EOF {
				err = nil
			}
			return total, d.n, err
		}
		total += len(ev.Text)
		if ev.Kind == EventStartElement {
			total += len(ev.Name.QName())
			for _, a := range ev.Attrs {
				total += len(a.Value) + len(a.Name.QName())
			}
		}
	}
}

// TestXMLInv05_NoExpansionGrowth: the decoded text, names and attribute
// values never exceed the input length (no reference expands to more
// bytes than it occupies), and any internal subset is refused.
func TestXMLInv05_NoExpansionGrowth(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		src, _ := genDoc(r, 4)
		if i%2 == 1 {
			src = mutate(r, src)
		}
		total, n, _ := eventBytes(src)
		if total > n {
			t.Fatalf("%q: %d decoded bytes from %d input bytes", src, total, n)
		}
	}
	refs := bytes.Repeat([]byte("&#x10FFFF;&lt;&amp;&#9;"), 1000)
	doc := append(append([]byte("<a>"), refs...), "</a>"...)
	if total, n, err := eventBytes(doc); err != nil || total*2 > n {
		t.Fatalf("reference-heavy doc: %d bytes from %d (%v)", total, n, err)
	}
	for i := 0; i < 200; i++ {
		body, _ := genDoc(r, 2)
		src := append([]byte("<!DOCTYPE x [<!ENTITY a \""+strings.Repeat("&a;", core.IntN(r, 9))+"\">]>"), body...)
		if e := parseErr(src); e == nil || e.Code != ErrDTDInternalSubset || GradeOf(e) != GradeUnsupported {
			t.Fatalf("internal subset not refused: %v", e)
		}
	}
}

// TestXMLInv06_LinearSteps: decoder steps stay within 3 per input byte
// plus a constant on generated and adversarial inputs.
func TestXMLInv06_LinearSteps(t *testing.T) {
	r := testkit.Rng(t)
	check := func(name string, src []byte) {
		d := NewDecoder(src, Options{MaxDepth: 1 << 20})
		for {
			if _, err := d.Next(); err != nil {
				break
			}
		}
		if d.steps > 3*len(src)+16 {
			t.Fatalf("%s: %d steps for %d bytes", name, d.steps, len(src))
		}
	}
	for i := 0; i < testkit.Cases(); i++ {
		src, _ := genDoc(r, 4)
		check("generated", src)
	}
	for name, src := range adversarialXML(1 << 16) {
		check(name, src)
	}
}

func adversarialXML(n int) map[string][]byte {
	rep := func(pre, s, post string) []byte {
		k := n / len(s)
		return []byte(pre + strings.Repeat(s, k) + post)
	}
	return map[string][]byte{
		"attributes":  rep("<a ", "b0='1' ", "/>"),
		"deep":        []byte(strings.Repeat("<a>", n/7) + strings.Repeat("</a>", n/7)),
		"refs":        rep("<a>", "&#x41;&lt;", "</a>"),
		"comment":     rep("<a><!--", "- ", "--></a>"),
		"text":        rep("<a>", "x", "</a>"),
		"cdata":       rep("<a><![CDATA[", "]]", "]]></a>"),
		"long-name":   rep("<a", "b", "/>"),
		"namespaces":  rep("<a ", "xmlns:p='urn:x' ", "/>"),
		"pi":          rep("<?t ", "??", "?><a/>"),
		"siblings":    rep("<a>", "<b/>", "</a>"),
		"whitespace":  rep("", " \r\n", "<a/>"),
		"line-breaks": rep("<a>", "\r", "</a>"),
	}
}

// TestXMLInv07_LimitsGradePartial: whenever a limit stops the read the
// grade is partial, never complete; inputs within the limits are never
// partial.
func TestXMLInv07_LimitsGradePartial(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		src, _ := genDoc(r, 4)
		opts := Options{MaxDepth: core.IntN(r, 5) + 1, MaxInputBytes: core.IntN(r, len(src)+4) + 1}
		doc, err := Parse(src, opts)
		limitHit := err != nil && (err.(*SyntaxError).Code == ErrDepthLimit || err.(*SyntaxError).Code == ErrInputTooLarge)
		if limitHit != (doc.Grade == GradePartial) {
			t.Fatalf("%q %+v: err %v grade %s", src, opts, err, doc.Grade)
		}
		if len(src) > opts.MaxInputBytes && doc.Grade == GradeComplete {
			t.Fatalf("oversized input graded complete")
		}
	}
}

// TestXMLInv08_EventsNestAndStick: decoder events are properly nested
// with matching names, the tree has one node per content event, and after
// an error every call returns the same error.
func TestXMLInv08_EventsNestAndStick(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		src, _ := genDoc(r, 4)
		if i%2 == 1 {
			src = mutate(r, src)
		}
		d := NewDecoder(src, Options{})
		var stack []Name
		content := 0
		var last error
		for {
			ev, err := d.Next()
			if err != nil {
				last = err
				break
			}
			switch ev.Kind {
			case EventStartElement:
				stack = append(stack, ev.Name)
				content++
			case EventEndElement:
				if len(stack) == 0 || stack[len(stack)-1] != ev.Name {
					t.Fatalf("unbalanced end %v", ev.Name)
				}
				stack = stack[:len(stack)-1]
			case EventText, EventCData, EventComment, EventPI:
				content++
			}
		}
		for k := 0; k < 3; k++ {
			if _, err := d.Next(); err != last {
				t.Fatalf("error not sticky: %v then %v", last, err)
			}
		}
		if last == io.EOF && len(stack) != 0 {
			t.Fatalf("EOF with %d open elements", len(stack))
		}
		doc, _ := Parse(src, Options{})
		if len(doc.Nodes)-1 != content {
			t.Fatalf("tree has %d nodes, decoder %d content events", len(doc.Nodes)-1, content)
		}
	}
}
