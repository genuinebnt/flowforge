package textindex

import (
	"errors"
	"math"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// iters scales a property test's outer loop from testkit.Cases().
func iters(div, minimum int) int { return max(minimum, testkit.Cases()/div) }

func mustOpen(t testing.TB, cfg Config) *Index {
	t.Helper()
	if cfg.Schema.Text == nil {
		cfg.Schema = testSchema()
	}
	ix, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return ix
}

// buildPair fills an index and the reference with n random docs,
// refreshing at random points so the index has several segments.
func buildPair(t testing.TB, r core.Rng, n int, cfg Config) (*Index, *refIndex) {
	t.Helper()
	ix := mustOpen(t, cfg)
	ref := newRef(testSchema(), ix.cfg.MaxRegexCandidates)
	for i := 0; i < n; i++ {
		d := genDoc(r, uint64(core.IntN(r, 4*n+1)))
		gerr := ix.Add(d)
		werr := ref.add(d, false)
		if (gerr == nil) != (werr == nil) {
			t.Fatalf("add %d: index %v, reference %v", d.ID, gerr, werr)
		}
		if core.IntN(r, 25) == 0 {
			mustRefresh(t, ix, ref)
		}
	}
	mustRefresh(t, ix, ref)
	return ix, ref
}

func mustRefresh(t testing.TB, ix *Index, ref *refIndex) {
	t.Helper()
	if err := ix.Refresh(); err != nil {
		t.Fatal(err)
	}
	ref.refresh()
}

func checkQueries(t testing.TB, ix *Index, ref *refIndex, r core.Rng, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		q := genQueryTop(r)
		opts := SearchOptions{K: 1 + core.IntN(r, 30), Now: genNow}
		checkQuery(t, ix, ref, q, opts)
	}
}

func checkQuery(t testing.TB, ix *Index, ref *refIndex, q string, opts SearchOptions) {
	t.Helper()
	got, gerr := ix.Search(q, opts)
	want, werr := ref.search(q, opts)
	if ok, why := sameOutcome(got, gerr, want, werr); !ok {
		t.Fatalf("query %q (k=%d): %s\n got  %+v err %v\n want %+v err %v", q, opts.K, why, got, gerr, want, werr)
	}
}

// TIDX-INV-01: search results (hits, scores, order, totals, error codes)
// equal the brute-force reference for random corpora and queries.
func TestTIdxInv01_SearchMatchesBruteForce(t *testing.T) {
	r := testkit.Rng(t)
	for c := 0; c < iters(100, 4); c++ {
		n := 50 + core.IntN(r, 250)
		if c%2 == 1 {
			n = 600 + core.IntN(r, 900) // long postings: skip blocks of 128
		}
		ix, ref := buildPair(t, r, n, Config{MergeFactor: 2 + core.IntN(r, 4), DisableAutoMerge: core.IntN(r, 2) == 0})
		checkQueries(t, ix, ref, r, 150)
	}
}

// TIDX-INV-02: after any interleaving of add, update, delete, refresh,
// merge, force-merge and reopen, searches and Get equal the reference.
func TestTIdxInv02_OpsMatchBruteForce(t *testing.T) {
	r := testkit.Rng(t)
	for c := 0; c < iters(100, 4); c++ {
		dir := NewMemDirectory()
		cfg := Config{Schema: testSchema(), Dir: dir, MergeFactor: 2 + core.IntN(r, 3), MaxRegexCandidates: 40}
		ix := mustOpen(t, cfg)
		ref := newRef(testSchema(), 40)
		runOps(t, r, ix, ref, cfg, 300, func(ix2 *Index) { ix = ix2 })
	}
}

// runOps applies n random operations to ix and ref, checking after every
// refresh-like operation.
func runOps(t testing.TB, r core.Rng, ix *Index, ref *refIndex, cfg Config, n int, setIx func(*Index)) {
	t.Helper()
	maxID := 60
	for i := 0; i < n; i++ {
		switch op := core.IntN(r, 20); {
		case op < 8:
			d := genDoc(r, uint64(core.IntN(r, maxID)))
			gerr, werr := ix.Add(d), ref.add(d, false)
			if (gerr == nil) != (werr == nil) {
				t.Fatalf("op %d add %d: index %v, ref %v", i, d.ID, gerr, werr)
			}
		case op < 11:
			d := genDoc(r, uint64(core.IntN(r, maxID)))
			if err := ix.Update(d); err != nil {
				t.Fatal(err)
			}
			_ = ref.add(d, true)
		case op < 14:
			id := uint64(core.IntN(r, maxID))
			if err := ix.Delete(id); err != nil {
				t.Fatal(err)
			}
			ref.delete(id)
		case op < 17:
			mustRefresh(t, ix, ref)
		case op == 17:
			if err := ix.Merge(); err != nil {
				t.Fatal(err)
			}
		case op == 18:
			if err := ix.ForceMerge(1 + core.IntN(r, 3)); err != nil {
				t.Fatal(err)
			}
		default:
			if err := ix.Close(); err != nil {
				t.Fatal(err)
			}
			ix2, err := Open(cfg)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			t.Cleanup(func() { _ = ix2.Close() })
			ix = ix2
			setIx(ix2)
			ref.current = cloneMap(ref.committed)
		}
		if core.IntN(r, 4) == 0 {
			checkQueries(t, ix, ref, r, 3)
			id := uint64(core.IntN(r, maxID))
			got, ok, err := ix.Get(id)
			want, wok := ref.committed[id]
			if err != nil || ok != wok || (ok && docSummary(got) != docSummary(want.document(ref.schema))) {
				t.Fatalf("Get(%d) = %v %v %v, want %v %v", id, got, ok, err, want.document(ref.schema), wok)
			}
		}
	}
}

func cloneMap(m map[uint64]docFields) map[uint64]docFields {
	out := make(map[uint64]docFields, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// genRegex builds random RE2 patterns over a small alphabet.
func genRegex(r core.Rng, depth int) string {
	if depth <= 0 || core.IntN(r, 3) == 0 {
		switch core.IntN(r, 9) {
		case 0:
			return pick(r, []string{".", `\w`, `\d`, `\s`, "[a-c]", "[^x]", "(?i:k)", "K"})
		case 1:
			return pick(r, []string{"^", "$", `\b`})
		default:
			return pick(r, []string{"ab", "abc", "bcd", "xyz", "k", "K", "é", "É", "ab_c", "a/b", "ẞ", "ß", "�", "abcd"})
		}
	}
	a, b := genRegex(r, depth-1), genRegex(r, depth-1)
	switch core.IntN(r, 7) {
	case 0:
		return a + "|" + b
	case 1:
		return "(" + a + ")*"
	case 2:
		return "(" + a + ")+"
	case 3:
		return "(" + a + ")?"
	case 4:
		return "(?i:" + a + ")" + b
	case 5:
		return "(" + a + "){2}"
	}
	return a + b
}

func genRegexText(r core.Rng) string {
	alpha := []string{"a", "b", "c", "d", "x", "y", "z", "k", "K", "K", "é", "É", "_", "/", " ", "\n", "ß", "ẞ", "\xff", "�", "abc", "bcd", "xyz"}
	var b strings.Builder
	for n := core.IntN(r, 30); n > 0; n-- {
		b.WriteString(pick(r, alpha))
	}
	return b.String()
}

// TIDX-INV-03: the regex trigram prefilter never drops a true match: if
// the regexp matches a text, the text's trigrams satisfy the trigram
// query; regex searches equal a brute-force regexp scan.
func TestTIdxInv03_RegexPrefilterNeverDrops(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases()*10; i++ {
		pat := genRegex(r, 4)
		flags := pick(r, []string{"", "i", "s", "im"})
		re, tq, err := CompileRegex(pat, flags)
		if err != nil {
			continue
		}
		for j := 0; j < 10; j++ {
			text := genRegexText(r)
			if re.MatchString(text) && !tq.MatchTrigrams(Trigrams([]byte(text))) {
				t.Fatalf("regex /%s/%s matches %q but trigram query %s rejects it", pat, flags, text, tq)
			}
		}
	}
	// Through the index, on the generated corpus.
	ix, ref := buildPair(t, r, 300, Config{MergeFactor: 3})
	for _, pat := range genRegexes {
		for _, fl := range []string{"", "i"} {
			re := regexp.MustCompile(regexSource(pat, fl))
			want := 0
			for _, d := range ref.committed {
				for _, v := range d.text {
					if v != "" && re.MatchString(v) {
						want++
						break
					}
				}
			}
			q := "/" + strings.ReplaceAll(pat, "/", `\/`) + "/" + fl
			res, err := ix.Search(q, SearchOptions{K: 1})
			var qe *QueryError
			if errors.As(err, &qe) && qe.Code == CodeRegexNoTrigrams {
				continue
			}
			if err != nil {
				t.Fatalf("%s: %v", q, err)
			}
			if res.Total != want {
				t.Fatalf("/%s/%s: index total %d, brute-force regexp %d", pat, fl, res.Total, want)
			}
		}
	}
}

// TIDX-INV-04: BM25 scores equal a straightforward textbook computation
// (Robertson's tf saturation, Lucene's idf, statistics over live docs).
func TestTIdxInv04_BM25MatchesTextbook(t *testing.T) {
	r := testkit.Rng(t)
	ix, ref := buildPair(t, r, 400, Config{MergeFactor: 3})
	// Delete some docs so live statistics differ from segment statistics.
	for id := range ref.committed {
		if id%3 == 0 {
			_ = ix.Delete(id)
			ref.delete(id)
		}
	}
	mustRefresh(t, ix, ref)
	type tdoc struct{ tf, dl int }
	for _, term := range []string{"user", "token", "json", "server", "get", "admin", "secret"} {
		n, df, sum := 0, 0, 0
		per := map[uint64]tdoc{}
		for id, d := range ref.committed {
			n++
			toks := Analyze([]byte(d.text[0]))
			dl := 0
			tf := 0
			for _, tk := range toks {
				dl = max(dl, tk.Pos+1)
				if tk.Term == term {
					tf++
				}
			}
			dl = FieldLength([]byte(d.text[0]))
			sum += dl
			if tf > 0 {
				df++
				per[id] = tdoc{tf, dl}
			}
		}
		avg := float64(sum) / float64(n)
		idf := math.Log((float64(n)-float64(df)+0.5)/(float64(df)+0.5) + 1)
		res, err := ix.Search("text:"+term, SearchOptions{K: 10_000})
		if err != nil {
			t.Fatal(err)
		}
		if res.Total != len(per) {
			t.Fatalf("%s: total %d, want %d", term, res.Total, len(per))
		}
		for _, h := range res.Hits {
			p := per[h.ID]
			tf := float64(p.tf)
			want := idf * (tf * 2.2) / (tf + 1.2*(0.25+0.75*float64(p.dl)/avg))
			if math.Abs(h.Score-want) > 1e-12*want {
				t.Fatalf("%s doc %d: score %.17g, textbook %.17g", term, h.ID, h.Score, want)
			}
		}
		// tf saturation: score increases with tf and stays below idf*(k1+1).
		for tf := 1; tf < 50; tf++ {
			a, b := BM25(idf, tf, 10, avg), BM25(idf, tf+1, 10, avg)
			if !(b > a) || b >= idf*(BM25K1+1) {
				t.Fatalf("BM25 not monotone/saturating at tf=%d", tf)
			}
		}
	}
}

// TIDX-INV-05: merging (policy and forced) changes no result, and a
// forced merge drops every tombstoned document.
func TestTIdxInv05_MergePreservesResults(t *testing.T) {
	r := testkit.Rng(t)
	for c := 0; c < iters(250, 2); c++ {
		ix, ref := buildPair(t, r, 300, Config{MergeFactor: 50, DisableAutoMerge: true})
		for id := range ref.committed {
			if core.IntN(r, 4) == 0 {
				_ = ix.Delete(id)
				ref.delete(id)
			}
		}
		mustRefresh(t, ix, ref)
		var qs []string
		var before []Result
		for i := 0; i < 150; i++ {
			q := genQueryTop(r)
			res, err := ix.Search(q, SearchOptions{K: 25, Now: genNow})
			if err != nil {
				continue
			}
			qs = append(qs, q)
			before = append(before, res)
		}
		for _, step := range []func() error{ix.Merge, func() error { return ix.ForceMerge(2) }, func() error { return ix.ForceMerge(1) }} {
			if err := step(); err != nil {
				t.Fatal(err)
			}
			for i, q := range qs {
				res, err := ix.Search(q, SearchOptions{K: 25, Now: genNow})
				if ok, why := sameOutcome(res, err, before[i], nil); !ok {
					t.Fatalf("%q changed after merge: %s", q, why)
				}
			}
		}
		if st := ix.Stats(); st.Segments != 1 || st.Deleted != 0 || st.Live != len(ref.committed) {
			t.Fatalf("after ForceMerge(1): %+v, want 1 segment, 0 deleted, %d live", st, len(ref.committed))
		}
	}
}

func randomDocs(r core.Rng, n int) []IndexedDoc {
	seen := map[uint64]bool{}
	var out []IndexedDoc
	for len(out) < n {
		id := r.Uint64() >> core.IntN(r, 64)
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, IndexedDoc{Doc: genDoc(r, id), Gen: uint64(1 + core.IntN(r, 1000))})
	}
	return out
}

// TIDX-INV-06: encode → open → decode is the identity on normalized
// documents with their generations.
func TestTIdxInv06_SegmentRoundTrip(t *testing.T) {
	r := testkit.Rng(t)
	s := testSchema()
	for c := 0; c < iters(20, 10); c++ {
		docs := randomDocs(r, core.IntN(r, 300))
		data, err := EncodeSegment(s, docs)
		if err != nil {
			t.Fatal(err)
		}
		sg, err := OpenSegment(data, s)
		if err != nil {
			t.Fatal(err)
		}
		got, err := sg.Documents()
		if err != nil {
			t.Fatal(err)
		}
		want := slices.Clone(docs)
		for i := range want {
			want[i].Doc, _ = Normalize(s, want[i].Doc)
		}
		slices.SortFunc(want, func(a, b IndexedDoc) int {
			if a.Doc.ID < b.Doc.ID {
				return -1
			}
			return 1
		})
		if len(got) != len(want) {
			t.Fatalf("%d docs back, want %d", len(got), len(want))
		}
		for i := range got {
			if docSummary(got[i].Doc) != docSummary(want[i].Doc) || got[i].Gen != want[i].Gen {
				t.Fatalf("doc %d: got %+v want %+v", i, got[i], want[i])
			}
		}
		// Every indexed term is found by lookup with the right df.
		for i := range want {
			for f, fv := range want[i].Doc.Text {
				_ = f
				fid := slices.Index(s.Text, fv.Name)
				for _, tk := range Analyze([]byte(fv.Value)) {
					if _, _, _, ok := sg.lookupTerm(fid, tk.Term, nil); !ok {
						t.Fatalf("term %q of field %s not in dictionary", tk.Term, fv.Name)
					}
				}
			}
		}
	}
}

// flipAll asserts that every single-byte change of data makes open fail
// with ErrChecksum or ErrCorrupt.
func flipAll(t *testing.T, what string, data []byte, open func([]byte) error) {
	t.Helper()
	if err := open(data); err != nil {
		t.Fatalf("%s: pristine open: %v", what, err)
	}
	b := slices.Clone(data)
	for i := range b {
		for _, x := range []byte{0x01, 0x80, 0xFF} {
			b[i] ^= x
			err := open(b)
			if !errors.Is(err, ErrChecksum) && !errors.Is(err, ErrCorrupt) {
				t.Fatalf("%s: flipping byte %d (^%#x) gave %v", what, i, x, err)
			}
			b[i] ^= x
		}
	}
	for _, n := range []int{0, 1, len(data) / 2, len(data) - 1} {
		if err := open(data[:n]); err == nil {
			t.Fatalf("%s: truncation to %d bytes accepted", what, n)
		}
	}
}

// TIDX-INV-07: any single-byte corruption (and any truncation) of a
// segment, tombstone file or commit point is detected.
func TestTIdxInv07_CorruptionDetected(t *testing.T) {
	r := testkit.Rng(t)
	s := testSchema()
	data, err := EncodeSegment(s, randomDocs(r, 3))
	if err != nil {
		t.Fatal(err)
	}
	c, _ := newCodec()
	defer c.close()
	flipAll(t, "segment", data, func(b []byte) error { _, err := openSegment(b, s, c); return err })

	dir := NewMemDirectory()
	ix := mustOpen(t, Config{Dir: dir})
	for _, d := range randomDocs(r, 5) {
		_ = ix.Add(d.Doc)
	}
	if err := ix.Refresh(); err != nil {
		t.Fatal(err)
	}
	_ = ix.Delete(mustFirstID(t, ix))
	if err := ix.Refresh(); err != nil {
		t.Fatal(err)
	}
	names, _ := dir.List()
	if len(names) != 3 {
		t.Fatalf("expected segment, tombstone file and commit point, got %v", names)
	}
	for _, n := range names {
		if n == CommitFile || strings.HasSuffix(n, ".fftd") {
			orig, _ := dir.ReadFile(n)
			flipAll(t, n, orig, func(b []byte) error {
				_ = dir.WriteFile(n, b)
				ix2, err := Open(Config{Schema: s, Dir: dir})
				if err == nil {
					ix2.Close()
				}
				_ = dir.WriteFile(n, orig)
				return err
			})
		}
	}
}

func mustFirstID(t *testing.T, ix *Index) uint64 {
	for _, v := range ix.views() {
		if v.seg.n > 0 {
			return v.seg.ids[0]
		}
	}
	t.Fatal("empty index")
	return 0
}

// TIDX-INV-08: segment bytes are deterministic: independent of input
// order, and a merge of any split of a document set produces the same
// bytes as encoding the set directly.
func TestTIdxInv08_SegmentDeterminism(t *testing.T) {
	r := testkit.Rng(t)
	s := testSchema()
	for c := 0; c < iters(50, 5); c++ {
		docs := randomDocs(r, 1+core.IntN(r, 200))
		a, err := EncodeSegment(s, docs)
		if err != nil {
			t.Fatal(err)
		}
		sh := slices.Clone(docs)
		core.Shuffle(r, len(sh), func(i, j int) { sh[i], sh[j] = sh[j], sh[i] })
		b, _ := EncodeSegment(s, sh)
		if !slices.Equal(a, b) {
			t.Fatal("segment bytes depend on input order")
		}
		// Index path: add in random batches, force-merge, compare.
		ix := mustOpen(t, Config{DisableAutoMerge: true})
		var gens []IndexedDoc
		for i, d := range sh {
			_ = ix.Add(d.Doc)
			gens = append(gens, IndexedDoc{Doc: d.Doc, Gen: uint64(i + 1)})
			if core.IntN(r, 20) == 0 {
				_ = ix.Refresh()
			}
		}
		_ = ix.Refresh()
		if err := ix.ForceMerge(1); err != nil {
			t.Fatal(err)
		}
		direct, _ := EncodeSegment(s, gens)
		if got := ix.views()[0].seg.data; !slices.Equal(got, direct) {
			t.Fatal("merged segment differs from the directly encoded one")
		}
	}
}

// TIDX-INV-09: every emitted compound and word token of a document is
// retrievable by searching for it.
func TestTIdxInv09_CompoundTokensRetrievable(t *testing.T) {
	r := testkit.Rng(t)
	ix, ref := buildPair(t, r, 150, Config{MergeFactor: 4})
	for id, d := range ref.committed {
		for f, text := range d.text {
			for _, tk := range Analyze([]byte(text)) {
				if tk.Kind == TokenPart || core.IntN(r, 3) != 0 {
					continue
				}
				q := ref.schema.Text[f] + ":" + tk.Term
				res, err := ix.Search(q, SearchOptions{K: 10_000})
				if err != nil {
					t.Fatalf("%q: %v", q, err)
				}
				if !slices.ContainsFunc(res.Hits, func(h Hit) bool { return h.ID == id }) {
					t.Fatalf("%q does not find doc %d (%q)", q, id, text)
				}
			}
		}
	}
}

// checkTokens asserts the analyzer's structural invariants on one input.
func checkTokens(t testing.TB, text []byte) {
	t.Helper()
	toks := Analyze(text)
	n := FieldLength(text)
	prevPos := -1
	lastPart := -1
	for i, tk := range toks {
		if tk.Start < 0 || tk.End > len(text) || tk.Start >= tk.End {
			t.Fatalf("%q: token %d span [%d,%d) out of bounds", text, i, tk.Start, tk.End)
		}
		if !utf8.Valid(text[tk.Start:tk.End]) {
			t.Fatalf("%q: token %d span splits a rune", text, i)
		}
		if tk.Pos < prevPos || tk.Pos >= n {
			t.Fatalf("%q: token %d position %d not monotonic / beyond length %d", text, i, tk.Pos, n)
		}
		if got := string(appendFold(nil, text[tk.Start:tk.End])); got != tk.Term {
			t.Fatalf("%q: token %d term %q is not fold(span)=%q", text, i, tk.Term, got)
		}
		if tk.End-tk.Start > MaxTokenBytes {
			t.Fatalf("%q: token %d longer than MaxTokenBytes", text, i)
		}
		switch tk.Kind {
		case TokenPart:
			if tk.Pos <= lastPart {
				t.Fatalf("%q: parts not strictly increasing", text)
			}
			lastPart = tk.Pos
			for _, c := range string(text[tk.Start:tk.End]) {
				if classOf(c) == classOther || classOf(c) == classConnector {
					t.Fatalf("%q: part %q contains a delimiter", text, tk.Term)
				}
			}
		case TokenCompound:
			first := tk.Start
			for first < tk.End && text[first] == '_' {
				first++
			}
			for j := i + 1; j < len(toks) && toks[j].Start < tk.End; j++ {
				if toks[j].Kind == TokenPart && toks[j].Start == first && toks[j].Pos != tk.Pos {
					t.Fatalf("%q: compound at %d but its first part at %d", text, tk.Pos, toks[j].Pos)
				}
			}
		case TokenWord:
			lastPart = tk.Pos
		}
		prevPos = tk.Pos
	}
}

// TIDX-INV-10: analyzer output is well formed: spans in bounds on rune
// boundaries, Term = fold(span), positions non-decreasing and below
// FieldLength, parts strictly increasing and delimiter-free, a compound at
// its first part's position.
func TestTIdxInv10_AnalyzerTokenInvariants(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases()*5; i++ {
		checkTokens(t, []byte(genText(r, core.IntN(r, 30))))
		checkTokens(t, []byte(genRegexText(r)))
		b := make([]byte, core.IntN(r, 64))
		for j := range b {
			b[j] = byte(r.Uint64())
		}
		checkTokens(t, b)
	}
}

// TIDX-INV-11: matching is case-insensitive: folding is idempotent and
// case-invariant on every rune, and random case variants of a document's
// words find the document.
func TestTIdxInv11_CaseInsensitive(t *testing.T) {
	for c := rune(0); c <= unicode.MaxRune; c++ {
		f := foldRune(c)
		if foldRune(f) != f {
			t.Fatalf("fold not idempotent at %U: %U -> %U", c, f, foldRune(f))
		}
		if foldRune(unicode.ToUpper(c)) != f || foldRune(unicode.ToLower(c)) != f || foldRune(unicode.ToTitle(c)) != f {
			t.Fatalf("fold differs between case variants of %U", c)
		}
	}
	r := testkit.Rng(t)
	ix, ref := buildPair(t, r, 150, Config{MergeFactor: 4})
	for id, d := range ref.committed {
		text := d.text[0]
		for _, tk := range Analyze([]byte(text)) {
			if tk.Kind == TokenPart || core.IntN(r, 3) != 0 {
				continue
			}
			q := "text:" + randCase(r, text[tk.Start:tk.End])
			res, err := ix.Search(q, SearchOptions{K: 10_000})
			if err != nil {
				t.Fatalf("%q: %v", q, err)
			}
			if !slices.ContainsFunc(res.Hits, func(h Hit) bool { return h.ID == id }) {
				t.Fatalf("%q does not find doc %d (%q)", q, id, text)
			}
		}
	}
}

// TIDX-INV-12: printing is a canonical inverse of parsing:
// parse(print(parse(q))) equals parse(q) up to spans, and print is a
// fixpoint.
func TestTIdxInv12_PrintParseIdentity(t *testing.T) {
	r := testkit.Rng(t)
	parsed := 0
	for i := 0; i < testkit.Cases()*10; i++ {
		q := genQueryTop(r)
		if core.IntN(r, 3) == 0 {
			q = mutateQuery(r, q)
		}
		a, err := Parse(q)
		if err != nil {
			continue
		}
		parsed++
		s := a.String()
		b, err := Parse(s)
		if err != nil {
			t.Fatalf("print(parse(%q)) = %q does not parse: %v", q, s, err)
		}
		if a.JSON(false) != b.JSON(false) {
			t.Fatalf("AST changed through printing:\n q   %q\n s   %q\n a   %s\n b   %s", q, s, a.JSON(false), b.JSON(false))
		}
		if b.String() != s {
			t.Fatalf("print not a fixpoint: %q -> %q", s, b.String())
		}
	}
	if parsed < testkit.Cases() {
		t.Fatalf("only %d generated queries parsed", parsed)
	}
}

var queryNoise = []string{"(", ")", "\"", "/", "-", ":", " ", "OR", "AND", "\\", "x", "i", "since:", "kind:", "zz:", "\x00", "é"}

func mutateQuery(r core.Rng, q string) string {
	b := []byte(q)
	for n := 1 + core.IntN(r, 3); n > 0; n-- {
		p := core.IntN(r, len(b)+1)
		if core.IntN(r, 2) == 0 && p < len(b) {
			b = append(b[:p], b[p+1:]...)
		} else {
			ins := pick(r, queryNoise)
			b = append(b[:p], append([]byte(ins), b[p:]...)...)
		}
	}
	return string(b)
}

var allCodes = []ErrorCode{CodeEmptyQuery, CodeQueryTooLong, CodeInvalidUTF8, CodeUnexpectedToken, CodeUnbalancedParen,
	CodeExpectedTerm, CodeExpectedValue, CodeUnterminatedPhrase, CodeUnterminatedRegex, CodeEmptyRegex, CodeBadRegexFlag,
	CodeBadRegex, CodeBadTime, CodeTooDeep, CodeTooManyClauses, CodeUnknownField, CodeFieldType, CodeRegexNoTrigrams}

func checkQueryError(t testing.TB, q string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	var qe *QueryError
	if !errors.As(err, &qe) || !errors.Is(err, ErrQuery) {
		t.Fatalf("%q: error %v is not a *QueryError wrapping ErrQuery", q, err)
	}
	if qe.Span.Start < 0 || qe.Span.Start > qe.Span.End || qe.Span.End > len(q) {
		t.Fatalf("%q: span [%d,%d) outside the query", q, qe.Span.Start, qe.Span.End)
	}
	if !slices.Contains(allCodes, qe.Code) {
		t.Fatalf("%q: unknown code %s", q, qe.Code)
	}
}

// TIDX-INV-13: every parse, lowering and execution error is a
// *QueryError with a code from the closed list and a span inside the
// query.
func TestTIdxInv13_ErrorSpansInsideQuery(t *testing.T) {
	r := testkit.Rng(t)
	ix := mustOpen(t, Config{})
	codes := map[ErrorCode]bool{}
	for i := 0; i < testkit.Cases()*10; i++ {
		q := mutateQuery(r, genQueryTop(r))
		_, err := Parse(q)
		checkQueryError(t, q, err)
		_, err = ix.Search(q, SearchOptions{})
		checkQueryError(t, q, err)
		var qe *QueryError
		if errors.As(err, &qe) {
			codes[qe.Code] = true
		}
	}
	if len(codes) < 8 {
		t.Fatalf("mutations reached only %d error codes: %v", len(codes), codes)
	}
}

// TIDX-INV-14: writes are invisible until Refresh, and reopening the
// directory (memory or file system) reproduces every result.
func TestTIdxInv14_VisibilityAndDurability(t *testing.T) {
	r := testkit.Rng(t)
	fsdir, err := NewFSDirectory(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []Directory{NewMemDirectory(), fsdir} {
		cfg := Config{Schema: testSchema(), Dir: dir, MergeFactor: 3}
		ix := mustOpen(t, cfg)
		ref := newRef(testSchema(), 10_000)
		for i := 0; i < 200; i++ {
			d := genDoc(r, uint64(i))
			_ = ix.Add(d)
			_ = ref.add(d, false)
			if i%40 == 39 {
				// Before refresh the index still equals the last snapshot.
				checkQueries(t, ix, ref, r, 5)
				mustRefresh(t, ix, ref)
				id := uint64(core.IntN(r, i))
				_ = ix.Delete(id)
				ref.delete(id)
			}
		}
		var qs []string
		var res []Result
		for i := 0; i < 100; i++ {
			q := genQueryTop(r)
			out, err := ix.Search(q, SearchOptions{K: 15, Now: genNow})
			if err == nil {
				qs, res = append(qs, q), append(res, out)
			}
		}
		_ = ix.Close()
		ix2 := mustOpen(t, cfg) // unrefreshed delete is lost, by design
		for i, q := range qs {
			out, err := ix2.Search(q, SearchOptions{K: 15, Now: genNow})
			if ok, why := sameOutcome(out, err, res[i], nil); !ok {
				t.Fatalf("%q differs after reopen: %s", q, why)
			}
		}
	}
}

// TIDX-INV-15: postings advance with skip entries agrees with a linear
// scan, and leapfrog intersection equals set intersection.
func TestTIdxInv15_SkipsAgreeWithScan(t *testing.T) {
	r := testkit.Rng(t)
	s := testSchema()
	var docs []IndexedDoc
	var common, rare []uint32
	for i := 0; i < 3000; i++ {
		text := "filler"
		if core.IntN(r, 3) > 0 {
			text += " common"
			common = append(common, uint32(i))
		}
		if core.IntN(r, 11) == 0 {
			text += " rare"
			rare = append(rare, uint32(i))
		}
		docs = append(docs, IndexedDoc{Doc: Document{ID: uint64(i), Text: []FieldValue{{"text", text}}}})
	}
	data, err := EncodeSegment(s, docs)
	if err != nil {
		t.Fatal(err)
	}
	sg, err := OpenSegment(data, s)
	if err != nil {
		t.Fatal(err)
	}
	for c := 0; c < testkit.Cases(); c++ {
		it := sg.termIter(0, "common", nil)
		target := uint32(0)
		for target < 3100 {
			target += uint32(core.IntN(r, 300))
			got := it.advance(target)
			i, _ := slices.BinarySearch(common, target)
			want := uint32(noMore)
			if i < len(common) {
				want = common[i]
			}
			if got != want {
				t.Fatalf("advance(%d) = %d, want %d", target, got, want)
			}
			if got != noMore && core.IntN(r, 2) == 0 {
				if ps := it.positions(); len(ps) != 1 || ps[0] != 1 {
					t.Fatalf("positions of doc %d = %v", got, ps)
				}
			}
		}
	}
	and := newAndIter([]docIter{sg.termIter(0, "common", nil), sg.termIter(0, "rare", nil), newSliceIter(rare)})
	var got []uint32
	for d := and.nextDoc(); d != noMore; d = and.nextDoc() {
		got = append(got, d)
	}
	var want []uint32
	for _, d := range rare {
		if _, ok := slices.BinarySearch(common, d); ok {
			want = append(want, d)
		}
	}
	if !slices.Equal(got, want) {
		t.Fatalf("intersection has %d docs, want %d", len(got), len(want))
	}
}

// TIDX-INV-16: a regex without usable trigrams is rejected with
// REGEX_NO_TRIGRAMS unless the root's filters select at most
// MaxRegexCandidates documents; then it is answered exactly.
func TestTIdxInv16_RegexWithoutTrigramsRule(t *testing.T) {
	r := testkit.Rng(t)
	ix, ref := buildPair(t, r, 300, Config{MaxRegexCandidates: 60, MergeFactor: 3})
	ref.maxRegex = 60
	count := func(q string) int {
		res, err := ref.search(q, SearchOptions{K: 1, Now: genNow})
		if err != nil {
			t.Fatal(err)
		}
		return res.Total
	}
	for _, q := range []string{`/\w+/`, `/a.b/ OR user`, `-/x/`, `user /.*/`} {
		_, err := ix.Search(q, SearchOptions{})
		var qe *QueryError
		if !errors.As(err, &qe) || qe.Code != CodeRegexNoTrigrams {
			t.Fatalf("%q: got %v, want REGEX_NO_TRIGRAMS", q, err)
		}
	}
	accepted := 0
	for _, f := range []string{"kind:endpoint_added", "kind:script_added side:added", "program:p1", "since:3d", "since:10d program:p2"} {
		q := f + ` /[a-z]_/`
		_, err := ix.Search(q, SearchOptions{Now: genNow})
		n := count(f)
		var qe *QueryError
		if n > 60 {
			if !errors.As(err, &qe) || qe.Code != CodeRegexNoTrigrams {
				t.Fatalf("%q selects %d > 60 docs but got %v", q, n, err)
			}
			continue
		}
		accepted++
		checkQuery(t, ix, ref, q, SearchOptions{K: 50, Now: genNow})
	}
	if accepted == 0 {
		t.Fatal("no filter was selective enough to exercise the accepted path")
	}
}

// TIDX-INV-17: RRF fusion follows vecindex's definition (constant 60,
// 1-based ranks, first occurrence per list, ties by id) on vecindex's own
// conformance vectors.
func TestTIdxInv17_RRFMatchesVecindex(t *testing.T) {
	type rrfCase struct {
		RRFK      int        `json:"rrf_k"`
		Lists     [][]string `json:"lists"`
		Tolerance float64    `json:"tolerance"`
		Expect    []struct {
			ID    string  `json:"id"`
			Score float64 `json:"score"`
		} `json:"expect"`
	}
	toID := func(u string) uint64 {
		u = strings.ReplaceAll(u, "-", "")
		var v uint64
		for _, c := range u[16:] {
			v = v<<4 | uint64(strings.IndexRune(hexDigits, c))
		}
		if strings.Trim(u[:16], "0") != "" {
			t.Fatalf("uuid %s does not fit 64 bits", u)
		}
		return v
	}
	for _, cf := range testkit.LoadCases[rrfCase](t, "vecindex/rrf") {
		c := cf.Data
		var lists [][]uint64
		for _, l := range c.Lists {
			var ids []uint64
			for _, s := range l {
				ids = append(ids, toID(s))
			}
			lists = append(lists, ids)
		}
		got := RRF(c.RRFK, lists...)
		if len(got) != len(c.Expect) {
			t.Fatalf("%s: %d fused, want %d", cf.Name, len(got), len(c.Expect))
		}
		for i, e := range c.Expect {
			if got[i].ID != toID(e.ID) || math.Abs(got[i].Score-e.Score) > c.Tolerance {
				t.Fatalf("%s: rank %d = %+v, want %s %.5f", cf.Name, i, got[i], e.ID, e.Score)
			}
		}
	}
	h := Hybrid([]Hit{{ID: 7}, {ID: 3}}, []uint64{3, 9}, 0, 2)
	if len(h) != 2 || h[0].ID != 3 || h[0].Ranks[0] != 2 || h[0].Ranks[1] != 1 {
		t.Fatalf("Hybrid = %+v", h)
	}
}
