package content

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// VolatileRule is one learned volatility rule: a feature path whose value
// changes while the page is otherwise stable (V2 §5 "learned
// volatility"). Path forms:
//
//	html:attr:<tag>.<attr>   attribute values (<tag> may be "*")
//	json:<pointer>           an RFC 6901 pointer; a "*" token matches any key or index
//	header:<name>            a response header; only non-security headers are
//	                         allowed, and the block already drops those, so
//	                         such a rule records the learning without effect
type VolatileRule struct {
	Path string `json:"path"`
}

// VolatilitySet is a frozen set of learned rules. Version 0 with no rules
// is "no learned volatility". A set is an explicit normalizer input: it
// changes normalizer_version (Normalized.Version gets ".learned<n>"), so
// it is never applied silently and every diff replays with the exact
// rules it used.
type VolatilitySet struct {
	Version int            `json:"version"`
	Rules   []VolatileRule `json:"rules,omitempty"`
}

// Empty reports a set without rules.
func (s VolatilitySet) Empty() bool { return len(s.Rules) == 0 }

// Validate checks every rule path and refuses rules on level-3 feature
// paths (ErrMaskOnFeaturePath; V2 §12.5: learned volatility never masks a
// level-3 feature path). Rules must be sorted and unique.
func (s VolatilitySet) Validate() error {
	if s.Version < 0 || s.Version == 0 && len(s.Rules) > 0 {
		return fmt.Errorf("%w: a set with rules needs a version > 0", ErrInvalidRule)
	}
	for i, r := range s.Rules {
		if err := validateRulePath(r.Path); err != nil {
			return err
		}
		if i > 0 && s.Rules[i-1].Path >= r.Path {
			return fmt.Errorf("%w: rules must be sorted and unique (%q)", ErrInvalidRule, r.Path)
		}
	}
	return nil
}

func validateRulePath(p string) error {
	kind, rest, ok := strings.Cut(p, ":")
	if !ok || rest == "" || len(p) > 256 || hasCtl(p) {
		return fmt.Errorf("%w: path %q", ErrInvalidRule, p)
	}
	switch kind {
	case "html":
		spec, ok := strings.CutPrefix(rest, "attr:")
		tag, attr, ok2 := strings.Cut(spec, ".")
		if !ok || !ok2 || tag == "" || attr == "" || strings.ToLower(spec) != spec {
			return fmt.Errorf("%w: html path %q (want html:attr:<tag>.<attr>)", ErrInvalidRule, p)
		}
	case "json":
		if !strings.HasPrefix(rest, "/") {
			return fmt.Errorf("%w: json path %q (want json:/pointer)", ErrInvalidRule, p)
		}
	case "header":
		if strings.ToLower(rest) != rest {
			return fmt.Errorf("%w: header path %q must be lower case", ErrInvalidRule, p)
		}
	default:
		return fmt.Errorf("%w: unknown path kind %q", ErrInvalidRule, kind)
	}
	if IsFeaturePath(p) {
		return fmt.Errorf("%w: %q", ErrMaskOnFeaturePath, p)
	}
	return nil
}

// featureAttrs are the HTML (tag, attribute) pairs the extractors read
// for level-3 features; "*" stands for any tag.
var featureAttrs = map[string]bool{
	"a.href": true, "area.href": true, "link.href": true, "link.rel": true, "link.as": true, "script.src": true,
	"script.type": true, "script.integrity": true, "iframe.src": true, "frame.src": true, "embed.src": true,
	"object.data": true, "form.action": true, "form.method": true, "form.enctype": true, "input.name": true,
	"input.type": true, "select.name": true, "textarea.name": true, "button.formaction": true,
	"button.formmethod": true, "input.formaction": true, "meta.content": true, "meta.name": true, "base.href": true,
}

// IsFeaturePath reports whether a rule path addresses something a level-3
// extractor reads: the attributes in featureAttrs (and any attribute of a
// wildcard tag that is one of them, and event handlers, which become the
// #handlers document), the OpenAPI "paths" subtree, or a security header.
func IsFeaturePath(p string) bool {
	kind, rest, _ := strings.Cut(p, ":")
	switch kind {
	case "html":
		spec := strings.TrimPrefix(rest, "attr:")
		tag, attr, _ := strings.Cut(spec, ".")
		if strings.HasPrefix(attr, "on") || featureAttrs[spec] {
			return true
		}
		if tag == "*" {
			for k := range featureAttrs {
				if strings.HasSuffix(k, "."+attr) {
					return true
				}
			}
		}
	case "json":
		return rest == "/paths" || strings.HasPrefix(rest, "/paths/") || rest == "/" || rest == "/*" ||
			strings.HasPrefix(rest, "/data/__schema") || strings.HasPrefix(rest, "/*/")
	case "header":
		return securityHeaders[rest]
	}
	return false
}

// htmlAttrRule reports whether a learned rule masks tag.attr.
func (s VolatilitySet) htmlAttrRule(tag, attr string) bool {
	for _, r := range s.Rules {
		spec, ok := strings.CutPrefix(r.Path, "html:attr:")
		if !ok {
			continue
		}
		t, a, _ := strings.Cut(spec, ".")
		if a == attr && (t == tag || t == "*") {
			return true
		}
	}
	return false
}

// jsonPointerRule reports whether a learned rule masks the value at the
// pointer tokens.
func (s VolatilitySet) jsonPointerRule(tokens []string) bool {
	for _, r := range s.Rules {
		ptr, ok := strings.CutPrefix(r.Path, "json:")
		if !ok {
			continue
		}
		pt := strings.Split(ptr[1:], "/")
		if len(pt) != len(tokens) {
			continue
		}
		match := true
		for i, t := range pt {
			t = strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
			if t != "*" && t != tokens[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// ruleLines renders the set for the rule-table hash.
func (s VolatilitySet) ruleLines() []string {
	out := make([]string, 0, len(s.Rules)+1)
	if len(s.Rules) > 0 {
		out = append(out, fmt.Sprintf("learned:set=%d", s.Version))
	}
	for _, r := range s.Rules {
		out = append(out, "learned:"+r.Path)
	}
	return out
}

// VolatilityCounter is the per-(URL template, profile, feature path)
// state the content store keeps: how often the path's value changed while
// the page was otherwise stable, how often it stayed the same, and the
// current run of consecutive changes.
type VolatilityCounter struct {
	Path    string `json:"path"`
	Changes int    `json:"changes"`
	Stable  int    `json:"stable"`
	Streak  int    `json:"streak"`
}

// Record returns the counter after one more observation.
func (c VolatilityCounter) Record(changed bool) VolatilityCounter {
	if changed {
		c.Changes++
		c.Streak++
	} else {
		c.Stable++
		c.Streak = 0
	}
	return c
}

// VolatilityAlgo selects the volatility detector (a pluggable decision,
// CONVENTIONS "Pluggable algorithms"; benchmark
// BenchmarkContentAlgo/volatility/<candidate>).
type VolatilityAlgo uint8

// Volatility detectors.
const (
	// VolatilityRatio (default): volatile once at least MinObservations
	// were seen and changes/(changes+stable) >= MinRatio. Robust to the
	// occasional stable observation.
	VolatilityRatio VolatilityAlgo = iota
	// VolatilityStreak: volatile after MinStreak consecutive changes.
	// Learns faster, but one stable observation resets it.
	VolatilityStreak
)

// String returns "ratio" or "streak".
func (a VolatilityAlgo) String() string {
	if a == VolatilityStreak {
		return "streak"
	}
	return "ratio"
}

// VolatilityPolicy configures LearnVolatility; zero fields take defaults
// (8 observations, ratio 0.9, streak 5).
type VolatilityPolicy struct {
	Algo            VolatilityAlgo
	MinObservations int
	MinRatio        float64
	MinStreak       int
}

func (p VolatilityPolicy) withDefaults() VolatilityPolicy {
	if p.MinObservations <= 0 {
		p.MinObservations = 8
	}
	if p.MinRatio <= 0 {
		p.MinRatio = 0.9
	}
	if p.MinStreak <= 0 {
		p.MinStreak = 5
	}
	return p
}

// Volatile applies the policy to one counter.
func (p VolatilityPolicy) Volatile(c VolatilityCounter) bool {
	p = p.withDefaults()
	if p.Algo == VolatilityStreak {
		return c.Streak >= p.MinStreak
	}
	n := c.Changes + c.Stable
	return n >= p.MinObservations && float64(c.Changes) >= p.MinRatio*float64(n)
}

// LearnVolatility freezes the counters that pass the policy into a new
// VolatilitySet (prev.Version+1) when at least one rule is new, and
// returns prev unchanged otherwise. Paths that are level-3 feature paths
// or malformed are never frozen; they are returned in refused (sorted),
// so the store can audit them.
func LearnVolatility(prev VolatilitySet, counters []VolatilityCounter, p VolatilityPolicy) (next VolatilitySet, refused []string) {
	have := map[string]bool{}
	for _, r := range prev.Rules {
		have[r.Path] = true
	}
	var added []VolatileRule
	for _, c := range counters {
		if have[c.Path] || !p.Volatile(c) {
			continue
		}
		if err := validateRulePath(c.Path); err != nil {
			refused = append(refused, c.Path)
			continue
		}
		have[c.Path] = true
		added = append(added, VolatileRule{Path: c.Path})
	}
	slices.Sort(refused)
	refused = slices.Compact(refused)
	if len(added) == 0 {
		return prev, refused
	}
	rules := append(slices.Clone(prev.Rules), added...)
	slices.SortFunc(rules, func(a, b VolatileRule) int { return cmp.Compare(a.Path, b.Path) })
	return VolatilitySet{Version: prev.Version + 1, Rules: rules}, refused
}
