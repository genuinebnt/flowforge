// Package textindex is Flowforge's full-text search over change history
// (V2_DESIGN §7): an inverted index with positions and a trigram index in
// immutable "FFTX" segments, its own query language, BM25 ranking, and
// regex search through a trigram prefilter.
//
// Pipeline: the code/URL analyzer (Analyze) splits text into words and
// camelCase, acronym, digit and snake_case parts, keeping each compound
// (README "Analyzer rules"). Writes are buffered and Refresh turns them
// into one segment plus tombstone sidecars and swaps the commit point
// atomically (FORMAT.md). Size-tiered merges rebuild segments without
// deleted documents. A query is lexed by a state machine, parsed by
// recursive descent into an arena AST (Parse), lowered against the schema
// into a plan, and executed per segment: candidates from postings (leapfrog
// intersection over skip entries, IIR §2.3), verification with positions,
// doc-values filters and regexps (Cox 2012), BM25 scores from index-wide
// live statistics (Robertson and Zaragoza 2009; Lucene's idf), and a
// top-k heap with deterministic tie-breaking.
//
// Posting codec, intersection algorithm, scorer and regex strategy are
// pluggable through Config (README "Choosing an algorithm"); ScanIndex is
// the brute-force engine with the same semantics. RRF fuses lexical
// results with vector search (vecindex's rank fusion, constant 60).
//
// Layer 1: the package imports only the standard library and
// github.com/klauspost/compress/zstd. It reads no clock (relative times
// resolve against SearchOptions.Now) and starts no goroutines.
package textindex
