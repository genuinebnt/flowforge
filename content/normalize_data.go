package content

import (
	"bytes"
	"compress/gzip"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/diff"
	"github.com/genuinebnt/flowforge/xmlparse"
)

// jsonRules is the json@1 rule table.
var jsonRules = []string{
	"parse:rfc-8259 (diff.ParseJSON: numbers verbatim, duplicate keys rejected); an xssi )]}' first line is dropped",
	"emit:canonical members sorted bytewise by key, two-space indentation, one scalar or bracket per line",
	"string:escape \" \\ and controls (\\b \\f \\n \\r \\t, else \\u00xx); everything else raw utf-8",
	"fallback:invalid json → text@1 line normalization; grade partial",
	"mask:values of volatile keys (case and -_ insensitive: timestamp ts time servertime now generatedat buildtime builddate buildid nonce csrf csrftoken xsrf requestid traceid spanid correlationid rid) → class by key",
}

// volatileJSONKeys maps a squashed key (lower case, '-' and '_' removed)
// to the class its scalar value is masked as.
var volatileJSONKeys = map[string]MaskClass{
	"timestamp": MaskTimestamp, "ts": MaskTimestamp, "time": MaskTimestamp, "servertime": MaskTimestamp,
	"now": MaskTimestamp, "generatedat": MaskTimestamp, "buildtime": MaskTimestamp, "builddate": MaskTimestamp,
	"buildid": MaskBuildHash, "nonce": MaskNonce, "csrf": MaskCSRF, "csrftoken": MaskCSRF, "xsrf": MaskCSRF,
	"requestid": MaskRandomID, "traceid": MaskRandomID, "spanid": MaskRandomID, "correlationid": MaskRandomID,
	"rid": MaskRandomID,
}

func squashKey(k string) string {
	return strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(k))
}

func normalizeJSON(body []byte, learned VolatilitySet, m *masker) normResult {
	b := bytes.TrimPrefix(body, []byte("\xEF\xBB\xBF"))
	if t := bytes.TrimLeft(b, " \t\r\n"); bytes.HasPrefix(t, []byte(")]}'")) {
		_, b, _ = bytes.Cut(t, []byte("\n"))
	}
	v, err := diff.ParseJSON(b)
	if err != nil || !utf8.Valid(b) {
		r := normalizeText(body, KindText, m)
		return normResult{text: r.text, grade: GradePartial, diags: []string{"json_invalid_fallback"}}
	}
	var w strings.Builder
	jw := jsonWriter{w: &w, m: m, learned: learned}
	jw.value(v, 0, nil, "")
	w.WriteByte('\n')
	return normResult{text: []byte(w.String()), grade: GradeComplete}
}

type jsonWriter struct {
	w       *strings.Builder
	m       *masker
	learned VolatilitySet
}

func (j *jsonWriter) indent(n int) {
	for range n {
		j.w.WriteString("  ")
	}
}

// value writes v at depth; path holds the pointer tokens and key the
// member name v is stored under. Recursion depth is bounded by
// diff.MaxJSONDepth.
func (j *jsonWriter) value(v diff.Value, depth int, path []string, key string) {
	if v.Kind != diff.KindArray && v.Kind != diff.KindObject {
		class, vol := volatileJSONKeys[squashKey(key)]
		if !vol && !j.learned.Empty() && j.learned.jsonPointerRule(path) {
			class, vol = MaskLearned, true
		}
		if vol && v.Kind != diff.KindNull && !(v.Kind == diff.KindString && isPlaceholder(v.Text)) {
			orig := v.Text
			if v.Kind == diff.KindNumber {
				orig = v.Number
			} else if v.Kind == diff.KindBool {
				orig = strconv.FormatBool(v.Bool)
			}
			j.m.hit(class, orig)
			v = diff.StringValue(maskPlaceholder(class))
		}
	}
	switch v.Kind {
	case diff.KindNull:
		j.w.WriteString("null")
	case diff.KindBool:
		j.w.WriteString(strconv.FormatBool(v.Bool))
	case diff.KindNumber:
		j.w.WriteString(v.Number)
	case diff.KindString:
		writeJSONString(j.w, v.Text)
	case diff.KindArray:
		if len(v.Items) == 0 {
			j.w.WriteString("[]")
			return
		}
		j.w.WriteString("[\n")
		for i, it := range v.Items {
			j.indent(depth + 1)
			j.value(it, depth+1, append(path, strconv.Itoa(i)), "")
			if i < len(v.Items)-1 {
				j.w.WriteByte(',')
			}
			j.w.WriteByte('\n')
		}
		j.indent(depth)
		j.w.WriteByte(']')
	case diff.KindObject:
		if len(v.Members) == 0 {
			j.w.WriteString("{}")
			return
		}
		ms := slices.Clone(v.Members)
		slices.SortFunc(ms, func(a, b diff.Member) int { return strings.Compare(a.Key, b.Key) })
		j.w.WriteString("{\n")
		for i, mb := range ms {
			j.indent(depth + 1)
			writeJSONString(j.w, mb.Key)
			j.w.WriteString(": ")
			j.value(mb.Value, depth+1, append(path, mb.Key), mb.Key)
			if i < len(ms)-1 {
				j.w.WriteByte(',')
			}
			j.w.WriteByte('\n')
		}
		j.indent(depth)
		j.w.WriteByte('}')
	}
}

// writeJSONString writes s as a JSON string with the minimal escapes of
// RFC 8259 §7 (PORT: unlike encoding/json, no HTML or U+2028 escaping).
func writeJSONString(w *strings.Builder, s string) {
	w.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			w.WriteString(`\"`)
		case '\\':
			w.WriteString(`\\`)
		case '\b':
			w.WriteString(`\b`)
		case '\f':
			w.WriteString(`\f`)
		case '\n':
			w.WriteString(`\n`)
		case '\r':
			w.WriteString(`\r`)
		case '\t':
			w.WriteString(`\t`)
		default:
			if c < 0x20 {
				w.WriteString(`\u00`)
				w.WriteByte("0123456789abcdef"[c>>4])
				w.WriteByte("0123456789abcdef"[c&15])
			} else {
				w.WriteByte(c)
			}
		}
	}
	w.WriteByte('"')
}

// xmlRules is the xml@1 rule table.
var xmlRules = []string{
	"parse:xml-1.0 5th ed. + namespaces (xmlparse); doctype subsets refused; gzip sitemaps inflated to 8 MiB",
	"emit:one line per start tag, end tag, text run and processing instruction; empty elements as start-end pairs",
	"attrs:namespace declarations first sorted by prefix, then attributes sorted by (namespace uri, local name)",
	"drop:comments, xml declaration, doctype",
	"text:cdata as text; collapse xml white space; trim; drop empty; escape & < > (attributes: & < \" tab lf cr)",
	"partial:a not-well-formed or limited read emits the partial tree with every open element closed",
}

func normalizeXML(body []byte, lim Limits) normResult {
	r := normResult{grade: GradeComplete}
	if len(body) > 2 && body[0] == 0x1f && body[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return normResult{text: nil, grade: GradeFailed, diags: []string{"gzip_invalid"}}
		}
		// Never inflate past the response bound plus one byte.
		out, err := io.ReadAll(io.LimitReader(zr, int64(lim.MaxResponseBytes)+1))
		if err != nil {
			r.grade, r.diags = GradePartial, append(r.diags, "gzip_invalid")
		}
		if len(out) > lim.MaxResponseBytes {
			out = out[:lim.MaxResponseBytes]
			r.grade, r.diags = GradePartial, append(r.diags, "input_truncated")
		}
		body = out
	}
	doc, _ := xmlparse.Parse(body, xmlparse.Options{MaxDepth: lim.MaxParseDepth, MaxInputBytes: lim.MaxResponseBytes})
	switch doc.Grade {
	case xmlparse.GradeComplete:
	case xmlparse.GradePartial:
		r.grade = worseGrade(r.grade, GradePartial)
	case xmlparse.GradeUnsupported:
		r.grade = worseGrade(r.grade, GradeUnsupported)
	default:
		r.grade = worseGrade(r.grade, GradeFailed)
	}
	if doc.Err != nil {
		r.diags = append(r.diags, "xml_"+doc.Err.Code.String())
	}
	var w lineWriter
	xw := xmlWriter{d: doc, w: &w}
	for c := doc.Nodes[0].FirstChild; c != xmlparse.NoNode; c = doc.Nodes[c].NextSibling {
		// Character data outside the root element is not well-formed
		// (XML 1.0 [1] document); it only exists in failed reads.
		if k := doc.Nodes[c].Kind; k != xmlparse.NodeText && k != xmlparse.NodeCData {
			xw.node(c)
		}
	}
	xw.flush()
	r.text = w.bytes()
	return r
}

type xmlWriter struct {
	d    *xmlparse.Document
	w    *lineWriter
	text strings.Builder
}

func (x *xmlWriter) flush() {
	if x.text.Len() == 0 {
		return
	}
	t := collapseSpace(x.text.String())
	x.text.Reset()
	if t != "" {
		x.w.line(strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(t))
	}
}

// node writes one node; recursion is bounded by xmlparse's MaxDepth.
func (x *xmlWriter) node(id xmlparse.NodeID) {
	n := &x.d.Nodes[id]
	switch n.Kind {
	case xmlparse.NodeText, xmlparse.NodeCData:
		x.text.WriteString(n.Text)
	case xmlparse.NodePI:
		x.flush()
		x.w.line("<?" + n.Name.Local + " " + n.Text + "?>")
	case xmlparse.NodeElement:
		x.flush()
		attrs := x.d.AttrsOf(id)
		type kv struct {
			ns            bool
			sortKey, text string
		}
		list := make([]kv, 0, len(attrs))
		for _, a := range attrs {
			q := a.Name.QName()
			val := strings.NewReplacer("&", "&amp;", "<", "&lt;", `"`, "&quot;", "\t", "&#x9;", "\n", "&#xA;", "\r", "&#xD;").Replace(a.Value)
			isNS := a.Name.Prefix == "xmlns" || a.Name.Prefix == "" && a.Name.Local == "xmlns"
			key := a.Name.Space + "\x00" + a.Name.Local
			if isNS {
				key = a.Name.Local
				if a.Name.Prefix == "" {
					key = ""
				}
			}
			list = append(list, kv{isNS, key, q + `="` + val + `"`})
		}
		slices.SortFunc(list, func(a, b kv) int {
			if a.ns != b.ns {
				if a.ns {
					return -1
				}
				return 1
			}
			return strings.Compare(a.sortKey, b.sortKey)
		})
		var b strings.Builder
		b.WriteString("<" + n.Name.QName())
		for _, a := range list {
			b.WriteString(" " + a.text)
		}
		b.WriteByte('>')
		x.w.line(b.String())
		for c := n.FirstChild; c != xmlparse.NoNode; c = x.d.Nodes[c].NextSibling {
			x.node(c)
		}
		x.flush()
		x.w.line("</" + n.Name.QName() + ">")
	}
}
