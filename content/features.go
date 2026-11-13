package content

import (
	"cmp"
	"slices"
	"strings"

	"github.com/genuinebnt/flowforge/oam"
)

// FeatureKind is the type of a level-3 feature.
type FeatureKind string

// Feature kinds. Each has a fixed rule family (FamilyOf); the families of
// kinds that become graph facts are the oam families of their edge label
// or tag property type (oam.FamilyOfLabel / oam.FamilyOfProperty).
const (
	FeatLink             FeatureKind = "link"              // links_to
	FeatScript           FeatureKind = "script"            // loads_script
	FeatStyle            FeatureKind = "style"             // loads_style
	FeatFrame            FeatureKind = "frame"             // embeds
	FeatRedirect         FeatureKind = "redirect"          // redirects_to
	FeatForm             FeatureKind = "form"              // form_action
	FeatEndpoint         FeatureKind = "endpoint"          // calls
	FeatSchemaEndpoint   FeatureKind = "schema_endpoint"   // documents
	FeatParameter        FeatureKind = "parameter"         // ParameterProperty
	FeatGraphQLOperation FeatureKind = "graphql_operation" // ParameterProperty (body, graphql)
	FeatSourceMap        FeatureKind = "sourcemap"         // source_map
	FeatLibrary          FeatureKind = "library"           // includes_library
	FeatGenerator        FeatureKind = "generator"         // meta generator (feature only)
	FeatHeader           FeatureKind = "header"            // HTTPHeaderProperty
	FeatSecret           FeatureKind = "secret"            // VulnProperty exposed_secret
	FeatSourceDisclosure FeatureKind = "source_disclosure" // VulnProperty source_disclosure
	FeatModuleImport     FeatureKind = "module_import"     // import / import() specifiers (feature only)
	FeatCSSImport        FeatureKind = "css_import"        // @import → loads_style
	FeatCSSAsset         FeatureKind = "css_asset"         // url() references (feature only)
	FeatRobotsRule       FeatureKind = "robots_rule"       // robots.txt allow/disallow (feature only)
	FeatSitemapEntry     FeatureKind = "sitemap_entry"     // links_to
	FeatSecurityField    FeatureKind = "security_field"    // security.txt field (feature only)
	FeatSourceMapSource  FeatureKind = "sourcemap_source"  // a source listed in a source map (feature only)
)

// FamilyOf returns the rule family of a feature kind.
func FamilyOf(k FeatureKind) string {
	switch k {
	case FeatLink, FeatSitemapEntry, FeatCSSAsset, FeatRobotsRule, FeatSecurityField:
		return oam.FamilyOfLabel(oam.LabelLinksTo)
	case FeatScript, FeatModuleImport:
		return oam.FamilyOfLabel(oam.LabelLoadsScript)
	case FeatStyle, FeatCSSImport:
		return oam.FamilyOfLabel(oam.LabelLoadsStyle)
	case FeatFrame:
		return oam.FamilyOfLabel(oam.LabelEmbeds)
	case FeatRedirect:
		return oam.FamilyOfLabel(oam.LabelRedirectsTo)
	case FeatForm:
		return oam.FamilyOfLabel(oam.LabelFormAction)
	case FeatEndpoint:
		return oam.FamilyOfLabel(oam.LabelCalls)
	case FeatSchemaEndpoint:
		return oam.FamilyOfLabel(oam.LabelDocuments)
	case FeatParameter, FeatGraphQLOperation:
		return oam.FamilyOfProperty(oam.PropertyParameter)
	case FeatSourceMap, FeatSourceMapSource:
		return oam.FamilyOfLabel(oam.LabelSourceMap)
	case FeatLibrary, FeatGenerator:
		return oam.FamilyOfLabel(oam.LabelIncludesLibrary)
	case FeatHeader:
		return oam.FamilyOfProperty(oam.PropertyHTTPHeader)
	case FeatSecret, FeatSourceDisclosure:
		return oam.FamilyOfProperty(oam.PropertyVuln)
	}
	return string(k)
}

// Attr is one extra attribute of a feature.
type Attr struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Feature is one typed, keyed level-3 fact. Identity is (Doc, Kind, Key);
// Value and Attrs are compared to find modifications.
type Feature struct {
	Family string      `json:"family"`
	Kind   FeatureKind `json:"kind"`
	Key    string      `json:"key"`
	Value  string      `json:"value,omitempty"`
	Attrs  []Attr      `json:"attrs,omitempty"`
	// Ref is the concrete URL behind a lineage-keyed feature (a script
	// whose key has its build hash stripped). It is not compared: a
	// renamed bundle is the same script.
	Ref string `json:"ref,omitempty"`
	// Doc is the derived-document suffix the fact came from ("" for the
	// artifact itself). It is not part of the identity used by
	// classification: an endpoint moving from an inline script to an
	// external one is not a change.
	Doc string `json:"doc,omitempty"`
}

// ID is the feature's identity for comparisons: "<kind>|<key>".
func (f Feature) ID() string { return string(f.Kind) + "|" + f.Key }

// Attr returns the value of a named attribute.
func (f Feature) Attr(name string) string {
	for _, a := range f.Attrs {
		if a.Name == name {
			return a.Value
		}
	}
	return ""
}

// sameContent reports equal Value and Attrs.
func (f Feature) sameContent(g Feature) bool {
	return f.Value == g.Value && slices.Equal(f.Attrs, g.Attrs)
}

func newFeature(k FeatureKind, key, value string, attrs ...Attr) Feature {
	as := make([]Attr, 0, len(attrs))
	for _, a := range attrs {
		if a.Value != "" {
			as = append(as, a)
		}
	}
	slices.SortFunc(as, func(a, b Attr) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Value, b.Value)) })
	if len(as) == 0 {
		as = nil
	}
	return Feature{Family: FamilyOf(k), Kind: k, Key: key, Value: value, Attrs: as}
}

// ExtractorGrade is the reading grade one extractor reported for one
// document (V2 §12.10).
type ExtractorGrade struct {
	// Extractor is "<name>@<version>".
	Extractor string `json:"extractor"`
	Doc       string `json:"doc,omitempty"`
	Grade     Grade  `json:"grade"`
	// Facts is how many features it produced (after the
	// MaxFactsPerParser cap).
	Facts  int    `json:"facts"`
	Reason string `json:"reason,omitempty"`
}

// FeatureSet is the outcome of extraction over one artifact and its
// derived documents.
type FeatureSet struct {
	Features []Feature        `json:"features"`
	Grades   []ExtractorGrade `json:"grades"`
	// Findings are the secrets found (their values are never serialized).
	Findings []SecretFinding `json:"findings,omitempty"`
}

// sortFeatures orders features by (kind, key, doc, value) and removes
// exact duplicates, so outputs never depend on discovery order.
func sortFeatures(fs []Feature) []Feature {
	slices.SortFunc(fs, func(a, b Feature) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Key, b.Key), cmp.Compare(a.Doc, b.Doc),
			cmp.Compare(a.Value, b.Value), slices.CompareFunc(a.Attrs, b.Attrs, func(x, y Attr) int {
				return cmp.Or(cmp.Compare(x.Name, y.Name), cmp.Compare(x.Value, y.Value))
			}))
	})
	return slices.CompactFunc(fs, func(a, b Feature) bool {
		return a.Kind == b.Kind && a.Key == b.Key && a.Doc == b.Doc && a.sameContent(b)
	})
}

// byID returns the features keyed by ID; when several documents report
// the same ID the first in sorted order represents it.
func byID(fs []Feature) map[string]Feature {
	m := make(map[string]Feature, len(fs))
	for _, f := range fs {
		if _, ok := m[f.ID()]; !ok {
			m[f.ID()] = f
		}
	}
	return m
}

// FeatureIDs returns the sorted, unique feature ids.
func (s FeatureSet) FeatureIDs() []string {
	out := make([]string, 0, len(s.Features))
	for _, f := range s.Features {
		out = append(out, f.ID())
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Families returns the sorted families that have at least one feature.
func (s FeatureSet) Families() []string {
	var out []string
	for _, f := range s.Features {
		out = append(out, f.Family)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// gradeOf returns the worst grade reported for the extractor (any doc)
// and whether it reported at all.
func (s FeatureSet) gradeOf(extractor string) (Grade, bool) {
	g, ok := GradeComplete, false
	for _, e := range s.Grades {
		if e.Extractor == extractor {
			g, ok = worseGrade(g, e.Grade), true
		}
	}
	return g, ok
}

// typeHint turns a free-form type name ("datetime-local", "Text") into
// the lower-case identifier ParameterProperty.TypeHint requires: letters
// first, then letters, digits and '_' (other separators become '_'), at
// most 32 bytes; "" when nothing usable remains.
func typeHint(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for i := 0; i < len(s) && b.Len() < 32; i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9' && b.Len() > 0:
			b.WriteByte(c)
		case (c == '-' || c == '_' || c == ' ') && b.Len() > 0:
			b.WriteByte('_')
		}
	}
	return strings.TrimRight(b.String(), "_")
}
