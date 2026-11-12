package placement

import (
	"slices"
	"testing"
)

// TestPlc001_TieBreak forces equal scores: the smaller id must win and rank
// first, whatever the order of the membership.
func TestPlc001_TieBreak(t *testing.T) {
	r := Rendezvous{score: func(uint64, WorkerID) uint64 { return 42 }}
	for _, live := range [][]WorkerID{{"c", "a", "b"}, {"b", "c", "a"}, {"a", "b", "c"}} {
		if w, _ := r.Owner([]byte("k"), live); w != "a" {
			t.Fatalf("Owner(%v) with tied scores = %q, want %q", live, w, "a")
		}
		if got := r.Ranking([]byte("k"), live); !slices.Equal(got, []WorkerID{"a", "b", "c"}) {
			t.Fatalf("Ranking(%v) with tied scores = %v", live, got)
		}
	}
}
