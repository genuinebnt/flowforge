package textindex

import (
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

func TestScratchSmoke(t *testing.T) {
	for _, tok := range Analyze([]byte("getUserById(user_id) HTTPServer v2 __init__ XMLHttpRequest")) {
		t.Logf("%+v", tok)
	}
	ix, err := Open(Config{Schema: testSchema(), MergeFactor: 3})
	if err != nil {
		t.Fatal(err)
	}
	ref := newRef(testSchema(), 10000)
	r := core.NewSplitMix64(1)
	for i := 0; i < 200; i++ {
		d := genDoc(r, uint64(i))
		if err := ix.Add(d); err != nil {
			t.Fatal(err)
		}
		ref.add(d, false)
		if i%37 == 0 {
			if err := ix.Refresh(); err != nil {
				t.Fatal(err)
			}
			ref.refresh()
		}
	}
	ix.Refresh()
	ref.refresh()
	t.Logf("%+v", ix.Stats())
	bad := 0
	for i := 0; i < 500; i++ {
		q := genQueryTop(r)
		got, gerr := ix.Search(q, SearchOptions{K: 20, Now: genNow})
		want, werr := ref.search(q, SearchOptions{K: 20, Now: genNow})
		if ok, why := sameOutcome(got, gerr, want, werr); !ok {
			bad++
			if bad < 5 {
				t.Errorf("q=%q: %s\n got %+v %v\nwant %+v %v", q, why, got, gerr, want, werr)
			}
		}
	}
}
