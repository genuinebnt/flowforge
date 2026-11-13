package textindex

import "math"

// BM25 parameters (spec 32.4, V2_DESIGN §7).
const (
	BM25K1 = 1.2
	BM25B  = 0.75
)

// BM25IDF is Lucene's BM25 idf: ln(1 + (N - df + 0.5) / (df + 0.5)),
// always positive. N is the number of live documents in the index and df
// the number of live documents whose field contains the term.
func BM25IDF(n, df int) float64 {
	return math.Log(1 + (float64(n)-float64(df)+0.5)/(float64(df)+0.5))
}

// BM25 is the classic Robertson term weight with Lucene's idf:
//
//	idf * tf * (k1 + 1) / (tf + k1 * (1 - b + b * dl / avgdl))
//
// tf is the term (or phrase) frequency in the field, dl the field length
// in positions and avgdl the mean field length over live documents.
//
// PORT: keep the operation order of the code below. Go's math.Log is not
// correctly rounded and neither is Rust's f64::ln, so scores can differ in
// the last bits across languages; conformance compares them at 1e-9
// relative.
func BM25(idf float64, tf, dl int, avgdl float64) float64 {
	ftf := float64(tf)
	norm := BM25K1 * (1 - BM25B + BM25B*float64(dl)/avgdl)
	return idf * (ftf * (BM25K1 + 1)) / (ftf + norm)
}
