package contentstore

import (
	"github.com/genuinebnt/flowforge/chunkstore"
)

// BlobCodec selects how a snapshot's blobs are encoded before they reach
// the chunk store (a pluggable decision, docs/ALGORITHMS.md
// "contentstore: blob codec"; benchmark BenchmarkCStoreAlgo/codec/<name>).
type BlobCodec uint8

const (
	// CodecDelta (default): each blob is one zstd frame ("FFZB") whose
	// raw dictionary is the same blob of the snapshot's delta base (its
	// parent, or its lineage predecessor) and, for the derived blobs, the
	// snapshot's own body or normalized text. Versions with small edits
	// cost little more than the edit, and small artifacts compress as a
	// whole instead of per chunk. Chains are bounded by keyframes every
	// Config.KeyframeInterval versions.
	CodecDelta BlobCodec = iota
	// CodecChunk: blobs are stored as they are; the chunk store's FastCDC
	// dedupe and per-chunk zstd (level 3) do all the work. No chains; best
	// when versions share long unchanged runs (large bundles).
	CodecChunk
)

// String returns "delta" or "chunk".
func (c BlobCodec) String() string {
	if c == CodecChunk {
		return "chunk"
	}
	return "delta"
}

// ParseBlobCodec is the inverse of String.
func ParseBlobCodec(s string) (BlobCodec, error) {
	switch s {
	case "", "delta":
		return CodecDelta, nil
	case "chunk":
		return CodecChunk, nil
	}
	return 0, invalid("unknown blob codec %q (delta, chunk)", s)
}

// encodeBlob encodes one blob with the delta codec (chunkstore delta
// frames, "FFZB"; the byte format is in chunkstore/delta.go).
func encodeBlob(raw, dict []byte) ([]byte, error) {
	b, err := chunkstore.EncodeDelta(raw, dict)
	if err != nil {
		return nil, internal("delta encode", err)
	}
	return b, nil
}

// isBlob reports a delta-codec blob.
func isBlob(b []byte) bool { return chunkstore.IsDelta(b) }

// decodeBlob reverses encodeBlob.
func decodeBlob(b, dict []byte) ([]byte, error) {
	raw, err := chunkstore.DecodeDelta(b, dict)
	if err != nil {
		return nil, integrity("delta blob: %v", err)
	}
	return raw, nil
}
