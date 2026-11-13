package htmlparse

import (
	"strings"
	"testing"
)

func treeDepth(d *Document, id NodeID) int {
	best := 0
	var walk func(NodeID, int)
	walk = func(n NodeID, k int) {
		if k > best {
			best = k
		}
		for c := range d.Children(n) {
			walk(c, k+1)
		}
		if c := d.Nodes[n].Content; c != None {
			walk(c, k+1)
		}
	}
	walk(id, 0)
	return best
}

func TestZZProbe(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("<b><div></b>", 200),
		strings.Repeat("<a><div><a>", 200),
		strings.Repeat("<p><b><div></p>", 200),
		"<b>" + strings.Repeat("<div></b>", 200),
		"<b><i>" + strings.Repeat("<div></b></i>", 200),
		strings.Repeat("<b>", 3) + strings.Repeat("<div></b>", 200),
		strings.Repeat("<template>", 20) + strings.Repeat("<div>", 20),
		"<table>" + strings.Repeat("<b>", 30),
	} {
		d, _ := Parse([]byte(in), Options{MaxDepth: 10})
		t.Logf("depth %d maxstack", treeDepth(d, d.Root))
	}
}
