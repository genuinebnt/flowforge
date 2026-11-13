package textindex

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/core"
)

// Random corpora and queries for the property tests. A small vocabulary
// keeps term, phrase and regex hits frequent.

var genParts = []string{"get", "user", "id", "by", "http", "server", "api", "v2", "token", "fetch",
	"url", "json", "xml", "auth", "admin", "graphql", "key", "secret", "x1", "42", "é", "straße", "Σigma", "K"}

var genSeps = []string{" ", " ", " ", ".", "/", "(", ")", "_", "-", "\n", ", ", "=", "'", "://", "?", "&"}

const genNow = int64(1_790_000_000_000) // 2026-09-21

func pick[T any](r core.Rng, xs []T) T { return xs[core.IntN(r, len(xs))] }

func randCase(r core.Rng, s string) string {
	var b strings.Builder
	for _, c := range s {
		switch core.IntN(r, 4) {
		case 0:
			b.WriteString(strings.ToUpper(string(c)))
		default:
			b.WriteRune(c)
		}
	}
	return b.String()
}

// genWord builds an identifier from one to three parts in a random style.
func genWord(r core.Rng) string {
	n := 1 + core.IntN(r, 3)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = pick(r, genParts)
	}
	switch core.IntN(r, 5) {
	case 0: // camelCase
		for i := 1; i < n; i++ {
			rs := []rune(parts[i])
			parts[i] = strings.ToUpper(string(rs[0])) + string(rs[1:])
		}
		return strings.Join(parts, "")
	case 1:
		return strings.Join(parts, "_")
	case 2:
		return strings.ToUpper(strings.Join(parts, ""))
	case 3:
		return randCase(r, parts[0])
	}
	return strings.Join(parts, "")
}

func genText(r core.Rng, words int) string {
	var b strings.Builder
	for i := 0; i < words; i++ {
		if i > 0 {
			b.WriteString(pick(r, genSeps))
		}
		b.WriteString(genWord(r))
	}
	return b.String()
}

var genKinds = []string{"endpoint_added", "endpoint_removed", "header_changed", "script_added"}

func genDoc(r core.Rng, id uint64) Document {
	d := Document{ID: id}
	if core.IntN(r, 10) > 0 {
		d.Text = append(d.Text, FieldValue{"text", genText(r, 1+core.IntN(r, 20))})
	}
	if core.IntN(r, 2) == 0 {
		d.Text = append(d.Text, FieldValue{"url", "https://" + genWord(r) + ".example/" + genWord(r) + "/" + genWord(r)})
	}
	if core.IntN(r, 5) > 0 {
		d.Keyword = append(d.Keyword, FieldValue{"kind", pick(r, genKinds)})
	}
	if core.IntN(r, 3) > 0 {
		d.Keyword = append(d.Keyword, FieldValue{"side", pick(r, []string{"added", "removed"})})
	}
	d.Keyword = append(d.Keyword, FieldValue{"program", pick(r, []string{"p1", "p2"})})
	if core.IntN(r, 6) > 0 {
		d.Numeric = append(d.Numeric, NumericValue{"at", genNow - int64(core.IntN(r, 20))*86_400_000 - int64(core.IntN(r, 3))*1000})
	}
	if core.IntN(r, 4) == 0 {
		d.Stored = append(d.Stored, FieldValue{"note", "hunk " + strconv.Itoa(core.IntN(r, 100))})
	}
	return d
}

func testSchema() Schema {
	s := DefaultSchema()
	s.Stored = []string{"note"}
	return s
}

var genRegexes = []string{
	`user`, `get[A-Z]\w+`, `(?:api|graphql)/v2`, `tok(en)?`, `fetch.*json`, `\d\d`, `[xy]ml`,
	`secret|key`, `^get`, `id$`, `straße`, `auth\.admin`, `s.rver`, `[a-z]+_[a-z]+`, `htt(p|ps)://`,
	`.`, `\w`, `a*`, `x1|42`, `σigma`, `\x{212a}`, `ü?ser`, `[^ ]+`, `http.*`, `json|.`,
}

func genLeaf(r core.Rng) string {
	switch core.IntN(r, 14) {
	case 0, 1, 2:
		return genWord(r)
	case 3:
		return randCase(r, pick(r, genParts))
	case 4, 5:
		return `"` + genText(r, 1+core.IntN(r, 3)) + `"`
	case 6, 7:
		flags := pick(r, []string{"", "", "i", "s", "im"})
		return "/" + strings.ReplaceAll(pick(r, genRegexes), "/", `\/`) + "/" + flags
	case 8:
		return "kind:" + pick(r, genKinds)
	case 9:
		return "side:" + pick(r, []string{"added", "removed", "x"})
	case 10:
		return pick(r, []string{"text:", "url:"}) + genWord(r)
	case 11:
		return pick(r, []string{"since:", "until:"}) + strconv.Itoa(core.IntN(r, 25)) + pick(r, []string{"d", "h", "w"})
	case 12:
		return "program:" + pick(r, []string{"p1", "p2"})
	}
	return `url:"` + genText(r, 2) + `"`
}

func genQuery(r core.Rng, depth int) string {
	if depth <= 0 || core.IntN(r, 3) == 0 {
		l := genLeaf(r)
		if core.IntN(r, 6) == 0 {
			return "-" + l
		}
		return l
	}
	n := 2 + core.IntN(r, 2)
	parts := make([]string, n)
	for i := range parts {
		parts[i] = genQuery(r, depth-1)
	}
	var q string
	switch core.IntN(r, 3) {
	case 0:
		q = strings.Join(parts, " ")
	case 1:
		q = strings.Join(parts, " AND ")
	default:
		q = strings.Join(parts, " OR ")
	}
	switch core.IntN(r, 4) {
	case 0:
		return "(" + q + ")"
	case 1:
		return "-(" + q + ")"
	}
	return "(" + q + ")"
}

// genQueryTop sometimes prefixes filters so trigram-less regexes can run.
func genQueryTop(r core.Rng) string {
	q := genQuery(r, 3)
	if core.IntN(r, 4) == 0 {
		q = "kind:" + pick(r, genKinds) + " " + q
	}
	return q
}

func docSummary(d Document) string { return fmt.Sprintf("%+v", d) }
