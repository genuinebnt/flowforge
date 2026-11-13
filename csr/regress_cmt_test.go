package csr

import (
	"bytes"
	"math"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Build promises a byte-identical snapshot for any input order of the
// same edges. A confidence of -0 and one of +0 on parallel edges compared
// equal in the row sort but encode to different bits, so the file
// depended on the input order. Build now folds -0 into +0.
func TestRegress_CSRNegativeZeroConf(t *testing.T) {
	negZero := float32(math.Copysign(0, -1))
	nodes := []core.UUID{{1}, {2}}
	a := []Edge{{Src: 0, Dst: 1, Label: 0, Conf: negZero}, {Src: 0, Dst: 1, Label: 0, Conf: 0}}
	b := []Edge{a[1], a[0]}
	meta := Meta{CreatedAt: time.UnixMilli(0)}
	ga, err := Build(meta, nodes, nil, a, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	gb, err := Build(meta, nodes, nil, b, []string{"x"})
	if err != nil {
		t.Fatal(err)
	}
	ea, _ := ga.Encode()
	eb, _ := gb.Encode()
	if !bytes.Equal(ea, eb) {
		t.Fatal("snapshot bytes depend on the input order of -0 and +0 confidences")
	}
}
