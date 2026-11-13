package migrations

import (
	"errors"
	"slices"
	"testing"
	"testing/fstest"
)

// TestRegress_MigrationGaps pins the numbering rule: gaps are allowed (the
// 0015-0029 range is reserved for v1 fixes merged forward), duplicate and
// non-positive versions are rejected, and files come back in version order
// regardless of name order.
func TestRegress_MigrationGaps(t *testing.T) {
	gapped := fstest.MapFS{
		"sql/0001_a.sql":   {Data: []byte("SELECT 1;")},
		"sql/0002_b.sql":   {Data: []byte("SELECT 2;")},
		"sql/0030_c.sql":   {Data: []byte("SELECT 30;")},
		"sql/0015_fix.sql": {Data: []byte("SELECT 15;")},
	}
	got, err := load(gapped)
	if err != nil {
		t.Fatalf("gapped set rejected: %v", err)
	}
	var versions []int
	for _, m := range got {
		versions = append(versions, m.Version)
	}
	if want := []int{1, 2, 15, 30}; !slices.Equal(versions, want) {
		t.Fatalf("order = %v, want %v", versions, want)
	}
	for name, fsys := range map[string]fstest.MapFS{
		"duplicate": {"sql/0001_a.sql": {Data: []byte("x")}, "sql/0001_b.sql": {Data: []byte("y")}},
		"zero":      {"sql/0000_a.sql": {Data: []byte("x")}},
	} {
		if _, err := load(fsys); !errors.Is(err, ErrBadFile) {
			t.Errorf("%s: err = %v, want ErrBadFile", name, err)
		}
	}
}
