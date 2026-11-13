package chunkstore

import (
	"bytes"
	"errors"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// Delta frames round-trip under their dictionary, are deterministic, are
// much smaller than a dictionary-free frame for a small edit, refuse a
// missing dictionary, and detect a flipped byte or truncation by code.
func TestChunkDeltaFrames(t *testing.T) {
	r := testkit.Rng(t)
	base := make([]byte, 200_000)
	for i := range base {
		base[i] = "abcdefghij klmnop\n"[core.IntN(r, 18)]
	}
	edit := bytes.Clone(base)
	copy(edit[100_000:], "a small edit in the middle")
	for _, dict := range [][]byte{nil, base} {
		f, err := EncodeDelta(edit, dict)
		if err != nil {
			t.Fatal(err)
		}
		g, _ := EncodeDelta(edit, dict)
		if !bytes.Equal(f, g) || !IsDelta(f) {
			t.Fatal("not deterministic or not a delta frame")
		}
		got, err := DecodeDelta(f, dict)
		if err != nil || !bytes.Equal(got, edit) {
			t.Fatalf("round trip with dict %d bytes: %v", len(dict), err)
		}
		if dict == nil {
			continue
		}
		plain, _ := EncodeDelta(edit, nil)
		if len(f)*20 > len(plain) {
			t.Fatalf("delta %d bytes vs %d without the dictionary", len(f), len(plain))
		}
		if _, err := DecodeDelta(f, nil); !errors.Is(err, &Error{Code: CodeChunkDecompress}) {
			t.Fatalf("missing dictionary: %v", err)
		}
		for i := range f {
			bad := bytes.Clone(f)
			bad[i] ^= 0x41
			if out, err := DecodeDelta(bad, dict); err == nil && !bytes.Equal(out, edit) {
				t.Fatalf("flipped byte %d decoded to other bytes without an error", i)
			}
		}
		if _, err := DecodeDelta(f[:len(f)/2], dict); err == nil {
			t.Fatal("truncated frame decoded")
		}
	}
}
