package textindex

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// Conformance data in conformance/textindex/{analyzer,query_parse,
// trigram_query,search}. Set FF_UPDATE_CONFORMANCE=1 to regenerate the
// files: analyzer, parse and trigram vectors from this implementation
// (review the diff), search expectations from the brute-force reference.

type tokenJSON struct {
	Term  string `json:"term"`
	Pos   int    `json:"pos"`
	Start int    `json:"start"`
	End   int    `json:"end"`
	Kind  string `json:"kind"`
}

type analyzerCase struct {
	Input       string      `json:"input,omitempty"`
	InputHex    string      `json:"input_hex,omitempty"`
	FieldLength int         `json:"field_length"`
	Trigrams    int         `json:"distinct_trigrams"`
	Tokens      []tokenJSON `json:"tokens"`
}

type analyzerFile struct {
	Description string         `json:"description"`
	Cases       []analyzerCase `json:"cases"`
}

type errJSON struct {
	Code string `json:"code"`
	Span [2]int `json:"span"`
}

type parseCase struct {
	Query string          `json:"query"`
	AST   json.RawMessage `json:"ast,omitempty"`
	Print string          `json:"print,omitempty"`
	Error *errJSON        `json:"error,omitempty"`
}

type parseFile struct {
	Description string      `json:"description"`
	Cases       []parseCase `json:"cases"`
}

type trigramCase struct {
	Regex string `json:"regex"`
	Flags string `json:"flags,omitempty"`
	Query string `json:"query"`
}

type trigramFile struct {
	Description string        `json:"description"`
	Cases       []trigramCase `json:"cases"`
}

type docJSON struct {
	ID      uint64            `json:"id"`
	Text    map[string]string `json:"text,omitempty"`
	Keyword map[string]string `json:"keyword,omitempty"`
	Numeric map[string]int64  `json:"numeric,omitempty"`
	Stored  map[string]string `json:"stored,omitempty"`
}

type hitJSON struct {
	ID    uint64  `json:"id"`
	Score float64 `json:"score"`
	At    *int64  `json:"at,omitempty"`
}

type searchQuery struct {
	Query string    `json:"query"`
	K     int       `json:"k"`
	Total int       `json:"total"`
	Hits  []hitJSON `json:"hits"`
	Error string    `json:"error,omitempty"`
}

type searchFile struct {
	Description        string        `json:"description"`
	Now                int64         `json:"now"`
	MaxRegexCandidates int           `json:"max_regex_candidates"`
	Tolerance          float64       `json:"tolerance"`
	Docs               []docJSON     `json:"docs"`
	Queries            []searchQuery `json:"queries"`
}

func (d docJSON) document() Document {
	out := Document{ID: d.ID}
	add := func(m map[string]string, dst *[]FieldValue) {
		for _, k := range slices.Sorted(mapKeys(m)) {
			*dst = append(*dst, FieldValue{k, m[k]})
		}
	}
	add(d.Text, &out.Text)
	add(d.Keyword, &out.Keyword)
	add(d.Stored, &out.Stored)
	for _, k := range slices.Sorted(mapKeys(d.Numeric)) {
		out.Numeric = append(out.Numeric, NumericValue{k, d.Numeric[k]})
	}
	return out
}

func mapKeys[V any](m map[string]V) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func toDocJSON(d Document) docJSON {
	j := docJSON{ID: d.ID}
	for _, f := range d.Text {
		if j.Text == nil {
			j.Text = map[string]string{}
		}
		j.Text[f.Name] = f.Value
	}
	for _, f := range d.Keyword {
		if j.Keyword == nil {
			j.Keyword = map[string]string{}
		}
		j.Keyword[f.Name] = f.Value
	}
	for _, f := range d.Stored {
		if j.Stored == nil {
			j.Stored = map[string]string{}
		}
		j.Stored[f.Name] = f.Value
	}
	for _, f := range d.Numeric {
		if j.Numeric == nil {
			j.Numeric = map[string]int64{}
		}
		j.Numeric[f.Name] = f.Value
	}
	return j
}

func updating() bool { return os.Getenv("FF_UPDATE_CONFORMANCE") == "1" }

func writeJSON(t *testing.T, suite, name string, v any) {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	dir := testkit.ConformanceDir(t, suite)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

var analyzerInputs = []string{
	"getUserById", "user_id", "HTTPServer", "XMLHttpRequest", "parseHTTP2Response", "v2", "__init__",
	"https://api.example.com/v2/users/{id}?token=abc123", "fetch('/api/graphql', {method: 'POST'})",
	"snake_case_ID_42x", "MAX_VALUE", "iOS", "ABC", "a", "", "   ", "!!!", "123abc456", "_", "foo__bar",
	"Straße STRASSE", "ΣΊΣΥΦΟΣ σίσυφος", "K Kelvin k", "naïve café", "éclair", "日本語テキスト",
	"İstanbul ıi", "ǅungla", "x1y2z3", "camelCaseAndPascalCase", "already-hyphen-ated.dotted/path",
	"a_b_c", "_leading", "trailing_", "ID", "Id", "loooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooooong_tail",
}

var analyzerHexInputs = []string{"ff61626380e9", "c3", "6162c0af6364"}

func analyzerCaseFor(b []byte) analyzerCase {
	c := analyzerCase{FieldLength: FieldLength(b), Trigrams: len(Trigrams(b)), Tokens: []tokenJSON{}}
	for _, tk := range Analyze(b) {
		c.Tokens = append(c.Tokens, tokenJSON{tk.Term, tk.Pos, tk.Start, tk.End, tk.Kind.String()})
	}
	return c
}

func TestTIdxConformance_Analyzer(t *testing.T) {
	if updating() {
		f := analyzerFile{Description: "Analyzer vectors: input -> tokens (case-folded term, position, byte span, kind), field length in positions and distinct trigram count. input_hex carries inputs that are not valid UTF-8."}
		for _, in := range analyzerInputs {
			c := analyzerCaseFor([]byte(in))
			c.Input = in
			f.Cases = append(f.Cases, c)
		}
		for _, h := range analyzerHexInputs {
			b, _ := hex.DecodeString(h)
			c := analyzerCaseFor(b)
			c.InputHex = h
			f.Cases = append(f.Cases, c)
		}
		writeJSON(t, "textindex/analyzer", "vectors.json", f)
	}
	for _, cf := range testkit.LoadCases[analyzerFile](t, "textindex/analyzer") {
		for _, c := range cf.Data.Cases {
			in := []byte(c.Input)
			if c.InputHex != "" {
				in, _ = hex.DecodeString(c.InputHex)
			}
			got := analyzerCaseFor(in)
			got.Input, got.InputHex = c.Input, c.InputHex
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(c)
			if !bytes.Equal(gb, wb) {
				t.Errorf("%s: analyzer(%q)\n got  %s\n want %s", cf.Name, in, gb, wb)
			}
		}
	}
}

var parseInputs = []string{
	"foo", "getUserById", "foo bar", "foo AND bar", "foo OR bar", "a b OR c d", "a (b OR c) d",
	"-foo", "-(a OR b)", "-(-a)", `"exact phrase"`, `"esc \" quote \\ slash"`, `/get[A-Z]\w+/`, `/a\/b/i`,
	`/x/ims`, `/x/smi`, `kind:endpoint_added`, `side:added`, `url:"api v2"`, `url:/v\d+/`, `since:7d`,
	`since:2026-11-01`, `until:2026-11-01T10:20:30.5Z`, `since:"12h"`, `(a)`, `((a b))`, `a AND (b c)`,
	`program:p1 kind:endpoint_added -side:removed (token OR secret) since:30d`, `field:-x`, `kind:AND`, `a:b:c`,
	`9:x`, `https://example.com`, `"https://example.com/a"`, `a-b`, `foo(bar)`, `and or`, `AND`, `OR foo`,
	`foo OR`, `foo AND`, `(`, `)`, `(foo`, `foo)`, `()`, `- foo`, `--foo`, `"open`, `/open`, `//`, `/x/q`,
	`/x/ii`, `/(/`, `kind:`, `kind: x`, `since:7y`, `since:2026-02-30`, `since:/x/`, ``, `   `, "a\x00b",
	"\xff", `a /b/ "c" d:e`,
}

func parseCaseFor(q string) parseCase {
	c := parseCase{Query: q}
	pq, err := Parse(q)
	var qe *QueryError
	if errors.As(err, &qe) {
		c.Error = &errJSON{Code: string(qe.Code), Span: [2]int{qe.Span.Start, qe.Span.End}}
		return c
	}
	c.AST = json.RawMessage(pq.JSON(true))
	c.Print = pq.String()
	return c
}

func TestTIdxConformance_QueryParse(t *testing.T) {
	if updating() {
		f := parseFile{Description: "Query parse vectors: query -> AST (canonical JSON with byte spans) and canonical print, or error code and span."}
		for _, q := range parseInputs {
			if !jsonSafe(q) {
				continue
			}
			f.Cases = append(f.Cases, parseCaseFor(q))
		}
		r := core.NewSplitMix64(0x7458)
		for len(f.Cases) < len(parseInputs)+60 {
			q := genQueryTop(r)
			if core.IntN(r, 3) == 0 {
				q = mutateQuery(r, q)
			}
			if jsonSafe(q) && len(q) < 200 {
				f.Cases = append(f.Cases, parseCaseFor(q))
			}
		}
		writeJSON(t, "textindex/query_parse", "vectors.json", f)
	}
	n := 0
	for _, cf := range testkit.LoadCases[parseFile](t, "textindex/query_parse") {
		for _, c := range cf.Data.Cases {
			got := parseCaseFor(c.Query)
			var a, b bytes.Buffer
			_ = json.Compact(&a, got.AST)
			_ = json.Compact(&b, c.AST)
			if !bytes.Equal(a.Bytes(), b.Bytes()) || got.Print != c.Print || !slices.Equal(errFields(got.Error), errFields(c.Error)) {
				t.Errorf("%s: Parse(%q)\n got  %s %q %v\n want %s %q %v", cf.Name, c.Query, a.String(), got.Print, got.Error, b.String(), c.Print, c.Error)
			}
			n++
		}
	}
	if n < 50 {
		t.Fatalf("only %d parse vectors", n)
	}
}

func errFields(e *errJSON) []string {
	if e == nil {
		return nil
	}
	return []string{e.Code, string(rune(e.Span[0])), string(rune(e.Span[1]))}
}

func jsonSafe(s string) bool {
	b, _ := json.Marshal(s)
	var back string
	return json.Unmarshal(b, &back) == nil && back == s
}

var trigramInputs = []struct{ re, flags string }{
	{"abc", ""}, {"abcd", ""}, {"abc|xyz", ""}, {"get[A-Z]\\w+", ""}, {"(?:api|graphql)/v2", ""}, {"fetch.*json", ""},
	{"ab", ""}, {".", ""}, {"a*", ""}, {"[a-z]_", ""}, {"tok(en)?", ""}, {"user", "i"}, {"Kelvin", "i"}, {"straße", "i"},
	{"(abc)+", ""}, {"(abc){2,3}", ""}, {"^https?://[^/]+/admin", ""}, {"secret_?key", "i"}, {"a�b", ""},
	{"[\\x{fff0}-\\x{ffff}]abc", ""}, {"x+y+z+", ""}, {"abc|", ""}, {"(a|b)(c|d)(e|f)", ""}, {"\\bid\\b", ""},
	{"[^a]bcd", ""}, {"(?s)a.b.c", ""}, {"aws_access_key_id\\s*=\\s*[A-Z0-9]{20}", ""},
}

func TestTIdxConformance_TrigramQuery(t *testing.T) {
	if updating() {
		f := trigramFile{Description: "Regex -> canonical trigram query (Cox 2012, codesearch rules, ASCII-folded trigrams; see README 'Regex queries'). A port may emit an equivalent or weaker query; these vectors pin the Go reference."}
		for _, in := range trigramInputs {
			_, tq, err := CompileRegex(in.re, in.flags)
			if err != nil {
				t.Fatal(err)
			}
			f.Cases = append(f.Cases, trigramCase{Regex: in.re, Flags: in.flags, Query: tq.String()})
		}
		writeJSON(t, "textindex/trigram_query", "vectors.json", f)
	}
	for _, cf := range testkit.LoadCases[trigramFile](t, "textindex/trigram_query") {
		for _, c := range cf.Data.Cases {
			_, tq, err := CompileRegex(c.Regex, c.Flags)
			if err != nil {
				t.Fatalf("%s: /%s/%s: %v", cf.Name, c.Regex, c.Flags, err)
			}
			if tq.String() != c.Query {
				t.Errorf("%s: /%s/%s\n got  %s\n want %s", cf.Name, c.Regex, c.Flags, tq, c.Query)
			}
		}
	}
}

var curatedDocs = []Document{
	{ID: 1, Text: []FieldValue{{"text", "+ const user = await getUserById(id);"}, {"url", "https://shop.example/app.js"}},
		Keyword: []FieldValue{{"kind", "endpoint_added"}, {"side", "added"}, {"program", "p1"}}, Numeric: []NumericValue{{"at", 1_790_000_000_000}}},
	{ID: 2, Text: []FieldValue{{"text", "- fetch('/api/v1/users/' + user_id)"}, {"url", "https://shop.example/app.js"}},
		Keyword: []FieldValue{{"kind", "endpoint_removed"}, {"side", "removed"}, {"program", "p1"}}, Numeric: []NumericValue{{"at", 1_789_900_000_000}}},
	{ID: 3, Text: []FieldValue{{"text", "+ fetch('/api/v2/users/' + userId, {headers: {Authorization: token}})"}, {"url", "https://shop.example/app.js"}},
		Keyword: []FieldValue{{"kind", "endpoint_added"}, {"side", "added"}, {"program", "p1"}}, Numeric: []NumericValue{{"at", 1_790_000_000_000}}},
	{ID: 4, Text: []FieldValue{{"text", "Content-Security-Policy: default-src 'self'"}, {"url", "https://shop.example/"}},
		Keyword: []FieldValue{{"kind", "header_changed"}, {"side", "removed"}, {"program", "p2"}}, Numeric: []NumericValue{{"at", 1_789_000_000_000}}},
	{ID: 5, Text: []FieldValue{{"text", "+ const AWS_SECRET_KEY = 'AKIAABCDEFGHIJKLMNOP'"}, {"url", "https://cdn.example/main.8f3a.js"}},
		Keyword: []FieldValue{{"kind", "secret_added"}, {"side", "added"}, {"program", "p2"}}, Numeric: []NumericValue{{"at", 1_790_000_000_000}}},
	{ID: 6, Text: []FieldValue{{"text", "+ XMLHttpRequest.open('POST', '/graphql')"}},
		Keyword: []FieldValue{{"kind", "endpoint_added"}, {"side", "added"}, {"program", "p1"}}},
	{ID: 7, Text: []FieldValue{{"text", "+ const user = await getUserById(id);"}, {"url", "https://shop.example/app.js"}},
		Keyword: []FieldValue{{"kind", "endpoint_added"}, {"side", "added"}, {"program", "p1"}}, Numeric: []NumericValue{{"at", 1_790_000_100_000}}},
}

var curatedQueries = []string{
	"getUserById", "getuserbyid", "GET_USER_BY_ID", "user", "user_id", "userId", `"get user by id"`, `"user by"`,
	"users api", `"api v2"`, "fetch -v1", "fetch AND (v1 OR v2)", "kind:endpoint_added", "kind:endpoint_added side:added user",
	"-kind:endpoint_added", `/get[A-Z]\w+/`, `/AKIA[A-Z0-9]{16}/`, `/akia/i`, `/api\/v\d\//`, `url:/main\.[0-9a-f]+\.js/`,
	"since:1d", "since:2d user", "until:2026-09-01", "since:2026-09-21T00:00:00Z", "program:p2", `/./`, `kind:secret_added /./`,
	"xmlhttprequest", "XML http", `"http request"`, "url:shop", "url:example text:token", "missingfield:x", `kind:/x/`,
	"authorization OR token", "content security policy", `"default src self"`, "graphql",
}

func buildConformanceIndex(t *testing.T, sf searchFile, chunk int) *Index {
	t.Helper()
	ix := mustOpen(t, Config{MaxRegexCandidates: sf.MaxRegexCandidates, DisableAutoMerge: true})
	for i, d := range sf.Docs {
		if err := ix.Add(d.document()); err != nil {
			t.Fatal(err)
		}
		if chunk > 0 && i%chunk == chunk-1 {
			if err := ix.Refresh(); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := ix.Refresh(); err != nil {
		t.Fatal(err)
	}
	return ix
}

func expectSearch(ref *refIndex, q string, k int, now int64) searchQuery {
	sq := searchQuery{Query: q, K: k, Hits: []hitJSON{}}
	res, err := ref.search(q, SearchOptions{K: k, Now: now})
	var qe *QueryError
	if errors.As(err, &qe) {
		sq.Error = string(qe.Code)
		return sq
	}
	sq.Total = res.Total
	for _, h := range res.Hits {
		hj := hitJSON{ID: h.ID, Score: h.Score}
		if h.HasAt {
			at := h.At
			hj.At = &at
		}
		sq.Hits = append(sq.Hits, hj)
	}
	return sq
}

func makeSearchFile(desc string, docs []Document, queries []string, k int) searchFile {
	sf := searchFile{Description: desc, Now: genNow, MaxRegexCandidates: 5, Tolerance: 1e-9}
	ref := newRef(testSchema(), sf.MaxRegexCandidates)
	for _, d := range docs {
		if ref.add(d, false) == nil {
			sf.Docs = append(sf.Docs, toDocJSON(d))
		}
	}
	ref.refresh()
	for _, q := range queries {
		sf.Queries = append(sf.Queries, expectSearch(ref, q, k, sf.Now))
	}
	return sf
}

func TestTIdxConformance_Search(t *testing.T) {
	if updating() {
		writeJSON(t, "textindex/search", "curated.json", makeSearchFile(
			"Hand-written diff hunks (schema DefaultSchema + stored 'note'); expected results from the brute-force reference. Scores are BM25 (k1 1.2, b 0.75, Lucene idf) over live docs; ties by at desc then id.",
			curatedDocs, curatedQueries, 10))
		r := core.NewSplitMix64(0xC0FFEE)
		var docs []Document
		for i := 0; i < 60; i++ {
			docs = append(docs, genDoc(r, uint64(i*3+1)))
		}
		var qs []string
		for len(qs) < 80 {
			q := genQueryTop(r)
			if len(q) < 160 && jsonSafe(q) {
				qs = append(qs, q)
			}
		}
		writeJSON(t, "textindex/search", "generated.json", makeSearchFile(
			"60 generated code/URL documents (seed 0xC0FFEE, gen_test.go generator) and 80 generated queries; expected results from the brute-force reference.",
			docs, qs, 15))
	}
	for _, cf := range testkit.LoadCases[searchFile](t, "textindex/search") {
		sf := cf.Data
		for _, chunk := range []int{0, 7} {
			ix := buildConformanceIndex(t, sf, chunk)
			for _, q := range sf.Queries {
				res, err := ix.Search(q.Query, SearchOptions{K: q.K, Now: sf.Now})
				var qe *QueryError
				if q.Error != "" {
					if !errors.As(err, &qe) || string(qe.Code) != q.Error {
						t.Errorf("%s %q: got %v, want error %s", cf.Name, q.Query, err, q.Error)
					}
					continue
				}
				if err != nil {
					t.Errorf("%s %q: %v", cf.Name, q.Query, err)
					continue
				}
				if res.Total != q.Total || len(res.Hits) != len(q.Hits) {
					t.Errorf("%s %q: total %d / %d hits, want %d / %d", cf.Name, q.Query, res.Total, len(res.Hits), q.Total, len(q.Hits))
					continue
				}
				for i, h := range res.Hits {
					w := q.Hits[i]
					if h.ID != w.ID || math.Abs(h.Score-w.Score) > sf.Tolerance*math.Max(1, math.Abs(w.Score)) || (w.At != nil) != h.HasAt {
						t.Errorf("%s %q: hit %d = %+v, want %+v", cf.Name, q.Query, i, h, w)
					}
				}
			}
		}
	}
}
