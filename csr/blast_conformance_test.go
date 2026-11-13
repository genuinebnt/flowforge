package csr

import (
	"fmt"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

// blastFile is the blast-radius part of a conformance/csr file.
type blastFile struct {
	confFile
	Expect struct {
		BlastRadius []struct {
			Start uint32      `json:"start"`
			Depth int         `json:"depth"`
			Reach [][2]uint32 `json:"reach"`
		} `json:"blast_radius"`
	} `json:"expect"`
}

// TestCSRConformance_BlastRadius checks the blast_radius expectations of
// conformance/csr (files without any are skipped): the reached nodes and
// their distances, in rank order, over BlastRadiusLabels.
func TestCSRConformance_BlastRadius(t *testing.T) {
	n := 0
	for _, cf := range testkit.LoadCases[blastFile](t, suite) {
		for _, b := range cf.Data.Expect.BlastRadius {
			n++
			g := cf.Data.graph(t)
			got, err := BlastRadius(g, b.Start, b.Depth)
			if err != nil {
				t.Fatal(err)
			}
			var flat [][2]uint32
			for _, r := range got {
				flat = append(flat, [2]uint32{r.Node, uint32(r.Distance)})
			}
			if fmt.Sprint(flat) != fmt.Sprint(b.Reach) && !(len(flat) == 0 && len(b.Reach) == 0) {
				t.Errorf("%s: blast radius from %d depth %d = %v, want %v", cf.Name, b.Start, b.Depth, flat, b.Reach)
			}
		}
	}
	if n == 0 {
		t.Fatal("no blast_radius conformance cases")
	}
}
