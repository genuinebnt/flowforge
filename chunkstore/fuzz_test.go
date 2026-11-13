package chunkstore

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func fuzzSeeds(t testing.TB) (pack, idx, man, ref []byte) {
	c, err := newCodec()
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	w := NewPackWriter(42)
	var es []IndexEntry
	for _, x := range [][]byte{[]byte("hello"), bytes.Repeat([]byte("fuzz"), 100), []byte("z")} {
		codec, payload := c.encode(x)
		es = append(es, IndexEntry{ID: SumChunk(x), Offset: w.Append(uint32(len(x)), codec, payload), CompLen: uint32(len(payload)), RawLen: uint32(len(x))})
	}
	pack, pid, crc := w.Finish()
	ix, _ := NewIndex(pid, uint64(len(pack)), crc, es)
	ref, _ = (&Ref{Generation: 1, Packs: []PackID{pid}}).Encode()
	return pack, ix.Encode(), (&Manifest{TotalLen: 406, Chunks: []ChunkID{es[0].ID, es[1].ID, es[2].ID}}).Encode(), ref
}

// withTrailerCRC returns a copy of data whose last four bytes are the
// CRC32C of the rest, so the fuzzer reaches the structural checks behind
// the checksum.
func withTrailerCRC(data []byte) []byte {
	out := bytes.Clone(data)
	if len(out) >= 4 {
		binary.LittleEndian.PutUint32(out[len(out)-4:], CRC32C(out[:len(out)-4]))
	}
	return out
}

// withPackCRCs also fixes every record CRC it can reach by walking the
// records as the decoder does.
func withPackCRCs(data []byte) []byte {
	out := bytes.Clone(data)
	if len(out) < PackHeaderSize+PackFooterSize {
		return out
	}
	body := uint64(len(out) - PackFooterSize)
	for off := uint64(PackHeaderSize); off+RecordHeaderSize <= body; {
		n := uint64(binary.LittleEndian.Uint32(out[off+4:]))
		if n > body-off-RecordHeaderSize {
			break
		}
		rec := out[off : off+RecordHeaderSize+n]
		binary.LittleEndian.PutUint32(rec[12:], recordCRC(rec))
		off += RecordHeaderSize + n
	}
	return withTrailerCRC(out)
}

func checkCode(t *testing.T, err error, prefixes ...string) {
	t.Helper()
	code := CodeOf(err)
	if code == "" {
		t.Fatalf("error without a chunkstore code: %v", err)
	}
	for _, p := range prefixes {
		if strings.HasPrefix(string(code), p) {
			return
		}
	}
	t.Fatalf("unexpected code %s", code)
}

// FuzzChunker asserts the boundary invariants (CHUNK-INV-02/04/05) on
// arbitrary input, parameters and read sizes.
func FuzzChunker(f *testing.F) {
	f.Add([]byte("hello world"), uint8(0), uint16(1))
	f.Add(bytes.Repeat([]byte{0}, 5000), uint8(1), uint16(7))
	f.Add(splitmixBytes(1, 20_000), uint8(2), uint16(1000))
	f.Fuzz(func(t *testing.T, data []byte, p uint8, readSize uint16) {
		avg := 256 << (p % 3)
		cdc, err := NewFastCDC(ChunkerConfig{MinSize: avg / 4, AvgSize: avg, MaxSize: avg * 4})
		if err != nil {
			t.Fatal(err)
		}
		cuts := cdc.Boundaries(data)
		checkBounds(t, cdc, data, cuts)
		got := streamCuts(t, cdc, &fixedReader{b: data, n: 1 + int(readSize)})
		if len(got) != len(cuts) || (len(cuts) > 0 && !equalInts(got, cuts)) {
			t.Fatal("streaming and slice cuts differ")
		}
		if len(cuts) > 1 {
			tail := cdc.Boundaries(data[cuts[0]:])
			for i := range tail {
				if tail[i]+cuts[0] != cuts[i+1] {
					t.Fatal("re-chunking from the first cut diverges")
				}
			}
		}
	})
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// FuzzPackDecode: arbitrary bytes give a pack or a PACK_*/RECORD_* error;
// an accepted pack re-encodes to the same bytes, and verifying its
// records never panics.
func FuzzPackDecode(f *testing.F) {
	pack, _, _, _ := fuzzSeeds(f)
	f.Add(pack)
	f.Add(pack[:len(pack)-1])
	f.Add([]byte("FFPK"))
	c, err := newCodec()
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		for _, data := range [][]byte{input, withPackCRCs(input)} {
			fuzzPack(t, c, data)
		}
	})
}

func fuzzPack(t *testing.T, c *codec, data []byte) {
	{
		pk, err := DecodePack(data)
		if err != nil {
			checkCode(t, err, "PACK_", "RECORD_")
			return
		}
		w := NewPackWriter(pk.CreatedUnixNs)
		for _, r := range pk.Records {
			w.Append(r.RawLen, r.Codec, r.Payload(data))
			e := IndexEntry{Offset: r.Offset, CompLen: r.CompLen, RawLen: r.RawLen}
			if _, err := c.openRecord(e, data[r.Offset:r.Offset+e.RecordSize()]); err != nil {
				checkCode(t, err, "RECORD_", "CHUNK_")
			}
		}
		if out, _, _ := w.Finish(); !bytes.Equal(out, data) {
			t.Fatal("accepted pack does not re-encode to its bytes")
		}
	}
}

// FuzzIndexDecode: arbitrary bytes give an index or an INDEX_* error; an
// accepted index is canonical (re-encodes to the same bytes) and finds
// every entry by Lookup.
func FuzzIndexDecode(f *testing.F) {
	_, idx, _, _ := fuzzSeeds(f)
	f.Add(idx)
	f.Add(idx[:IndexHeaderSize+indexFanoutSize+4])
	f.Fuzz(func(t *testing.T, input []byte) {
		for _, data := range [][]byte{input, withTrailerCRC(input)} {
			ix, err := DecodeIndex(data)
			if err != nil {
				checkCode(t, err, "INDEX_")
				continue
			}
			if !bytes.Equal(ix.Encode(), data) {
				t.Fatal("accepted index is not canonical")
			}
			for _, e := range ix.Entries {
				if got, ok := ix.Lookup(e.ID); !ok || got != e {
					t.Fatal("lookup misses an entry")
				}
			}
		}
	})
}

// FuzzManifestDecode: arbitrary bytes give a manifest or a MANIFEST_*
// error; an accepted manifest is canonical.
func FuzzManifestDecode(f *testing.F) {
	_, _, man, _ := fuzzSeeds(f)
	f.Add(man)
	f.Add((&Manifest{}).Encode())
	f.Fuzz(func(t *testing.T, input []byte) {
		for _, data := range [][]byte{input, withTrailerCRC(input)} {
			m, err := DecodeManifest(data)
			if err != nil {
				checkCode(t, err, "MANIFEST_")
				continue
			}
			if !bytes.Equal(m.Encode(), data) {
				t.Fatal("accepted manifest is not canonical")
			}
		}
	})
}

// FuzzRefDecode: arbitrary bytes give a ref or a REF_* error; an accepted
// ref is canonical.
func FuzzRefDecode(f *testing.F) {
	_, _, _, ref := fuzzSeeds(f)
	f.Add(ref)
	f.Fuzz(func(t *testing.T, input []byte) {
		for _, data := range [][]byte{input, withTrailerCRC(input)} {
			r, err := DecodeRef(data)
			if err != nil {
				checkCode(t, err, "REF_")
				continue
			}
			out, err := r.Encode()
			if err != nil || !bytes.Equal(out, data) {
				t.Fatal("accepted ref is not canonical")
			}
		}
	})
}
