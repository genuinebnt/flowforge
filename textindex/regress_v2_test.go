package textindex

import (
	"errors"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

// commitFailDir is a Directory whose commit-point writes fail while failing
// is set.
type commitFailDir struct {
	*MemDirectory
	failing bool
}

var errCommitWrite = errors.New("commit write refused")

func (d *commitFailDir) WriteFile(name string, data []byte) error {
	if d.failing && name == CommitFile {
		return errCommitWrite
	}
	return d.MemDirectory.WriteFile(name, data)
}

// TestRegress_MergeCommitFailureKeepsSegments: when a merge picked only
// fully deleted segments it wrote no new segment, yet on a failed commit
// it removed seg_<nextSeg-1>, the newest committed segment, which the
// unchanged commit point still names. The index then failed to reopen.
// A fully deleted segment only appears in a commit written by another
// writer, so the commit is built by hand here.
func TestRegress_MergeCommitFailureKeepsSegments(t *testing.T) {
	dir := &commitFailDir{MemDirectory: NewMemDirectory()}
	cfg := Config{Schema: DefaultSchema(), Dir: dir, DisableAutoMerge: true}
	ix, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for id := uint64(1); id <= 3; id++ {
		if err := ix.Add(Document{ID: id, Text: []FieldValue{{Name: "text", Value: "alpha beta"}}}); err != nil {
			t.Fatal(err)
		}
		if err := ix.Refresh(); err != nil {
			t.Fatal(err)
		}
	}
	if err := ix.Close(); err != nil {
		t.Fatal(err)
	}

	// Tombstone the only document of the first two segments.
	b, err := dir.ReadFile(CommitFile)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := decodeCommit(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.segs) != 3 {
		t.Fatalf("want 3 segments, have %d", len(cp.segs))
	}
	for i := 0; i < 2; i++ {
		s := &cp.segs[i]
		bm := newBitmap(int(s.docCount))
		bm.set(0)
		s.delGen, s.delCount = 1, 1
		if err := dir.WriteFile(tombFileName(s.name, 1), encodeTombstones(int(s.docCount), s.segCRC, 1, bm)); err != nil {
			t.Fatal(err)
		}
	}
	if err := dir.WriteFile(CommitFile, encodeCommit(cp)); err != nil {
		t.Fatal(err)
	}

	ix, err = Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dir.failing = true
	// The two empty segments are the smallest: the merge picks exactly them.
	if err := ix.ForceMerge(2); !errors.Is(err, errCommitWrite) {
		t.Fatalf("ForceMerge: want the commit failure, got %v", err)
	}
	dir.failing = false
	_ = ix.Close()

	ix, err = Open(cfg)
	if err != nil {
		t.Fatalf("reopen after failed merge commit: %v", err)
	}
	defer ix.Close()
	if _, ok, err := ix.Get(3); err != nil || !ok {
		t.Fatalf("Get(3) = %v, %v; want the document", ok, err)
	}
}

// TestTIdxSegmentCheckOrder pins the reader's check order (spec 32.6,
// FORMAT.md): header and end magics, version and codec come before the
// footer CRC, so damaging one is ErrCorrupt (the spec said the footer CRC
// came first, which would make it ErrChecksum); a damaged payload byte is
// ErrChecksum.
func TestTIdxSegmentCheckOrder(t *testing.T) {
	s := testSchema()
	data, err := EncodeSegment(s, randomDocs(testkit.Rng(t), 3), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		at   int
		want error
	}{{"magic", 0, ErrCorrupt}, {"version", 4, ErrCorrupt}, {"codec", 6, ErrCorrupt},
		{"end magic", len(data) - 1, ErrCorrupt}, {"payload", 8, ErrChecksum}, {"footer crc", len(data) - 8, ErrChecksum}} {
		b := slices.Clone(data)
		b[c.at] ^= 0x40
		if _, err := OpenSegment(b, s); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", c.name, err, c.want)
		}
	}
}
