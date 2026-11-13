package textindex

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

// FuzzQueryParse: Parse is total; errors carry a known code and an
// in-bounds span; accepted queries print to a canonical form that parses
// back to the same AST (TIDX-INV-12, -13).
func FuzzQueryParse(f *testing.F) {
	for _, q := range parseInputs {
		f.Add(q)
	}
	f.Fuzz(func(t *testing.T, q string) {
		a, err := Parse(q)
		checkQueryError(t, q, err)
		if err != nil {
			return
		}
		s := a.String()
		b, err := Parse(s)
		if err != nil {
			t.Fatalf("print(parse(%q)) = %q: %v", q, s, err)
		}
		if a.JSON(false) != b.JSON(false) || b.String() != s {
			t.Fatalf("%q: AST changed through printing (%q)", q, s)
		}
		for _, n := range a.Nodes {
			if n.Span.Start < 0 || n.Span.Start > n.Span.End || n.Span.End > len(q) {
				t.Fatalf("%q: node span %v outside query", q, n.Span)
			}
		}
	})
}

// reseal recomputes every section CRC and the footer CRC of a segment so
// the fuzzer reaches the structural validation behind them.
func reseal(b []byte) []byte {
	if len(b) < segHeaderSize+4+numSections*dirEntrySize+segFooterSize {
		return b
	}
	out := append([]byte(nil), b...)
	dirOff := len(out) - segFooterSize - 4 - numSections*dirEntrySize
	binary.LittleEndian.PutUint64(out[len(out)-16:], uint64(dirOff))
	for i := 0; i < numSections; i++ {
		e := out[dirOff+4+i*dirEntrySize:]
		off, n := binary.LittleEndian.Uint64(e[4:]), binary.LittleEndian.Uint64(e[12:])
		if off <= uint64(dirOff) && n <= uint64(dirOff)-off {
			binary.LittleEndian.PutUint32(e[20:], crc32c(out[off:off+n]))
		}
	}
	binary.LittleEndian.PutUint32(out[len(out)-8:], crc32c(out[:len(out)-8]))
	return out
}

// FuzzSegmentOpen: OpenSegment never panics or reads out of bounds; an
// accepted segment can be fully decoded and searched.
func FuzzSegmentOpen(f *testing.F) {
	r := core.NewSplitMix64(9)
	s := testSchema()
	for _, n := range []int{0, 1, 3, 8} {
		b, err := EncodeSegment(s, randomDocs(r, n))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
	}
	c, err := newCodec()
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, b := range [][]byte{data, reseal(data)} {
			sg, err := openSegment(b, s, c)
			if err != nil {
				if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrChecksum) && !errors.Is(err, ErrSchemaMismatch) {
					t.Fatalf("unexpected error class: %v", err)
				}
				continue
			}
			if _, err := sg.Documents(); err != nil {
				t.Fatalf("accepted segment does not decode: %v", err)
			}
			v := newSegView("fuzz", sg, nil, 0)
			for _, q := range []string{"user", `"get user"`, "/user|key/", "kind:endpoint_added -side:added", "since:5d"} {
				pq, _ := Parse(q)
				p, err := lowerQuery(pq, s, "at", genNow)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := execute([]*segView{v}, p, len(s.Text), 0, 10, 100); err != nil {
					var qe *QueryError
					if !errors.As(err, &qe) {
						t.Fatalf("%q on accepted segment: %v", q, err)
					}
				}
			}
		}
	})
}

// FuzzAnalyzer: the analyzer is total and its output satisfies
// TIDX-INV-10; trigrams are sorted and distinct.
func FuzzAnalyzer(f *testing.F) {
	for _, in := range analyzerInputs {
		f.Add([]byte(in))
	}
	f.Fuzz(func(t *testing.T, text []byte) {
		checkTokens(t, text)
		tr := Trigrams(text)
		for i := 1; i < len(tr); i++ {
			if tr[i] <= tr[i-1] {
				t.Fatal("trigrams not sorted and distinct")
			}
		}
		if len(text) >= 3 && len(tr) == 0 {
			t.Fatal("no trigrams for a text of 3+ bytes")
		}
	})
}

// FuzzIndexOps: a byte string is decoded into an operation sequence
// (add, update, delete, refresh, merge, force-merge, reopen, search) and
// run against the index and the brute-force reference (TIDX-INV-02).
func FuzzIndexOps(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11})
	f.Add([]byte("add add refresh delete refresh merge search"))
	f.Add([]byte{0, 0, 0, 0, 12, 12, 1, 13, 14, 15, 16, 17, 18, 19, 3, 3})
	f.Fuzz(func(t *testing.T, ops []byte) {
		if len(ops) > 200 {
			ops = ops[:200]
		}
		s := testSchema()
		dir := NewMemDirectory()
		cfg := Config{Schema: s, Dir: dir, MergeFactor: 2, MaxRegexCandidates: 8}
		ix, err := Open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ix.Close() }()
		ref := newRef(s, 8)
		r := core.NewSplitMix64(uint64(len(ops)))
		for i, op := range ops {
			id := uint64(op>>4) % 12
			switch op % 10 {
			case 0, 1, 2:
				d := genDoc(r, id)
				gerr, werr := ix.Add(d), ref.add(d, false)
				if (gerr == nil) != (werr == nil) {
					t.Fatalf("op %d add: %v vs %v", i, gerr, werr)
				}
			case 3:
				d := genDoc(r, id)
				_ = ix.Update(d)
				_ = ref.add(d, true)
			case 4:
				_ = ix.Delete(id)
				ref.delete(id)
			case 5:
				mustRefresh(t, ix, ref)
			case 6:
				if err := ix.Merge(); err != nil {
					t.Fatal(err)
				}
			case 7:
				if err := ix.ForceMerge(1 + int(id%3)); err != nil {
					t.Fatal(err)
				}
			case 8:
				_ = ix.Close()
				if ix, err = Open(cfg); err != nil {
					t.Fatal(err)
				}
				ref.current = cloneMap(ref.committed)
			default:
				checkQuery(t, ix, ref, genQueryTop(r), SearchOptions{K: 1 + int(id), Now: genNow})
			}
		}
		mustRefresh(t, ix, ref)
		checkQueries(t, ix, ref, r, 5)
	})
}
