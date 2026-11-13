package textindex

import "errors"

// refIndex is the brute-force oracle of the property tests.
type refIndex = ScanIndex

func newRef(s Schema, maxRegex int) *refIndex {
	r, err := NewScanIndex(Config{Schema: s, MaxRegexCandidates: maxRegex})
	if err != nil {
		panic(err) // test schemas are valid
	}
	return r
}

// sameOutcome compares an index result with the reference: equal error
// codes, or identical hits (ids, bit-identical scores, times) and totals.
func sameOutcome(got Result, gerr error, want Result, werr error) (bool, string) {
	var ge, we *QueryError
	if gerr != nil || werr != nil {
		if errors.As(gerr, &ge) && errors.As(werr, &we) && ge.Code == we.Code {
			return true, ""
		}
		return false, "errors differ: got " + errString(gerr) + " want " + errString(werr)
	}
	if got.Total != want.Total || len(got.Hits) != len(want.Hits) {
		return false, "totals or lengths differ"
	}
	for i := range got.Hits {
		if got.Hits[i] != want.Hits[i] {
			return false, "hit differs"
		}
	}
	return true, ""
}

func errString(e error) string {
	if e == nil {
		return "<nil>"
	}
	return e.Error()
}
