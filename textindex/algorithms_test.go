package textindex

import (
	"encoding/binary"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// TIDX-INV-18: the pluggable algorithms are interchangeable: every
// posting codec, intersector and regex strategy gives results identical
// to ScanIndex, and with the TF-IDF scorer Index still equals ScanIndex
// configured the same way.
func TestTIdxInv18_AlgorithmsInterchangeable(t *testing.T) {
	r := testkit.Rng(t)
	type combo struct {
		pc PostingCodec
		is Intersector
		rx RegexStrategy
		sc Scorer
	}
	var combos []combo
	for _, pc := range Codecs() {
		for _, is := range []Intersector{SkipIntersector{}, GallopIntersector{}, LinearIntersector{}} {
			for _, rx := range []RegexStrategy{TrigramPrefilter{}, FullScan{}} {
				combos = append(combos, combo{pc, is, rx, BM25Scorer{}})
			}
		}
	}
	combos = append(combos, combo{GroupVarintCodec{}, GallopIntersector{}, TrigramPrefilter{}, TFIDFScorer{}})
	for c := 0; c < iters(500, 1); c++ {
		var docs []Document
		for i := 0; i < 400; i++ {
			docs = append(docs, genDoc(r, uint64(i)))
		}
		var qs []string
		for i := 0; i < 60; i++ {
			qs = append(qs, genQueryTop(r))
		}
		for _, cb := range combos {
			name := fmt.Sprintf("%s/%s/%s/%s", cb.pc.Name(), cb.is.Name(), cb.rx.Name(), cb.sc.Name())
			cfg := Config{Schema: testSchema(), Codec: cb.pc, Intersector: cb.is, Regex: cb.rx, Scorer: cb.sc, MergeFactor: 4, MaxRegexCandidates: 50}
			ix := mustOpen(t, cfg)
			ref, err := NewScanIndex(cfg)
			if err != nil {
				t.Fatal(err)
			}
			for i, d := range docs {
				_ = ix.Add(d)
				_ = ref.Add(d)
				if i%97 == 96 {
					_ = ix.Refresh()
				}
			}
			mustRefresh(t, ix, ref)
			for _, v := range ix.views() {
				if v.seg.Codec().ID() != cb.pc.ID() {
					t.Fatalf("%s: segment written with codec %s", name, v.seg.Codec().Name())
				}
			}
			for _, q := range qs {
				opts := SearchOptions{K: 20, Now: genNow}
				got, gerr := ix.Search(q, opts)
				want, werr := ref.Search(q, opts)
				if ok, why := sameOutcome(got, gerr, want, werr); !ok {
					t.Fatalf("%s: %q: %s", name, q, why)
				}
			}
		}
	}
	// Searcher selection.
	s, err := NewSearcher(Config{Schema: testSchema(), Engine: EngineScan})
	if _, ok := s.(*ScanIndex); err != nil || !ok {
		t.Fatalf("EngineScan gave %T, %v", s, err)
	}
	if _, err := Open(Config{Schema: testSchema(), Codec: fakeCodec{}}); err == nil {
		t.Fatal("a non-built-in codec was accepted")
	}
}

type fakeCodec struct{ VarintCodec }

func (fakeCodec) ID() uint16 { return 99 }

type labelledSet struct {
	Docs    []docJSON `json:"docs"`
	Queries []struct {
		Query    string   `json:"query"`
		Relevant []uint64 `json:"relevant"`
	} `json:"queries"`
}

// rankQuality returns MRR and precision@1 of scorer on the labelled set.
func rankQuality(t testing.TB, ls labelledSet, sc Scorer) (mrr, p1 float64) {
	ix, err := NewScanIndex(Config{Schema: testSchema(), Scorer: sc})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ls.Docs {
		if err := ix.Add(d.document()); err != nil {
			t.Fatal(err)
		}
	}
	_ = ix.Refresh()
	for _, q := range ls.Queries {
		res, err := ix.Search(q.Query, SearchOptions{K: 100})
		if err != nil {
			t.Fatal(err)
		}
		for i, h := range res.Hits {
			if slices.Contains(q.Relevant, h.ID) {
				mrr += 1 / float64(i+1)
				if i == 0 {
					p1++
				}
				break
			}
		}
	}
	n := float64(len(ls.Queries))
	return mrr / n, p1 / n
}

// TestTIdxRankQuality compares the scorers on the hand-labelled set
// (README "Choosing an algorithm"). It pins the measured values so a
// scoring change shows up as a test diff.
func TestTIdxRankQuality(t *testing.T) {
	cases := testkit.LoadCases[labelledSet](t, "textindex/ranking")
	want := map[string][2]float64{"bm25": {0, 0}, "tfidf": {0, 0}}
	for _, sc := range []Scorer{BM25Scorer{}, TFIDFScorer{}} {
		mrr, p1 := rankQuality(t, cases[0].Data, sc)
		t.Logf("%-6s MRR %.3f  P@1 %.3f", sc.Name(), mrr, p1)
		want[sc.Name()] = [2]float64{mrr, p1}
		if mrr < 0.5 {
			t.Errorf("%s: MRR %.3f below the 0.5 floor", sc.Name(), mrr)
		}
	}
	if want["bm25"][0] < want["tfidf"][0] {
		t.Errorf("BM25 (the default) ranks worse than TF-IDF on the labelled set: %v", want)
	}
}

// BenchmarkTIdxAlgo is the side-by-side comparison of every pluggable
// choice (docs/CONVENTIONS.md "Pluggable algorithms"):
//
//	codec/<codec>             bytes/posting, decode ns/posting, AND time
//	intersect/<algo>/1:<r>    two lists, the second r times shorter
//	rank/<scorer>             query latency and labelled-set MRR, P@1
//	regex/<strategy>          trigram prefilter against a full scan
//
// Run with go test -run '^$' -bench TIdxAlgo -benchmem ./textindex.
func BenchmarkTIdxAlgo(b *testing.B) {
	b.Run("codec", benchCodec)
	b.Run("intersect", benchIntersect)
	b.Run("rank", benchRank)
	b.Run("regex", benchRegex)
}

func codecSegment(b *testing.B, pc PostingCodec, n int) *Segment {
	docs, _ := benchCorpus(n)
	in := make([]IndexedDoc, len(docs))
	for i, d := range docs {
		in[i] = IndexedDoc{Doc: d}
	}
	data, err := EncodeSegment(DefaultSchema(), in, pc)
	if err != nil {
		b.Fatal(err)
	}
	sg, err := OpenSegment(data, DefaultSchema())
	if err != nil {
		b.Fatal(err)
	}
	return sg
}

// benchCodec reports, per codec on a 20,000-doc segment: bytes
// per posting (term and trigram postings incl. skip tables), decode
// ns/posting (full iteration of every term list) and the time of an
// AND of two frequent terms.
func benchCodec(b *testing.B) {
	for _, pc := range Codecs() {
		b.Run(pc.Name(), func(b *testing.B) {
			sg := codecSegment(b, pc, 20_000)
			// every term list
			type ref struct{ df, po, pp int }
			var lists []ref
			postings := 0
			r := newRd(sg.blocks)
			var key []byte
			for i := 0; i < sg.termCount; i++ {
				shared, suffix := int(r.uvarint()), int(r.uvarint())
				key = append(key[:shared], r.bytes(suffix)...)
				df, po, pp := int(r.uvarint()), int(r.uvarint()), int(r.uvarint())
				lists = append(lists, ref{df, po, pp})
				postings += df
			}
			for i := 0; i < sg.triCount; i++ {
				postings += int(binary.LittleEndian.Uint32(sg.triEnts[i*triEntrySize+4:]))
			}
			bytes := len(sg.postings) + len(sg.triData)
			var it postIter
			b.ReportAllocs()
			var decoded int
			for b.Loop() {
				for _, l := range lists {
					it.reset(sg.pc, sg.postings, l.po, l.df, true, sg.positions, l.pp)
					for d := it.nextDoc(); d != noMore; d = it.nextDoc() {
						decoded++
					}
				}
			}
			b.ReportMetric(float64(bytes)/float64(postings), "B/posting")
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(decoded), "decode-ns/posting")
			// intersection of two frequent terms, timed separately
			t0 := time.Now()
			reps := 0
			for time.Since(t0) < 200*time.Millisecond {
				a, c := sg.termIter(0, "get", nil), sg.termIter(0, "user", nil)
				and := newAndIter([]docIter{a, c})
				for d := and.nextDoc(); d != noMore; d = and.nextDoc() {
				}
				reps++
			}
			b.ReportMetric(float64(time.Since(t0).Nanoseconds())/float64(reps), "and-ns")
		})
	}
}

var (
	isectOnce sync.Once
	isectSeg  *Segment
)

// intersectSegment holds 200,000 docs; term "a" is in every other doc and
// term "r<k>" in one doc out of 2k (ratios 1:1 .. 1:1000 against "a").
func intersectSegment(b *testing.B) *Segment {
	isectOnce.Do(func() { isectSeg = buildIntersectSegment(b) })
	return isectSeg
}

func buildIntersectSegment(b *testing.B) *Segment {
	const n = 200_000
	r := core.NewSplitMix64(5)
	docs := make([]IndexedDoc, n)
	for i := range docs {
		text := "x"
		if i%2 == 0 {
			text += " a"
		}
		for _, k := range []int{1, 10, 100, 1000} {
			if core.IntN(r, 2*k) == 0 {
				text += fmt.Sprintf(" r%d", k)
			}
		}
		docs[i] = IndexedDoc{Doc: Document{ID: uint64(i), Text: []FieldValue{{"text", text}}}}
	}
	data, err := EncodeSegment(DefaultSchema(), docs, nil)
	if err != nil {
		b.Fatal(err)
	}
	sg, err := OpenSegment(data, DefaultSchema())
	if err != nil {
		b.Fatal(err)
	}
	return sg
}

// benchIntersect intersects "a" (100,000 postings) with a list
// 1, 10, 100 or 1000 times shorter, per intersector.
func benchIntersect(b *testing.B) {
	sg := intersectSegment(b)
	for _, is := range []Intersector{SkipIntersector{}, GallopIntersector{}, LinearIntersector{}} {
		for _, k := range []int{1, 10, 100, 1000} {
			b.Run(fmt.Sprintf("%s/1:%d", is.Name(), k), func(b *testing.B) {
				b.ReportAllocs()
				n := 0
				for b.Loop() {
					it := is.intersect([]docIter{sg.termIter(0, "a", nil), sg.termIter(0, fmt.Sprintf("r%d", k), nil)})
					for d := it.nextDoc(); d != noMore; d = it.nextDoc() {
						n++
					}
				}
				b.ReportMetric(float64(n)/float64(b.N), "matches/op")
			})
		}
	}
}

// benchRank runs the term and phrase benchmark queries on the
// 100,000-doc index with each scorer and reports the labelled-set MRR.
func benchRank(b *testing.B) {
	ix := benchIndex(b)
	cases := testkit.LoadCases[labelledSet](b, "textindex/ranking")
	qs := []string{"token", "getUserId", "cart", `"await fetch"`, `"user id"`, "session_retry"}
	for _, sc := range []Scorer{BM25Scorer{}, TFIDFScorer{}} {
		b.Run(sc.Name(), func(b *testing.B) {
			saved := ix.x.scorer
			ix.x.scorer = sc
			defer func() { ix.x.scorer = saved }()
			mrr, p1 := rankQuality(b, cases[0].Data, sc)
			benchQueries(b, qs)
			b.ReportMetric(mrr, "MRR")
			b.ReportMetric(p1, "P@1")
		})
	}
}

// benchRegex runs the regex benchmark queries on the
// 100,000-doc index with the trigram prefilter and with a full scan.
func benchRegex(b *testing.B) {
	ix := benchIndex(b)
	qs := []string{`/fetch\('\/api\/v2\/\w+/`, `/secret[A-Z]\w*/`, `/session_?retry/i`, `/https:\/\/cdn\.example\.com\/[a-z]+\//`}
	for _, rx := range []RegexStrategy{TrigramPrefilter{}, FullScan{}} {
		b.Run(rx.Name(), func(b *testing.B) {
			saved := ix.x.regex
			ix.x.regex = rx
			defer func() { ix.x.regex = saved }()
			benchQueries(b, qs)
		})
	}
}
