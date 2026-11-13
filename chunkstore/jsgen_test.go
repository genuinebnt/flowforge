package chunkstore

import (
	"slices"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/core"
)

// jsBundle is a seeded generator of realistic, minified, webpack-like
// JavaScript bundles used by the acceptance benchmark. A bundle is a list
// of modules; a module is a list of statements; a statement is a list of
// parts, each a literal or a reference into the shared identifier pool,
// so renaming an identifier (as a minifier or a refactor does) changes
// every statement that uses it.
type jsBundle struct {
	r       *core.SplitMix64
	pool    []string // identifier pool; parts refer to it by index
	strs    []string
	modules []jsModule
	nextID  int
}

type jsPart struct {
	lit   string
	ident int // -1 for a literal
}

type jsModule struct {
	id    int
	vocab []int // identifiers this module uses
	stmts [][]jsPart
}

var jsSyllables = []string{
	"get", "set", "user", "data", "handle", "on", "click", "render", "state", "props",
	"api", "fetch", "item", "list", "config", "value", "index", "node", "el", "cb",
	"error", "promise", "update", "create", "delete", "load", "save", "token", "auth",
	"session", "cart", "price", "query", "route", "view", "modal", "form", "input",
	"submit", "valid", "parse", "format", "date", "time", "count", "total", "page",
}

var jsWords = []string{
	"Loading", "Something went wrong", "Please try again", "Submit", "Cancel",
	"Your session has expired", "Add to cart", "Checkout", "Sign in", "Sign out",
	"application/json", "Content-Type", "Authorization", "Bearer ", "click",
	"keydown", "change", "hidden", "active", "disabled", "is-open", "data-id",
}

func newJSBundle(seed uint64, target int) *jsBundle {
	b := &jsBundle{r: core.NewSplitMix64(seed)}
	for range 3000 {
		b.pool = append(b.pool, b.newIdent())
	}
	for range 400 {
		b.strs = append(b.strs, b.newString())
	}
	size := 0
	for size < target {
		m := b.newModule()
		b.modules = append(b.modules, m)
		size += b.moduleLen(m)
	}
	return b
}

func (b *jsBundle) intn(n int) int { return core.IntN(b.r, n) }

func (b *jsBundle) newIdent() string {
	parts := 1 + b.intn(3)
	var s strings.Builder
	for i := range parts {
		w := jsSyllables[b.intn(len(jsSyllables))]
		if i > 0 {
			w = strings.ToUpper(w[:1]) + w[1:]
		}
		s.WriteString(w)
	}
	if b.intn(4) == 0 {
		s.WriteString(strconv.Itoa(b.intn(100)))
	}
	return s.String()
}

func (b *jsBundle) newString() string {
	switch b.intn(3) {
	case 0:
		return "/api/v" + strconv.Itoa(1+b.intn(3)) + "/" + jsSyllables[b.intn(len(jsSyllables))] + "/" + jsSyllables[b.intn(len(jsSyllables))]
	case 1:
		return jsWords[b.intn(len(jsWords))]
	}
	return jsWords[b.intn(len(jsWords))] + " " + jsSyllables[b.intn(len(jsSyllables))]
}

// pickIdent draws from the module vocabulary with a skew towards its
// first entries, as real code reuses a few names heavily.
func (b *jsBundle) pickIdent(m *jsModule) jsPart {
	i := b.intn(len(m.vocab))
	if b.intn(2) == 0 {
		i = b.intn(1 + len(m.vocab)/4)
	}
	return jsPart{ident: m.vocab[i]}
}

func lit(s string) jsPart { return jsPart{lit: s, ident: -1} }

var minNames = []string{"e", "t", "n", "r", "o", "i", "a", "s", "u", "c"}

func (b *jsBundle) newStmt(m *jsModule) []jsPart {
	id := func() jsPart { return b.pickIdent(m) }
	str := func() jsPart { return lit(strconv.Quote(b.strs[b.intn(len(b.strs))])) }
	num := func() jsPart { return lit(strconv.Itoa(b.intn(10000))) }
	p := func() jsPart { return lit(minNames[b.intn(len(minNames))]) }
	switch b.intn(9) {
	case 0:
		return []jsPart{lit("var "), id(), lit("=n("), num(), lit(");")}
	case 1:
		return []jsPart{lit("function "), id(), lit("("), p(), lit(","), p(), lit("){return "), p(), lit("."), id(), lit("("), p(), lit(")||"), id(), lit("}")}
	case 2:
		return []jsPart{id(), lit(".prototype."), id(), lit("=function("), p(), lit("){this."), id(), lit("="), p(), lit(";return this};")}
	case 3:
		return []jsPart{lit("const "), id(), lit("={"), id(), lit(":"), str(), lit(","), id(), lit(":"), num(), lit(",items:["), num(), lit(","), num(), lit("]};")}
	case 4:
		return []jsPart{lit("if("), id(), lit("&&"), id(), lit("."), id(), lit("){"), id(), lit("("), str(), lit(")}else{"), id(), lit("()}")}
	case 5:
		return []jsPart{lit("fetch("), str(), lit("+"), p(), lit(",{method:\"POST\",headers:{"), str(), lit(":"), str(), lit("}}).then(function(e){return e.json()}).then("), id(), lit(");")}
	case 6:
		return []jsPart{id(), lit(".addEventListener("), str(), lit(",function(e){e.preventDefault();"), id(), lit("(e.target.value,"), num(), lit(")});")}
	case 7:
		return []jsPart{lit("return "), id(), lit("?"), str(), lit(":"), id(), lit("("), num(), lit(")}")}
	}
	return []jsPart{lit("for(var "), p(), lit("=0;"), p(), lit("<"), id(), lit(".length;"), p(), lit("++)"), id(), lit("("), id(), lit("["), p(), lit("]);")}
}

func (b *jsBundle) newModule() jsModule {
	m := jsModule{id: b.nextID}
	b.nextID += 1 + b.intn(7)
	for range 8 + b.intn(16) {
		m.vocab = append(m.vocab, b.intn(len(b.pool)))
	}
	for range 10 + b.intn(30) {
		m.stmts = append(m.stmts, b.newStmt(&m))
	}
	return m
}

func (b *jsBundle) renderModule(sb *strings.Builder, m jsModule) {
	sb.WriteString("/***/ ")
	sb.WriteString(strconv.Itoa(m.id))
	sb.WriteString(":function(e,t,n){\"use strict\";")
	for _, st := range m.stmts {
		for _, p := range st {
			if p.ident >= 0 {
				sb.WriteString(b.pool[p.ident])
			} else {
				sb.WriteString(p.lit)
			}
		}
	}
	sb.WriteString("},\n")
}

func (b *jsBundle) moduleLen(m jsModule) int {
	var sb strings.Builder
	b.renderModule(&sb, m)
	return sb.Len()
}

// Bytes renders the bundle.
func (b *jsBundle) Bytes() []byte {
	var sb strings.Builder
	sb.WriteString("!function(e){var t={};function n(r){if(t[r])return t[r].exports}}({\n")
	for _, m := range b.modules {
		b.renderModule(&sb, m)
	}
	sb.WriteString("});\n//# sourceMappingURL=main.js.map\n")
	return []byte(sb.String())
}

// NextVersion applies a deploy's worth of edits: one to four small
// statement edits (insert, delete, change a literal), occasionally a new
// module, and every tenth version a rename of one identifier everywhere.
func (b *jsBundle) NextVersion(version int) {
	for range 1 + b.intn(4) {
		m := &b.modules[b.intn(len(b.modules))]
		i := b.intn(len(m.stmts))
		switch b.intn(3) {
		case 0:
			m.stmts = slices.Insert(m.stmts, i+1, b.newStmt(m))
		case 1:
			if len(m.stmts) > 1 {
				m.stmts = slices.Delete(m.stmts, i, i+1)
			}
		default:
			m.stmts[i] = b.newStmt(m)
		}
	}
	if b.intn(8) == 0 {
		at := b.intn(len(b.modules))
		b.modules = slices.Insert(b.modules, at, b.newModule())
	}
	if version%10 == 0 {
		b.pool[b.intn(len(b.pool))] = b.newIdent() + "_" + strconv.Itoa(version)
	}
}
