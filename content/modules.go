package content

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/jsparse"
)

// Module is one bundler module (or, for scope-hoisted bundles, one
// top-level statement) of a JavaScript bundle (V2 §12.6, level 2.5).
type Module struct {
	// ID is the bundler's module id ("723", "./src/api.js",
	// "node_modules/react/index.js") or "stmt:<n>" for the n-th top-level
	// statement outside any module table.
	ID string `json:"id"`
	// Hash is the hex SHA-256 of the module's canonical token text, with
	// every locally bound identifier renamed to its positional name ($0,
	// $1, … by first occurrence), so a minifier's renaming and any
	// re-formatting leave it unchanged.
	Hash string `json:"hash"`
	// Shape names the bundler shape it was found in: webpack, esbuild or
	// toplevel.
	Shape string `json:"shape"`
	Size  int    `json:"size"`
	// Vendor is set for third-party code: a node_modules path, a hash in
	// the known-vendor set, or a library banner inside the module.
	Vendor  bool   `json:"vendor,omitempty"`
	Library string `json:"library,omitempty"` // "name@version" from a banner inside the module
	Start   int    `json:"start"`
	End     int    `json:"end"`
}

// SplitStrategy selects the module splitter (pluggable decision
// "splitter", benchmark BenchmarkContentAlgo/splitter/<candidate>).
type SplitStrategy uint8

// Module splitters.
const (
	// SplitAuto (default) finds webpack and esbuild module tables and
	// treats every remaining top-level statement as its own unit (the
	// rollup/vite scope-hoisted shape).
	SplitAuto SplitStrategy = iota
	// SplitWebpack finds webpack module tables only; the rest is one
	// runtime unit.
	SplitWebpack
	// SplitESBuild finds esbuild __commonJS/__esm wrappers only.
	SplitESBuild
	// SplitTopLevel uses top-level statements only (no table detection).
	SplitTopLevel
)

// String returns the strategy's configuration name.
func (s SplitStrategy) String() string {
	return [...]string{"auto", "webpack", "esbuild", "toplevel"}[min(int(s), 3)]
}

// ParseSplitStrategy is the inverse of String.
func ParseSplitStrategy(name string) (SplitStrategy, error) {
	for s := SplitAuto; s <= SplitTopLevel; s++ {
		if s.String() == name {
			return s, nil
		}
	}
	return 0, fmt.Errorf("%w: splitter %q", ErrUnknownAlgorithm, name)
}

// SplitOptions configures SplitModules.
type SplitOptions struct {
	Strategy SplitStrategy
	// VendorHashes are module hashes known to be library code (for
	// example from previous captures whose modules had node_modules
	// paths); a module with such a hash is Vendor.
	VendorHashes map[string]bool
}

// SplitModules splits a parsed bundle into modules. prog must have been
// parsed with Options.Tokens. The result is in source order.
func SplitModules(prog *jsparse.Program, opts SplitOptions) []Module {
	if prog.Root == jsparse.NoNode || len(prog.Tokens) == 0 {
		return nil
	}
	sp := splitter{prog: prog, bindings: bindingNames(prog)}
	var regions []Module
	switch opts.Strategy {
	case SplitAuto:
		regions = append(sp.webpackTables(), sp.esbuildWrappers()...)
	case SplitWebpack:
		regions = sp.webpackTables()
	case SplitESBuild:
		regions = sp.esbuildWrappers()
	}
	regions = outermost(regions)
	var out []Module
	for _, r := range regions {
		r.Hash = sp.hash(r.Start, r.End, nil)
		out = append(out, r)
	}
	// Everything outside the module regions: one unit per top-level
	// statement (auto, toplevel), or one runtime unit (webpack, esbuild).
	holes := make([][2]int, len(regions))
	for i, r := range regions {
		holes[i] = [2]int{r.Start, r.End}
	}
	stmts := prog.List(prog.Nodes[prog.Root].List)
	if opts.Strategy == SplitWebpack || opts.Strategy == SplitESBuild {
		if len(stmts) > 0 {
			st, en := int(prog.Nodes[stmts[0]].Span.Start), int(prog.Nodes[stmts[len(stmts)-1]].Span.End)
			if h := sp.hash(st, en, holes); h != "" {
				out = append(out, Module{ID: "runtime", Hash: h, Shape: "runtime", Size: en - st, Start: st, End: en})
			}
		}
	} else {
		for i, s := range stmts {
			n := &prog.Nodes[s]
			st, en := int(n.Span.Start), int(n.Span.End)
			if h := sp.hash(st, en, holes); h != "" {
				out = append(out, Module{ID: "stmt:" + strconv.Itoa(i), Hash: h, Shape: "toplevel", Size: en - st, Start: st, End: en})
			}
		}
	}
	for i := range out {
		m := &out[i]
		if strings.Contains(m.ID, "node_modules/") || opts.VendorHashes[m.Hash] {
			m.Vendor = true
		}
		if lib, ok := sp.bannerIn(m.Start, m.End); ok {
			m.Vendor, m.Library = true, lib.Name+"@"+lib.Version
		}
	}
	slices.SortFunc(out, func(a, b Module) int { return cmp.Or(cmp.Compare(a.Start, b.Start), cmp.Compare(a.ID, b.ID)) })
	return out
}

// outermost drops regions nested in another region.
func outermost(rs []Module) []Module {
	slices.SortFunc(rs, func(a, b Module) int { return cmp.Or(cmp.Compare(a.Start, b.Start), cmp.Compare(b.End, a.End)) })
	var out []Module
	for _, r := range rs {
		if len(out) > 0 && r.Start < out[len(out)-1].End {
			continue
		}
		out = append(out, r)
	}
	return out
}

type splitter struct {
	prog     *jsparse.Program
	bindings map[string]bool
}

func (sp *splitter) node(id jsparse.NodeID) *jsparse.Node { return &sp.prog.Nodes[id] }

func (sp *splitter) isFunction(id jsparse.NodeID, maxParams int) bool {
	if id == jsparse.NoNode {
		return false
	}
	n := sp.node(id)
	return (n.Kind == jsparse.KFunctionExpression || n.Kind == jsparse.KArrowFunctionExpression) && int(n.List.Len) <= maxParams
}

// moduleKey returns a property key that names a module: a number, or a
// string that looks like a path ("./src/a.js", "node_modules/x").
func (sp *splitter) moduleKey(key jsparse.NodeID) (string, bool) {
	k := sp.node(key)
	switch {
	case k.Kind == jsparse.KLiteral && k.Op == jsparse.LitNumber:
		return jsparse.NumberToString(k.Num), true
	case k.Kind == jsparse.KLiteral && k.Op == jsparse.LitString:
		s := sp.prog.Strings[k.Str]
		if strings.ContainsAny(s, "/.") || isAllDigits(s) {
			return s, true
		}
	}
	return "", false
}

// webpackTables finds webpack module tables (webpack 4/5 "modules" and
// "chunk push" shapes): an object literal with at least two properties,
// every key a module id and every value a function of at most three
// parameters (module, exports, __webpack_require__); or an array literal
// of at least two such functions (holes allowed), the webpack 4 array
// form, ids being the indices.
func (sp *splitter) webpackTables() []Module {
	var out []Module
	p := sp.prog
	p.Walk(p.Root, func(id jsparse.NodeID) bool {
		n := sp.node(id)
		switch n.Kind {
		case jsparse.KObjectExpression:
			props := p.List(n.List)
			if len(props) < 2 {
				return true
			}
			var mods []Module
			for _, pr := range props {
				pn := sp.node(pr)
				if pn.Kind != jsparse.KProperty || pn.Has(jsparse.NComputed) {
					return true
				}
				key, ok := sp.moduleKey(pn.A)
				fn := pn.B
				if !ok || !sp.isFunction(fn, 3) {
					return true
				}
				s := sp.node(fn).Span
				mods = append(mods, Module{ID: key, Shape: "webpack", Start: int(s.Start), End: int(s.End), Size: int(s.End - s.Start)})
			}
			out = append(out, mods...)
			return false
		case jsparse.KArrayExpression:
			els := p.List(n.List)
			fns := 0
			for _, e := range els {
				if e == jsparse.NoNode {
					continue
				}
				if !sp.isFunction(e, 3) {
					return true
				}
				fns++
			}
			if fns < 2 {
				return true
			}
			for i, e := range els {
				if e == jsparse.NoNode {
					continue
				}
				s := sp.node(e).Span
				out = append(out, Module{ID: strconv.Itoa(i), Shape: "webpack", Start: int(s.Start), End: int(s.End), Size: int(s.End - s.Start)})
			}
			return false
		}
		return true
	})
	return out
}

// esbuildWrappers finds esbuild's lazy module wrappers: a call whose only
// argument is an object literal with exactly one property, keyed by a
// module path, whose value is a function or method
// (`__commonJS({"node_modules/x/index.js"(exports, module) {...}})`,
// `__esm({"src/a.ts"() {...}})`, minified callee names included).
func (sp *splitter) esbuildWrappers() []Module {
	var out []Module
	p := sp.prog
	p.Walk(p.Root, func(id jsparse.NodeID) bool {
		n := sp.node(id)
		if n.Kind != jsparse.KCallExpression || n.List.Len != 1 {
			return true
		}
		arg := sp.node(p.List(n.List)[0])
		if arg.Kind != jsparse.KObjectExpression || arg.List.Len != 1 {
			return true
		}
		pr := sp.node(p.List(arg.List)[0])
		if pr.Kind != jsparse.KProperty || pr.Has(jsparse.NComputed) {
			return true
		}
		key, ok := sp.moduleKey(pr.A)
		if !ok || isAllDigits(key) || !sp.isFunction(pr.B, 4) {
			return true
		}
		s := pr.Span
		out = append(out, Module{ID: key, Shape: "esbuild", Start: int(s.Start), End: int(s.End), Size: int(s.End - s.Start)})
		return false
	})
	return out
}

// bindingNames collects every identifier the program binds: variable,
// function, class, parameter, catch and import bindings, through
// destructuring patterns. Free identifiers (window, fetch, document) are
// not bindings and keep their names in module hashes, so a change in what
// a module calls is still a change.
func bindingNames(p *jsparse.Program) map[string]bool {
	out := map[string]bool{}
	var pattern func(id jsparse.NodeID, depth int)
	pattern = func(id jsparse.NodeID, depth int) {
		if id == jsparse.NoNode || depth > 64 {
			return
		}
		n := &p.Nodes[id]
		switch n.Kind {
		case jsparse.KIdentifier:
			out[p.Strings[n.Str]] = true
		case jsparse.KObjectPattern:
			for _, c := range p.List(n.List) {
				cn := &p.Nodes[c]
				if cn.Kind == jsparse.KProperty {
					pattern(cn.B, depth+1)
				} else {
					pattern(c, depth+1)
				}
			}
		case jsparse.KArrayPattern:
			for _, c := range p.List(n.List) {
				pattern(c, depth+1)
			}
		case jsparse.KAssignmentPattern, jsparse.KRestElement:
			pattern(n.A, depth+1)
		}
	}
	for i := range p.Nodes {
		n := &p.Nodes[i]
		switch n.Kind {
		case jsparse.KVariableDeclarator, jsparse.KCatchClause:
			pattern(n.A, 0)
		case jsparse.KFunctionDeclaration, jsparse.KFunctionExpression, jsparse.KArrowFunctionExpression:
			if n.Kind != jsparse.KArrowFunctionExpression {
				pattern(n.A, 0)
			}
			for _, prm := range p.List(n.List) {
				pattern(prm, 0)
			}
		case jsparse.KClassDeclaration, jsparse.KClassExpression:
			pattern(n.A, 0)
		case jsparse.KImportSpecifier:
			pattern(n.B, 0)
		case jsparse.KImportDefaultSpecifier, jsparse.KImportNamespaceSpecifier:
			pattern(n.A, 0)
		}
	}
	return out
}

// tokenRange returns the indexes [i, j) of prog.Tokens inside [start, end).
func (sp *splitter) tokenRange(start, end int) (int, int) {
	toks := sp.prog.Tokens
	i, _ := slices.BinarySearchFunc(toks, start, func(t jsparse.Token, off int) int { return cmp.Compare(int(t.Start), off) })
	j, _ := slices.BinarySearchFunc(toks, end, func(t jsparse.Token, off int) int { return cmp.Compare(int(t.Start), off) })
	return i, j
}

// canonicalTokens renders the significant tokens of [start, end) minus
// holes as canonical text: bound identifiers outside property positions
// renamed positionally (or all to "$" when anonymous), strings quoted
// canonically, numbers by value, volatile literals masked.
func (sp *splitter) canonicalTokens(start, end int, holes [][2]int, anonymous bool) []string {
	i, j := sp.tokenRange(start, end)
	toks := sp.prog.Tokens
	names := map[string]int{}
	var out []string
	prevKind := jsparse.TEOF
	for k := i; k < j; k++ {
		t := &toks[k]
		if t.Kind.IsTrivia() || t.Kind == jsparse.TEOF {
			continue
		}
		if overlapsAny([2]int{int(t.Start), int(t.End)}, holes) {
			continue
		}
		var s string
		switch t.Kind {
		case jsparse.TIdentifier:
			s = t.Value
			if t.Kw == 0 && sp.bindings[t.Value] && prevKind != jsparse.TDot && prevKind != jsparse.TQuestionDot && !sp.isObjectKey(k, j) {
				if anonymous {
					s = "$"
				} else {
					n, ok := names[t.Value]
					if !ok {
						n = len(names)
						names[t.Value] = n
					}
					s = "$" + strconv.Itoa(n)
				}
			}
		case jsparse.TString:
			v := t.Value
			if c, ok := volatileScalar(v); ok {
				v = maskPlaceholder(c)
			}
			s = strconv.Quote(v)
		case jsparse.TNumeric:
			if t.Num >= minEpochSeconds*1000 && t.Num <= maxEpochSeconds*1000 {
				s = maskPlaceholder(MaskTimestamp)
			} else {
				s = jsparse.NumberToString(t.Num)
			}
		case jsparse.TRegExp:
			s = "/" + t.Value + "/" + t.Raw
		case jsparse.TNoSubstitutionTemplate, jsparse.TTemplateHead, jsparse.TTemplateMiddle, jsparse.TTemplateTail:
			s = "`" + strconv.Quote(t.Raw)
		default:
			s = sp.prog.Source[t.Start:t.End]
		}
		out = append(out, s)
		prevKind = t.Kind
	}
	return out
}

// isObjectKey reports an identifier token k used as an object-literal key
// or class member name (`{k: v}`, `, k:`): previous significant token "{"
// or "," and next ":".
func (sp *splitter) isObjectKey(k, limit int) bool {
	toks := sp.prog.Tokens
	prev, next := jsparse.TEOF, jsparse.TEOF
	for a := k - 1; a >= 0; a-- {
		if !toks[a].Kind.IsTrivia() {
			prev = toks[a].Kind
			break
		}
	}
	for b := k + 1; b < len(toks) && b < limit+1; b++ {
		if !toks[b].Kind.IsTrivia() {
			next = toks[b].Kind
			break
		}
	}
	return next == jsparse.TColon && (prev == jsparse.TLBrace || prev == jsparse.TComma)
}

func (sp *splitter) hash(start, end int, holes [][2]int) string {
	toks := sp.canonicalTokens(start, end, holes, false)
	if len(toks) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(toks, " ")))
	return hex.EncodeToString(sum[:])
}

func (sp *splitter) bannerIn(start, end int) (Library, bool) {
	for _, c := range sp.prog.Comments {
		if int(c.Span.Start) < start || int(c.Span.End) > end || c.Kind != jsparse.TBlockComment {
			continue
		}
		text := sp.prog.Source[c.Span.Start:c.Span.End]
		if strings.HasPrefix(text, "/*!") || strings.Contains(text, "@license") {
			if lib, ok := parseBanner(text); ok {
				return lib, true
			}
		}
	}
	return Library{}, false
}

// ModuleChange is one module-level change.
type ModuleChange struct {
	// Op is added, removed or changed (same id, different hash).
	Op      string `json:"op"`
	ID      string `json:"id"`
	OldHash string `json:"old_hash,omitempty"`
	NewHash string `json:"new_hash,omitempty"`
}

// ModuleDiff compares two module lists.
type ModuleDiff struct {
	Unchanged int `json:"unchanged"`
	// Moved counts unchanged modules whose id or position changed: no
	// change, wherever they moved (V2 §12.6).
	Moved int `json:"moved"`
	// VendorSuppressed counts changes in vendor modules; they are not
	// reported (a library change is reported as a version change by the
	// libraries extractor instead).
	VendorSuppressed int            `json:"vendor_suppressed"`
	Changes          []ModuleChange `json:"changes,omitempty"`
}

// DiffModules matches modules by hash as a multiset first (so moves and
// reorderings are free), then pairs the rest by id (changed), and reports
// what remains as added or removed. Vendor modules never appear in
// Changes.
func DiffModules(old, new []Module) ModuleDiff {
	var d ModuleDiff
	pool := map[string][]int{} // hash → unmatched old indexes, in source order
	for i, m := range old {
		pool[m.Hash] = append(pool[m.Hash], i)
	}
	usedOld := make([]bool, len(old))
	var restNew []Module
	for i, m := range new {
		if idx := pool[m.Hash]; len(idx) > 0 {
			oi := idx[0]
			pool[m.Hash] = idx[1:]
			usedOld[oi] = true
			if old[oi].ID != m.ID || oi != i {
				d.Moved++
			} else {
				d.Unchanged++
			}
			continue
		}
		restNew = append(restNew, m)
	}
	oldByID := map[string]int{}
	for i, m := range old {
		if !usedOld[i] {
			if _, dup := oldByID[m.ID]; !dup {
				oldByID[m.ID] = i
			}
		}
	}
	add := func(c ModuleChange, vendor bool) {
		if vendor {
			d.VendorSuppressed++
			return
		}
		d.Changes = append(d.Changes, c)
	}
	for _, m := range restNew {
		// Top-level statement ids are ordinals, so they do not pair.
		if i, ok := oldByID[m.ID]; ok && !usedOld[i] && !strings.HasPrefix(m.ID, "stmt:") {
			usedOld[i] = true
			add(ModuleChange{Op: "changed", ID: m.ID, OldHash: old[i].Hash, NewHash: m.Hash}, m.Vendor || old[i].Vendor)
			continue
		}
		add(ModuleChange{Op: "added", ID: m.ID, NewHash: m.Hash}, m.Vendor)
	}
	for i, m := range old {
		if !usedOld[i] {
			add(ModuleChange{Op: "removed", ID: m.ID, OldHash: m.Hash}, m.Vendor)
		}
	}
	slices.SortFunc(d.Changes, func(a, b ModuleChange) int {
		return cmp.Or(cmp.Compare(a.Op, b.Op), cmp.Compare(a.ID, b.ID), cmp.Compare(a.OldHash+a.NewHash, b.OldHash+b.NewHash))
	})
	return d
}
