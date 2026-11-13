package textindex

import (
	"encoding/binary"
	"testing"
)

func TestScratchSizes(t *testing.T) {
	docs, raw := benchCorpus(10000)
	var in []IndexedDoc
	for _, d := range docs {
		in = append(in, IndexedDoc{Doc: d})
	}
	b, err := EncodeSegment(DefaultSchema(), in)
	if err != nil {
		t.Fatal(err)
	}
	dirOff := len(b) - segFooterSize - 4 - numSections*dirEntrySize
	names := []string{"", "meta", "ids", "norms", "keywords", "numerics", "termdict", "postings", "positions", "trigrams", "stored"}
	for i := 0; i < numSections; i++ {
		e := b[dirOff+4+i*dirEntrySize:]
		n := binary.LittleEndian.Uint64(e[12:])
		t.Logf("%-10s %9d  %6.1f B/doc", names[i+1], n, float64(n)/10000)
	}
	t.Logf("total %d raw %d ratio %.2f", len(b), raw, float64(len(b))/float64(raw))
}
