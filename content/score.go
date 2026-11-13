package content

// Scoring (spec 33.11; V2 §12.4): an additive, clamped, hand-set feature model in the
// style of Sentinel's domain/analysis/changes.rs. Scores rank, never send:
// this file (and every content/score*.go) imports only the standard
// library's pure packages, so no code path leads from a score to a
// request (TestContentInv11_ScoreImportsNoEgress checks the import graph).

import (
	"cmp"
	"math"
	"slices"
	"strings"
)

// ScoreChange is one level-3 change as the scorer sees it: plain strings
// only, so scoring depends on nothing else in the package.
type ScoreChange struct {
	// Op is "added", "removed", "modified" or "returned".
	Op   string
	Kind string // a FeatureKind
	Key  string
	// Value and Attrs are the new ones (the old ones for a removal);
	// OldValue and OldAttrs the previous ones of a modification.
	Value, OldValue string
	Attrs, OldAttrs map[string]string
}

// ScoreReason is one named, weighted reason behind a score.
type ScoreReason struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
}

// Score is a score in [0, 1] and its reasons. Value equals the sum of the
// reason weights (in name order) clamped to [0, 1] and rounded to 4
// decimals (CONTENT-INV-11).
type Score struct {
	Value   float64       `json:"score"`
	Reasons []ScoreReason `json:"score_features,omitempty"`
}

// Base weights of an added (or, per kind, modified) feature.
var baseWeights = map[string]struct {
	name   string
	weight float64
}{
	"secret":            {"exposed_secret", 0.9},
	"source_disclosure": {"source_disclosure", 0.85},
	"endpoint":          {"new_endpoint", 0.4},
	"schema_endpoint":   {"new_endpoint", 0.4},
	"form":              {"new_form", 0.3},
	"parameter":         {"new_parameter", 0.1},
	"graphql_operation": {"new_graphql_operation", 0.2},
	"sourcemap":         {"source_map_exposed", 0.3},
	"script":            {"new_script", 0.1},
	"module_import":     {"new_module_import", 0.05},
	"library":           {"new_library", 0.1},
	"frame":             {"new_frame", 0.1},
	"style":             {"new_style", 0.02},
	"css_import":        {"new_style", 0.02},
	"link":              {"new_link", 0.02},
	"sitemap_entry":     {"new_link", 0.02},
	"header":            {"header_added", 0.05},
	"robots_rule":       {"robots_rule_added", 0.05},
	"security_field":    {"securitytxt_changed", 0.02},
	"redirect":          {"new_redirect", 0.05},
}

// Special weights.
const (
	wStateChanging   = 0.15 // POST/PUT/PATCH/DELETE on a new endpoint or form
	wTemplated       = 0.1  // a {id}/{uuid}/{hex}/{expr} path on a new endpoint
	wRemovedSecHdr   = 0.35 // a security header removed
	wCSPLoosened     = 0.3
	wCookieWeakened  = 0.25
	wLibraryVersion  = 0.2
	wThirdPartyNoSRI = 0.25
	wHeaderChanged   = 0.1
	removalFactor    = -0.5  // removals count at −½ of the positive weight
	returnFactor     = -0.25 // returns at −¼
)

// boostWords add weight once each per change set when the key of an added
// or modified endpoint, schema endpoint, form, link, script, parameter or
// GraphQL operation contains one of the words (containsWord).
var boostWords = []struct {
	name   string
	words  []string
	weight float64
}{
	{"word_admin", []string{"admin", "administrator", "manage"}, 0.15},
	{"word_auth", []string{"auth", "login", "oauth", "token", "session", "password"}, 0.1},
	{"word_payment", []string{"pay", "billing", "checkout", "invoice", "stripe"}, 0.15},
	{"word_internal", []string{"internal", "private", "staging", "intranet"}, 0.15},
	{"word_debug", []string{"debug", "trace", "test", "dev"}, 0.15},
}

// secHeaderNames are the headers whose removal is a removed_security_header.
var secHeaderNames = []string{"content-security-policy", "strict-transport-security", "x-frame-options",
	"x-content-type-options", "referrer-policy", "permissions-policy", "cross-origin-opener-policy",
	"cross-origin-embedder-policy", "cross-origin-resource-policy"}

// ScoreChanges scores a change set. Reasons are aggregated by name: each
// name's weight is the sum of its contributions clamped to [−1, 1] and
// rounded to 4 decimals, and a reason whose weight rounds to 0 is dropped
// (spec 33.11 does not mention the drop); the score is the sum of the
// reason weights in name order, clamped to [0, 1] and rounded to 4
// decimals. Reasons are sorted by name. Within one name, contributions are
// added in the order of cs, which callers pass sorted.
func ScoreChanges(cs []ScoreChange) Score {
	acc := map[string]float64{}
	add := func(name string, w float64) { acc[name] += w }
	boosted := map[string]bool{}
	for _, c := range cs {
		base, ok := baseWeights[c.Kind]
		if !ok {
			continue
		}
		switch c.Op {
		case "added":
			add(base.name, base.weight)
			if c.Kind == "endpoint" || c.Kind == "schema_endpoint" || c.Kind == "form" {
				method := c.Value
				if strings.HasPrefix(c.Key, "ep:") {
					method, _, _ = strings.Cut(strings.TrimPrefix(c.Key, "ep:"), " ")
				}
				switch method {
				case "POST", "PUT", "PATCH", "DELETE":
					add("state_changing_method", wStateChanging)
				}
				if strings.Contains(c.Key, "{id}") || strings.Contains(c.Key, "{uuid}") || strings.Contains(c.Key, "{hex}") ||
					strings.Contains(c.Key, "{expr}") {
					add("templated_path", wTemplated)
				}
			}
			if c.Kind == "script" && c.Attrs["third_party"] == "true" && c.Attrs["sri"] != "true" {
				add("third_party_script_without_sri", wThirdPartyNoSRI)
			}
		case "removed":
			if c.Kind == "header" && slices.Contains(secHeaderNames, headerName(c.Key)) {
				add("removed_security_header", wRemovedSecHdr)
				continue
			}
			add(removedName(base.name), base.weight*removalFactor)
		case "returned":
			add(returnedName(base.name), base.weight*returnFactor)
		case "modified":
			switch {
			case c.Kind == "library":
				add("library_version_change", wLibraryVersion)
			case c.Kind == "header" && strings.HasPrefix(c.Key, "content-security-policy"):
				if cspLoosened(c.OldValue, c.Value) {
					add("csp_loosened", wCSPLoosened)
				} else {
					add("header_changed", wHeaderChanged)
				}
			case c.Kind == "header" && strings.HasPrefix(c.Key, "set-cookie"):
				if cookieWeakened(c.OldAttrs, c.Attrs) {
					add("cookie_flags_weakened", wCookieWeakened)
				} else {
					add("header_changed", wHeaderChanged)
				}
			case c.Kind == "header":
				add("header_changed", wHeaderChanged)
			case c.Kind == "script" && c.Attrs["sri"] != "true" && c.OldAttrs["sri"] == "true":
				add("sri_removed", wThirdPartyNoSRI)
			}
		}
		if c.Op == "added" || c.Op == "modified" {
			switch c.Kind {
			case "endpoint", "schema_endpoint", "form", "link", "script", "parameter", "graphql_operation":
				lk := strings.ToLower(c.Key)
				for _, bw := range boostWords {
					if boosted[bw.name] {
						continue
					}
					for _, w := range bw.words {
						if containsWord(lk, w) {
							boosted[bw.name] = true
							add(bw.name, bw.weight)
							break
						}
					}
				}
			}
		}
	}
	var s Score
	sum := 0.0
	for name, w := range acc {
		w = round4(math.Max(-1, math.Min(1, w)))
		if w == 0 {
			continue
		}
		s.Reasons = append(s.Reasons, ScoreReason{Name: name, Weight: w})
	}
	// PORT: map order is random; sort before summing so float addition
	// order (and so the last bit of the sum) is fixed.
	slices.SortFunc(s.Reasons, func(a, b ScoreReason) int { return cmp.Compare(a.Name, b.Name) })
	for _, r := range s.Reasons {
		sum += r.Weight
	}
	s.Value = round4(math.Max(0, math.Min(1, sum)))
	return s
}

// round4 rounds half away from zero to 4 decimals (math.Round; Rust's
// f64::round has the same rule).
func round4(f float64) float64 { return math.Round(f*1e4) / 1e4 }

func removedName(n string) string {
	return strings.TrimPrefix(strings.TrimSuffix(n, "_added"), "new_") + "_removed"
}

func returnedName(n string) string {
	return strings.TrimPrefix(strings.TrimSuffix(n, "_added"), "new_") + "_returned"
}

func headerName(key string) string {
	n, _, _ := strings.Cut(key, ":")
	return n
}

// containsWord reports w in s at a boundary on the left (start, or a
// non-letter), so "admin" matches "/admin/users" and "x-admin" but not
// "badminton".
func containsWord(s, w string) bool {
	for off := 0; ; {
		i := strings.Index(s[off:], w)
		if i < 0 {
			return false
		}
		i += off
		if i == 0 || !(s[i-1] >= 'a' && s[i-1] <= 'z') {
			return true
		}
		off = i + 1
	}
}

// cspLoosened reports a policy that got weaker: a directive removed, or
// a source added that allows more ('unsafe-inline', 'unsafe-eval', '*',
// data:, http:, https: or a wildcard host).
func cspLoosened(oldV, newV string) bool {
	parse := func(v string) map[string][]string {
		m := map[string][]string{}
		for d := range strings.SplitSeq(v, ";") {
			fs := strings.Fields(d)
			if len(fs) > 0 {
				m[strings.ToLower(fs[0])] = fs[1:]
			}
		}
		return m
	}
	o, n := parse(oldV), parse(newV)
	for d := range o {
		if _, ok := n[d]; !ok {
			return true
		}
	}
	for d, srcs := range n {
		for _, s := range srcs {
			if slices.Contains(o[d], s) {
				continue
			}
			ls := strings.ToLower(s)
			if ls == "'unsafe-inline'" || ls == "'unsafe-eval'" || ls == "*" || ls == "data:" || ls == "http:" ||
				ls == "https:" || strings.HasPrefix(ls, "*.") || strings.Contains(ls, "://*") {
				return true
			}
		}
	}
	return false
}

// cookieWeakened reports Secure or HttpOnly dropped, or SameSite moved to
// a weaker value (strict > lax > none/absent).
func cookieWeakened(o, n map[string]string) bool {
	rank := func(s string) int {
		switch s {
		case "strict":
			return 2
		case "lax":
			return 1
		}
		return 0
	}
	return o["secure"] == "true" && n["secure"] != "true" || o["httponly"] == "true" && n["httponly"] != "true" ||
		rank(n["samesite"]) < rank(o["samesite"])
}
