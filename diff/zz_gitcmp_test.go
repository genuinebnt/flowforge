package diff

import (
	"encoding/json"
	"os"
	"testing"
)

func TestZZGitCompare(t *testing.T) {
	b, err := os.ReadFile(os.Getenv("GITJSON"))
	if err != nil {
		t.Skip()
	}
	var cases []struct {
		Name, Old, New string
		Git            map[string]string
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		ol, nl := SplitLines([]byte(c.Old)), SplitLines([]byte(c.New))
		for _, alg := range []Algorithm{Myers, Patience, Histogram} {
			for _, ctx := range []int{0, 1, 3} {
				res, _, _ := DiffStrings(ol, nl, Options{Algorithm: alg})
				u, _ := Unified(ol, nl, res.Script, UnifiedOptions{Context: ctx})
				key := map[Algorithm]string{Myers: "myers", Patience: "patience", Histogram: "histogram"}[alg] + "/" + string(rune('0'+ctx))
				if string(u) != c.Git[key] {
					t.Errorf("%s %s:\n--- ours\n%s--- git\n%s", c.Name, key, u, c.Git[key])
				}
			}
		}
	}
}
