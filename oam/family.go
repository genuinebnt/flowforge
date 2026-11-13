package oam

import "strings"

// Rule families group the facts an extractor produces (V2 design §3.4).
// An authoritative observation lists the families it read completely in
// Authority.CoveredRules; only edges and tags of a covered family can be
// withdrawn by it. The family of an edge is a function of its label and
// the family of a tag a function of its property type, so every
// implementation (and the Rust port) agrees without per-edge metadata.
const (
	FamilyLinks      = "links"      // links_to
	FamilyScripts    = "scripts"    // loads_script
	FamilyStyles     = "styles"     // loads_style
	FamilyFrames     = "frames"     // embeds
	FamilyRedirects  = "redirects"  // redirects_to
	FamilyForms      = "forms"      // form_action
	FamilyEndpoints  = "endpoints"  // calls
	FamilySourceMaps = "sourcemaps" // source_map
	FamilyLineage    = "lineage"    // previous_version
	FamilyLibraries  = "libraries"  // includes_library
	FamilySchemas    = "schemas"    // documents
	FamilyHeaders    = "headers"    // HTTPHeaderProperty tags
	FamilyContent    = "content"    // ContentProperty tags
	FamilyParameters = "parameters" // ParameterProperty tags
	FamilyFindings   = "findings"   // VulnProperty tags
)

// FamilyOfLabel returns the rule family of a relation label: the table
// above for the v2 content labels, and the (lower-cased) label itself for
// every other label.
func FamilyOfLabel(label string) string {
	switch l := strings.ToLower(label); l {
	case LabelLinksTo:
		return FamilyLinks
	case LabelLoadsScript:
		return FamilyScripts
	case LabelLoadsStyle:
		return FamilyStyles
	case LabelEmbeds:
		return FamilyFrames
	case LabelRedirectsTo:
		return FamilyRedirects
	case LabelFormAction:
		return FamilyForms
	case LabelCalls:
		return FamilyEndpoints
	case LabelSourceMap:
		return FamilySourceMaps
	case LabelPreviousVersion:
		return FamilyLineage
	case LabelIncludesLibrary:
		return FamilyLibraries
	case LabelDocuments:
		return FamilySchemas
	default:
		return l
	}
}

// FamilyOfProperty returns the rule family of a tag's property type: the
// table above, and the lower-cased type name for every other type.
func FamilyOfProperty(ptype string) string {
	switch ptype {
	case PropertyHTTPHeader:
		return FamilyHeaders
	case PropertyContent:
		return FamilyContent
	case PropertyParameter:
		return FamilyParameters
	case PropertyVuln:
		return FamilyFindings
	}
	return strings.ToLower(ptype)
}
