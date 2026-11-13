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

// formSubmission is one (action, method, enctype) a form can submit
// with: the action as written ("" for the form document's URL), the
// method as GET, POST or DIALOG, and the enctype keyword.
type formSubmission struct{ action, method, enctype string }

// form extracts the form's submissions (WHATWG §4.10.21.3 form
// submission) as form_action endpoints with their parameters: one for
// the form's own action, method and enctype (form.submit(), or implicit
// submission when the default button overrides nothing), and one for each
// further distinct (action, method, enctype) a submit button produces with
// its formaction, formmethod and formenctype attributes, which override
// the form's (§4.10.18.6). A dialog method does not submit. The form's
// own submission is extracted exactly as in html.forms@1, so its feature
// ids and attributes are unchanged; html.forms@1 also took a formaction
// from any button or input, kept the form's method unless formmethod was
// post (a formmethod=get button in a POST form submits GET) and ignored
// formmethod and formenctype without a formaction (OPEN_QUESTIONS 37).
func (h *htmlExtract) form(id htmlparse.NodeID) {
	enctype := h.attr(id, "enctype")
	own := formSubmission{h.attr(id, "action"), formMethod(h.attr(id, "method")), enctypeKeyword(enctype)}
	seen := map[formSubmission]bool{own: true}
	if own.method != "DIALOG" {
		h.submission(id, own.action, own.method, enctype)
	}
	for c := range h.doc.Descendants(id) {
		if !h.submitButton(c) {
			continue
		}
		s, overrides := own, false
		if v, ok := h.doc.Attr(c, "formaction"); ok {
			s.action, overrides = strings.TrimSpace(v), true
		}
		if v, ok := h.doc.Attr(c, "formmethod"); ok {
			s.method, overrides = formMethod(v), true
		}
		if v, ok := h.doc.Attr(c, "formenctype"); ok {
			s.enctype, overrides = enctypeKeyword(v), true
		}
		if !overrides || s.method == "DIALOG" || seen[s] {
			continue
		}
		seen[s] = true
		rec := enctype
		if s.enctype != own.enctype {
			rec = s.enctype
		}
		h.submission(id, s.action, s.method, rec)
	}
}

// formMethod is the state of a method or formmethod attribute value
// (§4.10.18.6): GET, POST or DIALOG, ASCII case-insensitive; the missing
// value default of method and the invalid value default of both is GET.
func formMethod(v string) string {
	switch m := strings.ToUpper(strings.TrimSpace(v)); m {
	case "POST", "DIALOG":
		return m
	default:
		return "GET"
	}
}

// enctypeKeyword is the state of an enctype or formenctype attribute value
// (§4.10.18.6): application/x-www-form-urlencoded (also the missing and
// invalid value default), multipart/form-data or text/plain.
func enctypeKeyword(v string) string {
	switch e := strings.ToLower(strings.TrimSpace(v)); e {
	case "multipart/form-data", "text/plain":
		return e
	default:
		return "application/x-www-form-urlencoded"
	}
}

// submitButton reports a submit button (§4.10.6, §4.10.5.1.15–16): an
// input of type submit or image, or a button whose type is submit, or in
// the auto state (missing or invalid type) without a command or
// commandfor attribute. Only a submit button's formaction, formmethod and
// formenctype take part in submission.
func (h *htmlExtract) submitButton(id htmlparse.NodeID) bool {
	n := h.doc.Node(id)
	if n.Kind != htmlparse.ElementNode || n.Namespace != htmlparse.HTMLNamespace {
		return false
	}
	typ := strings.ToLower(h.attr(id, "type"))
	switch n.Name {
	case "input":
		return typ == "submit" || typ == "image"
	case "button":
		switch typ {
		case "submit":
			return true
		case "reset", "button":
			return false
		}
		_, command := h.doc.Attr(id, "command")
		_, commandfor := h.doc.Attr(id, "commandfor")
		return !command && !commandfor
	}
	return false
}

// submission extracts one form submission: the endpoint of action (the
// form document's URL when empty) under method, its parameters, and the
// form feature with enctype (as written, lower-cased).
func (h *htmlExtract) submission(form htmlparse.NodeID, action, method, enctype string) {
	if action == "" {
		action = h.page
	}
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
