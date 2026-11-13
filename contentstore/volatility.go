package contentstore

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
)

// Learned volatility (V2 §5): per (URL template, profile, feature path)
// the store counts how often a path's value changed while the page was
// otherwise stable. The paths come from comparing two normalized
// versions: HTML attribute values (html:attr:<tag>.<attr>), JSON leaf
// values (json:<pointer>, array indices as "*") and response headers
// (header:<name>). Feature paths (content.IsFeaturePath) are never
// counted, since a rule on them is refused anyway (V2 §12.5).

// maxVolatilityPaths bounds the paths observed per artifact.
const maxVolatilityPaths = 256

// pathValues maps a path to its values in one version, sorted.
type pathValues map[string][]string

// observePaths extracts the volatility paths of a normalized text.
func observePaths(kind content.Kind, text []byte) pathValues {
	pv := pathValues{}
	add := func(p, v string) {
		if content.IsFeaturePath(p) {
			return
		}
		if _, ok := pv[p]; !ok && len(pv) >= maxVolatilityPaths {
			return
		}
		pv[p] = append(pv[p], v)
	}
	switch kind {
	case content.KindHTML:
		for line := range bytes.SplitSeq(text, []byte("\n")) {
			htmlLineAttrs(line, add)
		}
	case content.KindJSON:
		jsonLeaves(text, add)
	case content.KindHeaders:
		for line := range bytes.SplitSeq(text, []byte("\n")) {
			name, val, ok := bytes.Cut(line, []byte(": "))
			if ok && len(name) > 0 {
				add("header:"+strings.ToLower(string(name)), string(val))
			}
		}
	}
	for p := range pv {
		slices.Sort(pv[p])
	}
	return pv
}

// htmlLineAttrs reads one html@1 start-tag line, `<tag a="v" b="w">`,
// whose attribute values are double-quoted with '"' escaped as &quot;.
func htmlLineAttrs(line []byte, add func(path, value string)) {
	if len(line) < 3 || line[0] != '<' || line[1] == '/' || line[1] == '!' || line[len(line)-1] != '>' {
		return
	}
	s := string(line[1 : len(line)-1])
	i := strings.IndexByte(s, ' ')
	if i < 0 {
		return
	}
	tag, rest := strings.ToLower(s[:i]), s[i+1:]
	for rest != "" {
		rest = strings.TrimLeft(rest, " ")
		eq := strings.IndexAny(rest, "= ")
		if eq < 0 {
			return // a final boolean attribute
		}
		name := strings.ToLower(rest[:eq])
		if rest[eq] == ' ' {
			rest = rest[eq:]
			continue
		}
		rest = rest[eq+1:]
		if !strings.HasPrefix(rest, `"`) {
			return
		}
		end := strings.IndexByte(rest[1:], '"')
		if end < 0 {
			return
		}
		add("html:attr:"+tag+"."+name, rest[1:1+end])
		rest = rest[end+2:]
	}
}

// jsonLeaves walks canonical JSON with a token decoder (no generic
// values) and reports every scalar leaf under its RFC 6901 pointer, with
// array indices replaced by "*" so the path generalizes across items.
func jsonLeaves(text []byte, add func(path, value string)) {
	dec := json.NewDecoder(bytes.NewReader(text))
	dec.UseNumber()
	type frame struct {
		array bool
		key   string
		want  bool // object: the next string token is a key
	}
	var stack []frame
	pointer := func() string {
		var b strings.Builder
		for _, f := range stack {
			b.WriteByte('/')
			if f.array {
				b.WriteByte('*')
			} else {
				b.WriteString(strings.NewReplacer("~", "~0", "/", "~1").Replace(f.key))
			}
		}
		return b.String()
	}
	value := func() {
		if n := len(stack); n > 0 && !stack[n-1].array {
			stack[n-1].want = true
		}
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF || err != nil {
			return
		}
		if n := len(stack); n > 0 && !stack[n-1].array && stack[n-1].want {
			if k, ok := tok.(string); ok {
				stack[n-1].key, stack[n-1].want = k, false
				continue
			}
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, frame{want: true})
			case '[':
				stack = append(stack, frame{array: true})
			case '}', ']':
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
				value()
			}
		default:
			var v string
			switch x := t.(type) {
			case string:
				v = strconv.Quote(x)
			case json.Number:
				v = x.String()
			case bool:
				v = strconv.FormatBool(x)
			case nil:
				v = "null"
			}
			add("json:"+pointer(), v)
			value()
		}
	}
}

// volatilityObservation compares two versions' paths: a path present in
// both changed when its sorted values differ.
func volatilityObservation(prev, cur pathValues) (changed, stable []string) {
	for p, v := range cur {
		old, ok := prev[p]
		if !ok {
			continue
		}
		if slices.Equal(old, v) {
			stable = append(stable, p)
		} else {
			changed = append(changed, p)
		}
	}
	slices.Sort(changed)
	slices.Sort(stable)
	return changed, stable
}

// learnedSet rebuilds the frozen VolatilitySet from the rows: every
// frozen path, version = the highest freezing version.
func learnedSet(rows []VolatilityRow) content.VolatilitySet {
	var set content.VolatilitySet
	for _, r := range rows {
		if r.FrozenVersion > 0 {
			set.Rules = append(set.Rules, content.VolatileRule{Path: r.Path})
			set.Version = max(set.Version, r.FrozenVersion)
		}
	}
	slices.SortFunc(set.Rules, func(a, b content.VolatileRule) int { return strings.Compare(a.Path, b.Path) })
	return set
}

// updateVolatility records one observation per compared path of the
// page's main artifact and headers (only when the capture had no level-3
// change: "changed while page hash otherwise stable"), then lets
// content.LearnVolatility freeze the paths that pass the policy. It
// returns the rows to write.
func updateVolatility(rows []VolatilityRow, program core.UUID, tmpl, profile string, prev, cur *content.Processed,
	outcome string, policy content.VolatilityPolicy) []VolatilityRow {
	if prev == nil || outcome == content.DiffChanged || outcome == content.DiffDegraded {
		return nil
	}
	by := map[string]VolatilityRow{}
	for _, r := range rows {
		by[r.Path] = r
	}
	var touched []string
	headers := func(p *content.Processed) *content.Artifact {
		for i := range p.Artifacts {
			if p.Artifacts[i].DocID == p.URL+"#headers" {
				return &p.Artifacts[i]
			}
		}
		return nil
	}
	pairs := [][2]*content.Artifact{{prev.Main(), cur.Main()}, {headers(prev), headers(cur)}}
	for _, pr := range pairs {
		if pr[0] == nil || pr[1] == nil || pr[0].Kind != pr[1].Kind {
			continue
		}
		changed, stable := volatilityObservation(observePaths(pr[0].Kind, pr[0].Normalized.Text), observePaths(pr[1].Kind, pr[1].Normalized.Text))
		for _, list := range []struct {
			paths   []string
			changed bool
		}{{changed, true}, {stable, false}} {
			for _, p := range list.paths {
				r, ok := by[p]
				if !ok {
					r = VolatilityRow{Program: program, URLTemplate: tmpl, Profile: profile, Path: p}
				}
				if r.FrozenVersion > 0 {
					continue
				}
				c := content.VolatilityCounter{Path: p, Changes: r.Changes, Stable: r.Stable, Streak: r.Streak}.Record(list.changed)
				r.Changes, r.Stable, r.Streak = c.Changes, c.Stable, c.Streak
				by[p] = r
				touched = append(touched, p)
			}
		}
	}
	if len(touched) == 0 {
		return nil
	}
	var counters []content.VolatilityCounter
	for _, p := range sortedKeys(by) {
		r := by[p]
		if r.FrozenVersion == 0 {
			counters = append(counters, content.VolatilityCounter{Path: r.Path, Changes: r.Changes, Stable: r.Stable, Streak: r.Streak})
		}
	}
	prevSet := learnedSet(rows)
	next, _ := content.LearnVolatility(prevSet, counters, policy)
	if next.Version != prevSet.Version {
		for _, rule := range next.Rules {
			if r, ok := by[rule.Path]; ok && r.FrozenVersion == 0 {
				r.FrozenVersion = next.Version
				by[rule.Path] = r
				touched = append(touched, rule.Path)
			}
		}
	}
	slices.Sort(touched)
	touched = slices.Compact(touched)
	out := make([]VolatilityRow, 0, len(touched))
	for _, p := range touched {
		out = append(out, by[p])
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
