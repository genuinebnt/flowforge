package content

import (
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/htmlparse"
	"github.com/genuinebnt/flowforge/oam"
)

// html runs the html.* extractors over one parsed page.
func (x *extraction) html() {
	opts := htmlparse.Options{MaxDepth: MaxHTMLDepth, MaxInputSize: x.in.Limits.MaxResponseBytes}
	var doc *htmlparse.Document
	var errs []htmlparse.ParseError
	if utf8.Valid(x.in.Body) {
		doc, errs = htmlparse.Parse(x.in.Body, opts)
	} else {
		doc, _, errs = htmlparse.ParseRaw(x.in.Body, x.in.Charset, opts)
	}
	grade, reason := GradeComplete, ""
	for _, e := range errs {
		switch e.Code {
		case htmlparse.ErrNestingDepthLimit, htmlparse.ErrAttributeLimit, htmlparse.ErrInputSizeLimit:
			grade, reason = GradePartial, "html_"+strings.ReplaceAll(e.Code.String(), "-", "_")
		}
	}
	base := x.in.BaseURL
	for id := range doc.Elements(doc.Root, htmlparse.HTMLNamespace, "base") {
		if h, ok := doc.Attr(id, "href"); ok {
			if r, ok := resolveRef(base, h); ok {
				base = r
			}
			break // only the first base element with href counts (WHATWG §4.2.3)
		}
	}
	h := htmlExtract{doc: doc, base: base, page: x.in.URL}
	h.walk()
	x.report(ExHTMLLinks, grade, reason, h.links)
	x.report(ExHTMLScripts, grade, reason, h.scripts)
	x.report(ExHTMLStyles, grade, reason, h.styles)
	x.report(ExHTMLFrames, grade, reason, h.frames)
	x.report(ExHTMLForms, grade, reason, h.forms)
	x.report(ExHTMLMeta, grade, reason, h.meta)
}

type htmlExtract struct {
	doc                                         *htmlparse.Document
	base, page                                  string
	links, scripts, styles, frames, forms, meta []Feature
}

func (h *htmlExtract) attr(id htmlparse.NodeID, name string) string {
	v, _ := h.doc.Attr(id, name)
	return strings.TrimSpace(v)
}

func relTokens(v string) []string {
	f := strings.Fields(strings.ToLower(v))
	slices.Sort(f)
	return slices.Compact(f)
}

func (h *htmlExtract) walk() {
	for id := range h.doc.Descendants(h.doc.Root) {
		n := h.doc.Node(id)
		if n.Kind != htmlparse.ElementNode {
			continue
		}
		switch n.Name {
		case "a", "area":
			if n.Namespace != htmlparse.HTMLNamespace {
				continue
			}
			if u, ok := resolveRef(h.base, h.attr(id, "href")); ok {
				h.links = append(h.links, newFeature(FeatLink, u, "", Attr{"tag", n.Name}, Attr{"rel", strings.Join(relTokens(h.attr(id, "rel")), " ")}))
			}
		case "link":
			h.link(id)
		case "script":
			src := h.attr(id, "src")
			if src == "" {
				continue
			}
			if u, ok := resolveRef(h.base, src); ok {
				h.scripts = append(h.scripts, h.scriptFeature(u, id, h.attr(id, "type")))
			}
		case "iframe", "frame", "embed":
			if u, ok := resolveRef(h.base, h.attr(id, "src")); ok {
				h.frames = append(h.frames, newFeature(FeatFrame, u, "", Attr{"tag", n.Name}, Attr{"sandbox", boolAttr(h.doc, id, "sandbox")}))
			}
		case "object":
			if u, ok := resolveRef(h.base, h.attr(id, "data")); ok {
				h.frames = append(h.frames, newFeature(FeatFrame, u, "", Attr{"tag", n.Name}))
			}
		case "form":
			h.form(id)
		case "meta":
			if strings.EqualFold(h.attr(id, "name"), "generator") {
				if c := h.attr(id, "content"); c != "" {
					name, ver := splitProductVersion(c)
					h.meta = append(h.meta, newFeature(FeatGenerator, "generator", c, Attr{"product", name}, Attr{"version", ver}))
				}
			}
		}
	}
}

func boolAttr(doc *htmlparse.Document, id htmlparse.NodeID, name string) string {
	if _, ok := doc.Attr(id, name); ok {
		return "true"
	}
	return ""
}

func (h *htmlExtract) scriptFeature(u string, id htmlparse.NodeID, typ string) Feature {
	sri := "false"
	if h.attr(id, "integrity") != "" {
		sri = "true"
	}
	third := "false"
	if !sameOrigin(u, h.page) {
		third = "true"
	}
	if strings.EqualFold(typ, "modulepreload") {
		typ = "module"
	}
	f := newFeature(FeatScript, LineageKey(u), "", Attr{"sri", sri}, Attr{"third_party", third},
		Attr{"type", strings.ToLower(typ)}, Attr{"crossorigin", h.attr(id, "crossorigin")})
	f.Ref = u
	return f
}

func (h *htmlExtract) link(id htmlparse.NodeID) {
	rels := relTokens(h.attr(id, "rel"))
	u, ok := resolveRef(h.base, h.attr(id, "href"))
	if !ok {
		return
	}
	switch {
	case slices.Contains(rels, "stylesheet"):
		sri := "false"
		if h.attr(id, "integrity") != "" {
			sri = "true"
		}
		f := newFeature(FeatStyle, LineageKey(u), "", Attr{"sri", sri})
		f.Ref = u
		h.styles = append(h.styles, f)
	case slices.Contains(rels, "modulepreload") || slices.Contains(rels, "preload") && strings.EqualFold(h.attr(id, "as"), "script"):
		h.scripts = append(h.scripts, h.scriptFeature(u, id, "modulepreload"))
	default:
		h.links = append(h.links, newFeature(FeatLink, u, "", Attr{"tag", "link"}, Attr{"rel", strings.Join(rels, " ")}))
	}
}

// form extracts the form_action endpoint and its parameters (WHATWG
// §4.10.21.3 form submission: the action defaults to the document URL,
// the method to GET; dialog forms do not submit).
func (h *htmlExtract) form(id htmlparse.NodeID) {
	method := strings.ToUpper(h.attr(id, "method"))
	switch method {
	case "", "GET":
		method = "GET"
	case "POST":
	case "DIALOG":
		return
	default:
		method = "GET" // invalid values fall back to GET (the method attribute's missing value default)
	}
	action := h.attr(id, "action")
	if action == "" {
		action = h.page
	}
	h.submission(id, action, method, h.attr(id, "enctype"))
	for c := range h.doc.Descendants(id) {
		n := h.doc.Node(c)
		if n.Kind == htmlparse.ElementNode && (n.Name == "button" || n.Name == "input") {
			if fa := h.attr(c, "formaction"); fa != "" {
				fm := strings.ToUpper(h.attr(c, "formmethod"))
				if fm != "POST" {
					fm = method
				}
				h.submission(id, fa, fm, h.attr(id, "enctype"))
			}
		}
	}
}

func (h *htmlExtract) submission(form htmlparse.NodeID, action, method, enctype string) {
	abs, ok := resolveRef(h.base, action)
	if !ok {
		return
	}
	_, key, ok := endpointOf(abs, h.base, method, oam.EndpointSourceHTMLForm)
	if !ok {
		return
	}
	loc := oam.ParamQuery
	if method == "POST" {
		loc = oam.ParamBody
	}
	var names []string
	for c := range h.doc.Descendants(form) {
		n := h.doc.Node(c)
		if n.Kind != htmlparse.ElementNode || n.Namespace != htmlparse.HTMLNamespace {
			continue
		}
		switch n.Name {
		case "input", "select", "textarea", "button":
		default:
			continue
		}
		name := h.attr(c, "name")
		if name == "" {
			continue
		}
		typ := strings.ToLower(h.attr(c, "type"))
		if n.Name != "input" {
			typ = n.Name
		}
		if n.Name == "button" || typ == "submit" || typ == "reset" || typ == "image" || typ == "button" {
			continue
		}
		if typ == "" {
			typ = "text"
		}
		names = append(names, name)
		h.forms = append(h.forms, newFeature(FeatParameter, key+" "+loc+":"+name, "", Attr{"location", loc},
			Attr{"type_hint", typeHint(typ)}, Attr{"endpoint", key}))
	}
	slices.Sort(names)
	names = slices.Compact(names)
	h.forms = append(h.forms, newFeature(FeatForm, key, method, Attr{"enctype", strings.ToLower(enctype)},
		Attr{"inputs", strings.Join(names, ",")}))
}

// splitProductVersion splits "WordPress 6.4.2" into ("wordpress",
// "6.4.2"): the last token that looks like a version, and the words
// before it.
func splitProductVersion(s string) (string, string) {
	fs := strings.Fields(s)
	for i := len(fs) - 1; i >= 0; i-- {
		if v := semverPrefix(strings.TrimPrefix(strings.ToLower(fs[i]), "v")); v != "" {
			return libraryName(strings.Join(fs[:i], " ")), v
		}
	}
	return libraryName(s), ""
}
