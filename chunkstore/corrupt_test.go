package chunkstore

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

// expectedFlipCode is the code a decoder must return for a single flipped
// byte at offset i: the magic is checked before the CRC, and the CRC
// covers every other byte.
func expectedFlipCode(i int, badMagic, checksum Code) Code {
	if i < 4 {
		return badMagic
	}
	return checksum
}

func flipStep(n int) int {
	if testing.Short() && n > 2000 {
		return 7
	}
	return 1
}

// smallCorruptStore builds a tiny store: two blobs, two packs.
func smallCorruptStore(t *testing.T) (*MemBackend, []ManifestID, [][]byte) {
	t.Helper()
	r := testkit.Rng(t)
	b := NewMemBackend()
	s := openStore(t, b, Config{Chunking: ChunkerConfig{MinSize: 64, AvgSize: 256, MaxSize: 1024}})
	var ids []ManifestID
	var blobs [][]byte
	for _, x := range [][]byte{textish(r, 1500), randBytes(r, 700)} {
		id, _ := mustPut(t, s, x)
		ids = append(ids, id)
		blobs = append(blobs, x)
	}
	return b, ids, blobs
}

// CHUNK-INV-07: flipping any single byte of any pack, index or ref of a
// store is detected: fsck reports the object's BAD_MAGIC (first four
// bytes) or CHECKSUM code with error severity; Open fails with that code
// for indexes and the ref; and Get never returns bytes that differ from
// what was Put (it returns the right bytes or an error).
func TestChunkInv07_StoreCorruptionDetected(t *testing.T) {
	ctx := context.Background()
	b, ids, blobs := smallCorruptStore(t)
	base := snapshot(b)
	if rep, _ := Fsck(ctx, b, testNS, ids); len(rep.Problems) != 0 {
		t.Fatalf("clean store: %+v", rep.Problems)
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for k := range base {
			if !yield(k) {
				return
			}
		}
	})
	for _, name := range names {
		var badMagic, checksum Code
		switch {
		case strings.HasSuffix(name, ".pack"):
			badMagic, checksum = CodePackBadMagic, CodePackChecksum
		case strings.HasSuffix(name, ".idx"):
			badMagic, checksum = CodeIndexBadMagic, CodeIndexChecksum
		default:
			badMagic, checksum = CodeRefBadMagic, CodeRefChecksum
		}
		obj := base[name]
		for i := 0; i < len(obj); i += flipStep(len(obj)) {
			for _, mask := range []byte{0x01, 0xff} {
				objs := snapshot(b)
				objs[name][i] ^= mask
				cb := memFrom(objs)
				want := expectedFlipCode(i, badMagic, checksum)
				rep, err := Fsck(ctx, cb, testNS, ids)
				if err != nil {
					t.Fatal(err)
				}
				if rep.OK() || !rep.Has(want) {
					t.Fatalf("%s byte %d ^%#x: fsck %+v, want %s", name, i, mask, rep.Problems, want)
				}
				s, err := Open(ctx, cb, testNS, Config{Chunking: ChunkerConfig{MinSize: 64, AvgSize: 256, MaxSize: 1024}})
				if !strings.HasSuffix(name, ".pack") {
					if CodeOf(err) != want {
						t.Fatalf("%s byte %d: Open got %v, want %s", name, i, err, want)
					}
					continue
				}
				if err != nil {
					t.Fatalf("Open with a damaged pack: %v", err)
				}
				for j, id := range ids {
					got, err := getAll(s, id)
					if err == nil && !bytes.Equal(got, blobs[j]) {
						t.Fatalf("%s byte %d: Get returned wrong bytes without error", name, i)
					}
				}
			}
		}
	}
}

// CHUNK-INV-08: every decoder maps a single flipped byte to its
// BAD_MAGIC or CHECKSUM code, and a flipped byte in a record is caught by
// record verification (RECORD_MALFORMED for the length fields, which are
// checked against the index first, RECORD_CHECKSUM elsewhere).
func TestChunkInv08_DecoderCorruptionCodes(t *testing.T) {
	r := testkit.Rng(t)
	c, err := newCodec()
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	var ids []ChunkID
	w := NewPackWriter(123)
	var es []IndexEntry
	for _, x := range [][]byte{textish(r, 3000), randBytes(r, 500), textish(r, 40)} {
		id := SumChunk(x)
		ids = append(ids, id)
		codec, payload := c.encode(x)
		es = append(es, IndexEntry{ID: id, Offset: w.Append(uint32(len(x)), codec, payload), CompLen: uint32(len(payload)), RawLen: uint32(len(x))})
	}
	pack, pid, crc := w.Finish()
	ix, _ := NewIndex(pid, uint64(len(pack)), crc, es)
	idx := ix.Encode()
	man := (&Manifest{TotalLen: 3540, Chunks: ids}).Encode()
	ref, _ := (&Ref{Generation: 9, Packs: []PackID{pid, {1}}}).Encode()
	decoders := []struct {
		name               string
		data               []byte
		decode             func([]byte) error
		badMagic, checksum Code
	}{
		{"pack", pack, func(b []byte) error { _, err := DecodePack(b); return err }, CodePackBadMagic, CodePackChecksum},
		{"index", idx, func(b []byte) error { _, err := DecodeIndex(b); return err }, CodeIndexBadMagic, CodeIndexChecksum},
		{"manifest", man, func(b []byte) error { _, err := DecodeManifest(b); return err }, CodeManifestBadMagic, CodeManifestChecksum},
		{"ref", ref, func(b []byte) error { _, err := DecodeRef(b); return err }, CodeRefBadMagic, CodeRefChecksum},
	}
	for _, d := range decoders {
		if err := d.decode(d.data); err != nil {
			t.Fatalf("%s: clean decode: %v", d.name, err)
		}
		for i := range d.data {
			for _, mask := range []byte{0x01, 0x80, 0xff} {
				bad := slices.Clone(d.data)
				bad[i] ^= mask
				if got, want := CodeOf(d.decode(bad)), expectedFlipCode(i, d.badMagic, d.checksum); got != want {
					t.Fatalf("%s byte %d ^%#x: got %q, want %s", d.name, i, mask, got, want)
				}
			}
		}
		// Truncation at every length is a clean error, never a panic.
		for n := range len(d.data) {
			if err := d.decode(d.data[:n]); CodeOf(err) == "" {
				t.Fatalf("%s truncated to %d: %v", d.name, n, err)
			}
		}
	}
	for _, e := range es {
		rec := pack[e.Offset : e.Offset+e.RecordSize()]
		if _, err := c.openRecord(e, rec); err != nil {
			t.Fatal(err)
		}
		for i := range rec {
			bad := slices.Clone(rec)
			bad[i] ^= 0x01
			want := CodeRecordChecksum
			if i < 8 {
				want = CodeRecordMalformed
			}
			if _, err := c.openRecord(e, bad); CodeOf(err) != want {
				t.Fatalf("record byte %d: got %v, want %s", i, err, want)
			}
		}
	}
}
