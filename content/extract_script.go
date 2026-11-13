package content

import (
	"strings"

	"github.com/genuinebnt/flowforge/jsparse"
	"github.com/genuinebnt/flowforge/oam"
)

// callRule says how a call names an endpoint: the URL argument index (or
// the "url" property), the fixed method or where the method is.
type callRule struct {
	urlArg    int    // argument holding the URL; -1: only via the "url" property
	method    string // fixed method, or "" to read methodKey / methodArg
	methodKey string // property naming the method in an options object
	methodArg int    // argument holding the method (-1: none)
	def       string // default method
}

// callRules maps callee paths (after constructor resolution) to rules:
// fetch (WHATWG Fetch), XMLHttpRequest.open, axios, jQuery.ajax and its
// shorthands, navigator.sendBeacon and EventSource.
var callRules = map[string]callRule{
	"fetch":                {0, "", "method", -1, "GET"},
	"window.fetch":         {0, "", "method", -1, "GET"},
	"self.fetch":           {0, "", "method", -1, "GET"},
	"globalThis.fetch":     {0, "", "method", -1, "GET"},
	"XMLHttpRequest.open":  {1, "", "", 0, "GET"},
	"axios":                {0, "", "method", -1, "GET"},
	"axios.request":        {-1, "", "method", -1, "GET"},
	"$.ajax":               {0, "", "type", -1, "GET"},
	"jQuery.ajax":          {0, "", "type", -1, "GET"},
	"$.get":                {0, "GET", "", -1, "GET"},
	"jQuery.get":           {0, "GET", "", -1, "GET"},
	"$.getJSON":            {0, "GET", "", -1, "GET"},
	"jQuery.getJSON":       {0, "GET", "", -1, "GET"},
	"$.post":               {0, "POST", "", -1, "POST"},
	"jQuery.post":          {0, "POST", "", -1, "POST"},
	"navigator.sendBeacon": {0, "POST", "", -1, "POST"},
	"EventSource":          {0, "GET", "", -1, "GET"},
}

// httpVerbs are method-named calls (`client.post(url)`) recognized when
// the receiver names an HTTP client.
var httpVerbs = map[string]string{"get": "GET", "post": "POST", "put": "PUT", "delete": "DELETE", "patch": "PATCH",
	"head": "HEAD", "options": "OPTIONS"}

var httpReceivers = []string{"axios", "http", "api", "client", "request", "ky", "fetcher", "service"}

// ruleFor returns the rule of a callee path.
func ruleFor(path string) (callRule, bool) {
	if r, ok := callRules[path]; ok {
		return r, true
	}
	i := strings.LastIndexByte(path, '.')
	if i <= 0 {
		return callRule{}, false
	}
	verb, ok := httpVerbs[path[i+1:]]
	if !ok {
		return callRule{}, false
	}
	recv := strings.ToLower(path[:i])
	if j := strings.LastIndexByte(recv, '.'); j >= 0 {
		recv = recv[j+1:]
	}
	for _, h := range httpReceivers {
		if strings.Contains(recv, h) {
			return callRule{0, verb, "", -1, verb}, true
		}
	}
	return callRule{}, false
}

// jsFacts are the JS extractors' outputs for one program.
type jsFacts struct {
	endpoints, graphql, sourcemaps, libraries, modules []Feature
}

func (x *extraction) js(prog *jsparse.Program) {
	grade, reason := GradeComplete, ""
	if len(prog.Diagnostics) > 0 {
		grade, reason = GradePartial, "js_"+prog.Diagnostics[0].Code.String()
	}
	f := jsExtract(prog, x.in.URL, x.in.BaseURL)
	x.report(ExJSEndpoints, grade, reason, f.endpoints)
	x.report(ExJSGraphQL, grade, reason, f.graphql)
	x.report(ExJSSourceMap, grade, reason, f.sourcemaps)
	x.report(ExJSLibraries, grade, reason, f.libraries)
	x.report(ExJSModules, grade, reason, f.modules)
}

func jsExtract(prog *jsparse.Program, docURL, base string) jsFacts {
	var f jsFacts
	uses := prog.StringLiterals()
	add := func(raw, method, source string, attrs ...Attr) {
		ep, key, ok := endpointOf(raw, base, method, source)
		if !ok {
			return
		}
		_, dyn, _ := absTemplate(raw, base)
		if dyn {
			attrs = append(attrs, Attr{"dynamic_base", "true"})
		}
		f.endpoints = append(f.endpoints, newFeature(FeatEndpoint, key, ep.Method, append(attrs, Attr{"source", source})...))
		for _, q := range queryParamNames(raw) {
			f.endpoints = append(f.endpoints, newFeature(FeatParameter, key+" "+oam.ParamQuery+":"+q, "",
				Attr{"location", oam.ParamQuery}, Attr{"endpoint", key}))
		}
	}
	for i, u := range uses {
		callee := u.ReceiverType
		if callee == "" {
			callee = u.CalleePath
		}
		if r, ok := ruleFor(callee); ok && u.ArgIndex >= 0 {
			isURL := u.ArgIndex == r.urlArg && u.PropertyKey == "" || u.PropertyKey == "url" && (u.ArgIndex == 0 || u.ArgIndex == 1)
			if isURL && looksLikeURLRef(u.Value) {
				add(appendExpr(prog, u), callMethod(uses, i, r), oam.EndpointSourceJSCall, Attr{"callee", callee})
				continue
			}
		}
		if op, ok := graphqlOperation(u); ok {
			f.graphql = append(f.graphql, op)
			continue
		}
		if u.Kind != jsparse.TaggedTemplate && looksLikePathLiteral(u.Value) {
			add(appendExpr(prog, u), "*", oam.EndpointSourceJSLiteral)
		}
	}
	if u, _, ok := prog.SourceMappingURL(); ok {
		if abs, ok := resolveRef(docURL, u); ok {
			// Keyed by lineage key: main.3f9a1c2b.js.map renamed with its
			// bundle is the same source map.
			sm := newFeature(FeatSourceMap, LineageKey(abs), "")
			sm.Ref = abs
			f.sourcemaps = append(f.sourcemaps, sm)
		}
	}
	libs := append(libraryBanners(prog), libraryGlobals(uses)...)
	for _, l := range libs {
		f.libraries = append(f.libraries, newFeature(FeatLibrary, l.Name, l.Version, Attr{"evidence", l.Evidence}))
	}
	for _, m := range prog.ModuleRefs() {
		if !m.Literal || m.Specifier == "" {
			continue
		}
		spec, key := m.Specifier, m.Specifier // bare specifiers ("react") stay as written
		if strings.HasPrefix(spec, ".") || strings.HasPrefix(spec, "/") || strings.HasPrefix(spec, "http") {
			if abs, ok := resolveRef(docURL, strings.ReplaceAll(spec, jsparse.ExprPlaceholder, "{expr}")); ok {
				spec, key = abs, LineageKey(abs)
			}
		}
		dyn := "false"
		if m.Dynamic {
			dyn = "true"
		}
		mf := newFeature(FeatModuleImport, key, "", Attr{"dynamic", dyn})
		mf.Ref = spec
		f.modules = append(f.modules, mf)
	}
	return f
}

// appendExpr returns the literal's value, with "${}" appended when the
// literal ends in "/" and the source continues with "+" (string
// concatenation: "/api/users/" + id).
func appendExpr(prog *jsparse.Program, u jsparse.StringUse) string {
	v := u.Value
	if !strings.HasSuffix(v, "/") || int(u.Span.End) >= len(prog.Source) {
		return v
	}
	rest := strings.TrimLeft(prog.Source[u.Span.End:min(len(prog.Source), int(u.Span.End)+8)], " \t\n\r")
	if strings.HasPrefix(rest, "+") && !strings.HasPrefix(rest, "++") {
		return v + jsparse.ExprPlaceholder
	}
	return v
}

// callMethod finds the HTTP method of the call that use i belongs to: the
// fixed method, a method argument, or a method/type property of the same
// callee that follows the URL within the next few literals.
func callMethod(uses []jsparse.StringUse, i int, r callRule) string {
	if r.method != "" {
		return r.method
	}
	u := uses[i]
	look := func(j int) (string, bool) {
		v := uses[j]
		if v.CalleePath != u.CalleePath || v.ReceiverType != u.ReceiverType {
			return "", false
		}
		if r.methodArg >= 0 && v.ArgIndex == r.methodArg && v.PropertyKey == "" {
			return v.Value, true
		}
		if r.methodKey != "" && (v.PropertyKey == r.methodKey || v.PropertyKey == "method") {
			return v.Value, true
		}
		return "", false
	}
	// XMLHttpRequest.open(method, url): the method literal precedes.
	for j := i - 1; j >= 0 && j >= i-2; j-- {
		if m, ok := look(j); ok && r.methodArg >= 0 {
			if mm, err := oam.NormalizeHTTPMethod(m); err == nil {
				return mm
			}
		}
	}
	for j := i + 1; j < len(uses) && j <= i+4; j++ {
		if r.urlArg >= 0 && uses[j].ArgIndex == r.urlArg && uses[j].PropertyKey == "" {
			break // a URL argument again: the next call of the same function
		}
		if m, ok := look(j); ok {
			if mm, err := oam.NormalizeHTTPMethod(m); err == nil {
				return mm
			}
		}
	}
	return r.def
}

// looksLikeURLRef accepts a call argument that can name a URL: a path, a
// relative reference or an absolute http(s) URL.
func looksLikeURLRef(v string) bool {
	if v == "" || len(v) > 2048 || strings.ContainsAny(v, " \t\n\r<>\"'\\") {
		return false
	}
	lv := strings.ToLower(v)
	if strings.HasPrefix(lv, "data:") || strings.HasPrefix(lv, "blob:") || strings.HasPrefix(lv, "javascript:") ||
		strings.HasPrefix(lv, "ws:") || strings.HasPrefix(lv, "wss:") || strings.HasPrefix(lv, "mailto:") {
		return false
	}
	return true
}

// looksLikePathLiteral is the js_literal heuristic: a string that starts
// with "/" followed by a letter (not "//", "/*" or "/ "), made of URL
// characters, with a letter in some segment, that does not name a static
// file; or an absolute http(s) URL whose path is an API ("/api", "/v<n>/",
// "graphql", or an api.* host).
func looksLikePathLiteral(v string) bool {
	if len(v) < 2 || len(v) > 512 || staticAssetExt(v) {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		if !(alnum(c) || strings.IndexByte("-._~/%:{}$?=&+@,;*!()[]", c) >= 0) {
			return false
		}
	}
	lv := strings.ToLower(v)
	if strings.HasPrefix(lv, "http://") || strings.HasPrefix(lv, "https://") {
		rest := lv[strings.Index(lv, "//")+2:]
		host, p, _ := strings.Cut(rest, "/")
		return strings.HasPrefix(host, "api.") || strings.Contains("/"+p, "/api") || strings.Contains(p, "graphql") ||
			len(p) > 2 && p[0] == 'v' && p[1] >= '0' && p[1] <= '9'
	}
	if v[0] != '/' || !(v[1] >= 'a' && v[1] <= 'z' || v[1] >= 'A' && v[1] <= 'Z' || v[1] == '{' || v[1] == '$' || v[1] == ':') {
		return false
	}
	if strings.Contains(v, "//") || strings.Contains(v, "*") || strings.HasSuffix(v, "(") {
		return false
	}
	return true
}

// graphqlOperation recognizes a GraphQL document in a literal (October
// 2021 spec §2.3 OperationDefinition): "query|mutation|subscription
// [Name] ... {", or a gql/graphql tagged template.
func graphqlOperation(u jsparse.StringUse) (Feature, bool) {
	v := strings.TrimSpace(u.Value)
	tagged := u.Kind == jsparse.TaggedTemplate && (u.CalleePath == "gql" || u.CalleePath == "graphql" || strings.HasSuffix(u.CalleePath, ".gql"))
	for _, op := range []string{"query", "mutation", "subscription"} {
		rest, ok := strings.CutPrefix(v, op)
		if !ok || rest == "" || !(rest[0] == ' ' || rest[0] == '{' || rest[0] == '(' || rest[0] == '\n' || rest[0] == '\t') {
			continue
		}
		if !strings.Contains(rest, "{") {
			return Feature{}, false
		}
		rest = strings.TrimLeft(rest, " \t\n\r")
		name := gqlName(rest)
		if name == "" {
			// Anonymous operation: name it by its first selection.
			if b := strings.IndexByte(rest, '{'); b >= 0 {
				name = "anonymous:" + gqlName(strings.TrimLeft(rest[b+1:], " \t\n\r"))
			}
		}
		return newFeature(FeatGraphQLOperation, op+" "+name, op, Attr{"location", oam.ParamBody}), true
	}
	if tagged {
		if strings.HasPrefix(v, "fragment ") {
			return newFeature(FeatGraphQLOperation, "fragment "+gqlName(strings.TrimSpace(v[9:])), "fragment"), true
		}
	}
	return Feature{}, false
}

// gqlName reads a GraphQL Name ([_A-Za-z][_0-9A-Za-z]*).
func gqlName(s string) string {
	i := 0
	for i < len(s) && (alnum(s[i]) || s[i] == '_') && !(i == 0 && s[i] >= '0' && s[i] <= '9') {
		i++
	}
	return s[:i]
}
