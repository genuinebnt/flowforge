package content

import (
	"bytes"
	"slices"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/cssparse"
	"github.com/genuinebnt/flowforge/diff"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/sourcemap"
	"github.com/genuinebnt/flowforge/wellknown"
	"github.com/genuinebnt/flowforge/xmlparse/sitemap"
)

// css reads url() references: @import targets are styles (loads_style),
// everything else is an asset reference.
func (x *extraction) css() {
	res := cssparse.ExtractURLs(x.in.Body, cssparse.Options{MaxInputBytes: x.in.Limits.MaxResponseBytes})
	var fs []Feature
	for _, r := range res.Refs {
		abs, ok := resolveRef(x.in.URL, r.URL)
		if !ok {
			continue
		}
		if r.Context == cssparse.URLContextImport {
			fs = append(fs, newFeature(FeatCSSImport, LineageKey(abs), abs))
		} else if r.Context != cssparse.URLContextNamespace {
			fs = append(fs, newFeature(FeatCSSAsset, LineageKey(abs), abs, Attr{"context", r.Context.String()}))
		}
	}
	x.report(ExCSSURLs, Grade(res.Grade.String()), "", fs)
}

// headers reads the security headers of a header block (V2 §3.3
// HTTPHeaderProperty): one feature per header, keyed by name (and cookie
// name for Set-Cookie), valued by the canonical value.
func (x *extraction) headers() {
	var fs []Feature
	m := newMasker()
	for _, h := range parseHeaderBlock(x.in.Body) {
		if !securityHeaders[h.Name] || h.Name == "content-type" || h.Name == "location" {
			continue
		}
		v := canonicalHeaderValue(h.Name, h.Value, m)
		key := h.Name
		if h.Name == "set-cookie" {
			c := ParseSetCookie(h.Value)
			key += ":" + c.Name
			fs = append(fs, newFeature(FeatHeader, key, v, Attr{"secure", strconv.FormatBool(c.Secure)},
				Attr{"httponly", strconv.FormatBool(c.HTTPOnly)}, Attr{"samesite", c.SameSite}))
			continue
		}
		fs = append(fs, newFeature(FeatHeader, key, v))
	}
	x.report(ExHeaders, GradeComplete, "", fs)
}

func (x *extraction) robots() {
	r := wellknown.ParseRobots(x.in.Body, wellknown.RobotsOptions{})
	var fs []Feature
	for _, g := range r.Groups {
		agents := strings.Join(g.Agents, ",")
		for _, rule := range g.Rules {
			verb := "disallow"
			if rule.Allow {
				verb = "allow"
			}
			fs = append(fs, newFeature(FeatRobotsRule, verb+" "+agents+" "+rule.Pattern, verb))
		}
	}
	for _, s := range r.Sitemaps {
		if abs, ok := resolveRef(x.in.URL, s); ok {
			fs = append(fs, newFeature(FeatLink, abs, "", Attr{"tag", "robots_sitemap"}))
		}
	}
	x.report(ExRobots, Grade(r.Grade.String()), "", fs)
}

func (x *extraction) securityTxt() {
	s := wellknown.ParseSecurityTxt(x.in.Body, x.in.FetchedAt, wellknown.SecurityOptions{})
	var fs []Feature
	for _, f := range s.Fields {
		name := strings.ToLower(f.Name)
		switch name {
		case "contact", "policy", "encryption", "acknowledgments", "hiring", "canonical", "csaf", "expires", "preferred-languages":
			fs = append(fs, newFeature(FeatSecurityField, name+":"+f.Value, f.Value))
		}
	}
	x.report(ExSecurityTxt, Grade(s.Grade.String()), "", fs)
}

func (x *extraction) sitemap() {
	sm := sitemap.Parse(x.in.Body, sitemap.Options{})
	var fs []Feature
	for _, e := range sm.Entries {
		fs = append(fs, newFeature(FeatSitemapEntry, e.Loc, "", Attr{"kind", sm.Kind.String()}))
	}
	x.report(ExSitemap, Grade(sm.Grade.String()), "", fs)
}

func (x *extraction) sourceMap() {
	m, err := sourcemap.Parse(x.in.Body, sourcemap.Options{MaxInputBytes: x.in.Limits.MaxResponseBytes})
	g := Grade(sourcemap.GradeOf(err).String())
	var fs []Feature
	if m != nil {
		for i, u := range m.ResolveSources(x.in.URL) {
			if i >= len(m.Sources) || m.Sources[i].Null {
				continue
			}
			vendor := "false"
			if strings.Contains(u, "node_modules/") || m.Sources[i].Ignored {
				vendor = "true"
			}
			hasContent := strconv.FormatBool(m.Sources[i].HasContent)
			fs = append(fs, newFeature(FeatSourceMapSource, u, "", Attr{"vendor", vendor}, Attr{"content", hasContent}))
		}
	}
	reason := ""
	if err != nil {
		reason = "sourcemap_error"
	}
	x.report(ExSourceMap, g, reason, fs)
}

// jsonSchemas reads OpenAPI 3 / Swagger 2 documents and GraphQL
// introspection results (V2 §3.2 documents edges).
func (x *extraction) jsonSchemas() {
	v, err := diff.ParseJSON(x.in.Body)
	if err != nil {
		x.report(ExOpenAPI, GradeFailed, "json_invalid", nil)
		x.report(ExGraphQLSDL, GradeFailed, "json_invalid", nil)
		return
	}
	x.report(ExOpenAPI, GradeComplete, "", openAPIEndpoints(v, x.in.URL))
	x.report(ExGraphQLSDL, GradeComplete, "", graphqlIntrospection(v, x.in.URL))
}

var openAPIMethods = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

func jsonString(v diff.Value, key string) string {
	if m, ok := v.Get(key); ok && m.Kind == diff.KindString {
		return m.Text
	}
	return ""
}

// openAPIEndpoints maps paths × operations to schema endpoints (OpenAPI
// 3.1 §4.8.8 Paths Object; Swagger 2.0 host + basePath). The base is the
// first absolute servers[].url, the Swagger host, or the document's
// origin.
func openAPIEndpoints(v diff.Value, docURL string) []Feature {
	if v.Kind != diff.KindObject {
		return nil
	}
	_, isOAS := v.Get("openapi")
	_, isSwagger := v.Get("swagger")
	paths, ok := v.Get("paths")
	if !isOAS && !isSwagger || !ok || paths.Kind != diff.KindObject {
		return nil
	}
	base := docURL
	if servers, ok := v.Get("servers"); ok && servers.Kind == diff.KindArray {
		for _, s := range servers.Items {
			if u := jsonString(s, "url"); u != "" {
				if abs, ok := resolveRef(docURL, u); ok {
					base = strings.TrimSuffix(abs, "/")
					break
				}
			}
		}
	}
	if host := jsonString(v, "host"); host != "" {
		scheme := "https"
		if sch, ok := v.Get("schemes"); ok && sch.Kind == diff.KindArray && len(sch.Items) > 0 && sch.Items[0].Kind == diff.KindString {
			scheme = sch.Items[0].Text
		}
		base = scheme + "://" + host + strings.TrimSuffix(jsonString(v, "basePath"), "/")
	} else if bp := jsonString(v, "basePath"); bp != "" {
		if abs, ok := resolveRef(docURL, bp); ok {
			base = strings.TrimSuffix(abs, "/")
		}
	}
	var fs []Feature
	for _, p := range paths.Members {
		if p.Value.Kind != diff.KindObject || !strings.HasPrefix(p.Key, "/") {
			continue
		}
		raw := strings.TrimSuffix(base, "/") + p.Key
		if strings.HasPrefix(base, "/") {
			raw = base + p.Key
		}
		shared := paramList(p.Value)
		for _, op := range p.Value.Members {
			if !slices.Contains(openAPIMethods, op.Key) || op.Value.Kind != diff.KindObject {
				continue
			}
			ep, key, ok := endpointOf(raw, docURL, op.Key, oam.EndpointSourceOpenAPI)
			if !ok {
				continue
			}
			fs = append(fs, newFeature(FeatSchemaEndpoint, key, ep.Method, Attr{"operation_id", jsonString(op.Value, "operationId")}, Attr{"source", oam.EndpointSourceOpenAPI}))
			for _, pr := range append(slices.Clone(shared), paramList(op.Value)...) {
				fs = append(fs, newFeature(FeatParameter, key+" "+pr[1]+":"+pr[0], "", Attr{"location", pr[1]}, Attr{"endpoint", key}, Attr{"type_hint", pr[2]}))
			}
		}
	}
	return fs
}

// paramList reads an operation's parameters as (name, location, type).
func paramList(v diff.Value) [][3]string {
	ps, ok := v.Get("parameters")
	if !ok || ps.Kind != diff.KindArray {
		return nil
	}
	var out [][3]string
	for _, p := range ps.Items {
		name, in := jsonString(p, "name"), jsonString(p, "in")
		if name == "" || !slices.Contains(oam.ParameterLocations(), in) {
			continue
		}
		typ := jsonString(p, "type")
		if sch, ok := p.Get("schema"); ok {
			typ = jsonString(sch, "type")
		}
		out = append(out, [3]string{name, in, typeHint(typ)})
	}
	return out
}

// graphqlIntrospection reads root fields from an introspection result
// ({"data":{"__schema":...}} or {"__schema":...}).
func graphqlIntrospection(v diff.Value, docURL string) []Feature {
	if d, ok := v.Get("data"); ok {
		v = d
	}
	schema, ok := v.Get("__schema")
	if !ok || schema.Kind != diff.KindObject {
		return nil
	}
	roots := map[string]string{}
	for _, r := range [][2]string{{"queryType", "query"}, {"mutationType", "mutation"}, {"subscriptionType", "subscription"}} {
		if t, ok := schema.Get(r[0]); ok {
			if n := jsonString(t, "name"); n != "" {
				roots[n] = r[1]
			}
		}
	}
	types, ok := schema.Get("types")
	if !ok || types.Kind != diff.KindArray {
		return nil
	}
	var ops []string
	for _, t := range types.Items {
		op, ok := roots[jsonString(t, "name")]
		if !ok {
			continue
		}
		if fields, ok := t.Get("fields"); ok && fields.Kind == diff.KindArray {
			for _, f := range fields.Items {
				if n := jsonString(f, "name"); n != "" {
					ops = append(ops, op+" "+n)
				}
			}
		}
	}
	return graphqlSchemaFeatures(ops, docURL)
}

func graphqlSchemaFeatures(ops []string, docURL string) []Feature {
	if len(ops) == 0 {
		return nil
	}
	ep, key, ok := endpointOf("/graphql", docURL, "POST", oam.EndpointSourceGraphQL)
	if !ok {
		return nil
	}
	fs := []Feature{newFeature(FeatSchemaEndpoint, key, ep.Method, Attr{"source", oam.EndpointSourceGraphQL})}
	for _, op := range ops {
		kind, _, _ := strings.Cut(op, " ")
		fs = append(fs, newFeature(FeatGraphQLOperation, op, kind, Attr{"location", oam.ParamBody}, Attr{"endpoint", key}))
	}
	return fs
}

// graphqlSDL reads root fields from a GraphQL schema in SDL (October 2021
// spec §3.3: `type Query { field(...): T ... }`, `schema { query: Q }` is
// not resolved; the conventional root names are used).
func (x *extraction) graphqlSDL(body []byte) {
	if !bytes.Contains(body, []byte("type Query")) && !bytes.Contains(body, []byte("type Mutation")) {
		x.report(ExGraphQLSDL, GradeComplete, "", nil)
		return
	}
	var ops []string
	s := string(body)
	for _, root := range [][2]string{{"type Query", "query"}, {"type Mutation", "mutation"}, {"type Subscription", "subscription"}} {
		i := strings.Index(s, root[0])
		if i < 0 {
			continue
		}
		open := strings.IndexByte(s[i:], '{')
		if open < 0 {
			continue
		}
		depth, j := 0, i+open
		lineStart := true
		for ; j < len(s); j++ {
			c := s[j]
			switch {
			case c == '{' || c == '(':
				depth++
			case c == '}' || c == ')':
				depth--
			case c == '\n':
				lineStart = true
				continue
			}
			if depth == 0 {
				break
			}
			if depth == 1 && lineStart && (alnum(c) || c == '_') {
				if n := gqlName(s[j:]); n != "" {
					ops = append(ops, root[1]+" "+n)
					j += len(n) - 1
				}
			}
			if c != ' ' && c != '\t' {
				lineStart = false
			}
		}
	}
	x.report(ExGraphQLSDL, GradeComplete, "", graphqlSchemaFeatures(ops, x.in.URL))
}
