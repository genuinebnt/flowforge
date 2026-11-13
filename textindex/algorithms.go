package textindex

import "math"

// Pluggable algorithm choices (docs/CONVENTIONS.md "Pluggable
// algorithms"; measurements and rationale in README "Choosing an
// algorithm"). Every alternative returns the same matching documents; only
// the Scorer changes scores and therefore order.

// Scorer weights one term (or phrase) occurrence in one field. A phrase
// uses the sum of its terms' IDF values (Lucene's PhraseQuery weight).
type Scorer interface {
	// Name is the stable configuration name ("bm25", "tfidf").
	Name() string
	// IDF of a term with document frequency df among n live documents.
	IDF(n, df int) float64
	// Score of a field with term (or phrase) frequency tf and length dl
	// (positions); avgdl is the mean field length over live documents.
	Score(idf float64, tf, dl int, avgdl float64) float64
}

// BM25Scorer is BM25 with k1 = 1.2, b = 0.75 and Lucene's idf (see BM25,
// BM25IDF). It is the default.
type BM25Scorer struct{}

// Name implements Scorer.
func (BM25Scorer) Name() string { return "bm25" }

// IDF implements Scorer.
func (BM25Scorer) IDF(n, df int) float64 { return BM25IDF(n, df) }

// Score implements Scorer.
func (BM25Scorer) Score(idf float64, tf, dl int, avgdl float64) float64 {
	return BM25(idf, tf, dl, avgdl)
}

// TFIDFScorer is Lucene's ClassicSimilarity without query norm and coord:
// sqrt(tf) * idf^2 / sqrt(dl) with idf = 1 + ln(n / (df + 1)). It has no
// tf saturation and a weaker length normalization than BM25.
type TFIDFScorer struct{}

// Name implements Scorer.
func (TFIDFScorer) Name() string { return "tfidf" }

// IDF implements Scorer.
func (TFIDFScorer) IDF(n, df int) float64 { return 1 + math.Log(float64(n)/float64(df+1)) }

// Score implements Scorer. dl > 0 whenever tf > 0.
func (TFIDFScorer) Score(idf float64, tf, dl int, _ float64) float64 {
	return math.Sqrt(float64(tf)) * idf * idf / math.Sqrt(float64(dl))
}

// Intersector chooses how conjunctions (AND clauses, phrase candidates,
// trigram AND nodes) intersect sorted doc-id lists. The method set is
// closed: pick one of SkipIntersector, GallopIntersector,
// LinearIntersector.
type Intersector interface {
	// Name is the stable configuration name ("skip", "gallop", "linear").
	Name() string
	intersect(its []docIter) docIter
}

// SkipIntersector leapfrogs the iterators, cheapest first; postings lists
// advance through their skip entries (one per 128 postings) without
// decoding skipped blocks. It is the default.
type SkipIntersector struct{}

// GallopIntersector materializes every list and leapfrogs with galloping
// (exponential, then binary) search: Bentley–Yao / Demaine et al.
type GallopIntersector struct{}

// LinearIntersector materializes every list and merges them by stepping
// one doc at a time: the textbook two-pointer merge (IIR §1.3).
type LinearIntersector struct{}

// Name implements Intersector.
func (SkipIntersector) Name() string { return "skip" }

// Name implements Intersector.
func (GallopIntersector) Name() string { return "gallop" }

// Name implements Intersector.
func (LinearIntersector) Name() string { return "linear" }

func (SkipIntersector) intersect(its []docIter) docIter { return newAndIter(its) }

func (GallopIntersector) intersect(its []docIter) docIter {
	out := make([]docIter, len(its))
	for i, it := range its {
		out[i] = newSliceIter(drain(it))
	}
	return newAndIter(out)
}

func (LinearIntersector) intersect(its []docIter) docIter {
	out := make([]docIter, len(its))
	for i, it := range its {
		out[i] = &linearIter{docs: drain(it), i: -1}
	}
	return newAndIter(out)
}

// drain collects the remaining docs of it.
func drain(it docIter) []uint32 {
	var out []uint32
	for d := it.nextDoc(); d != noMore; d = it.nextDoc() {
		out = append(out, d)
	}
	return out
}

// linearIter is a sorted slice whose advance steps one element at a time.
type linearIter struct {
	docs []uint32
	i    int
}

func (s *linearIter) docID() uint32 {
	if s.i < 0 || s.i >= len(s.docs) {
		return noMore
	}
	return s.docs[s.i]
}
func (s *linearIter) cost() int       { return len(s.docs) }
func (s *linearIter) nextDoc() uint32 { s.i++; return s.docID() }
func (s *linearIter) advance(t uint32) uint32 {
	if s.i < 0 {
		s.i = 0
	}
	for s.i < len(s.docs) && s.docs[s.i] < t {
		s.i++
	}
	return s.docID()
}

// RegexStrategy chooses how regex clauses find candidates. The method
// set is closed: TrigramPrefilter or FullScan. The REGEX_NO_TRIGRAMS rule
// depends on the regex, not on the strategy, so both strategies accept and
// reject the same queries.
type RegexStrategy interface {
	// Name is the stable configuration name ("trigram", "fullscan").
	Name() string
	useTrigrams() bool
}

// TrigramPrefilter takes candidates from the trigram index (Cox 2012) and
// verifies them with the full regexp. It is the default.
type TrigramPrefilter struct{}

// FullScan verifies the regexp on every document the rest of the query
// admits, ignoring the trigram index: the baseline, and the cheaper choice
// for tiny segments.
type FullScan struct{}

// Name implements RegexStrategy.
func (TrigramPrefilter) Name() string { return "trigram" }

// Name implements RegexStrategy.
func (FullScan) Name() string { return "fullscan" }

func (TrigramPrefilter) useTrigrams() bool { return true }
func (FullScan) useTrigrams() bool         { return false }

// execParams bundles the per-index execution choices.
type execParams struct {
	nt, timeF, k, regexLimit int
	scorer                   Scorer
	isect                    Intersector
	regex                    RegexStrategy
}

// defaultsFor fills the nil algorithm choices of cfg with the defaults.
func defaultsFor(cfg Config) (Scorer, Intersector, RegexStrategy) {
	s, i, r := cfg.Scorer, cfg.Intersector, cfg.Regex
	if s == nil {
		s = BM25Scorer{}
	}
	if i == nil {
		i = SkipIntersector{}
	}
	if r == nil {
		r = TrigramPrefilter{}
	}
	return s, i, r
}
