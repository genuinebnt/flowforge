package chunkstore

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"hash/crc32"
)

// ChunkID is the SHA-256 of a chunk's uncompressed bytes.
type ChunkID [32]byte

// ManifestID is the ChunkID of an encoded manifest: the content id of a
// stored blob.
type ManifestID [32]byte

// PackID is the SHA-256 of a packfile's bytes; it names the pack and its
// index in the backend.
type PackID [32]byte

// String returns lowercase hex.
func (id ChunkID) String() string { return hex.EncodeToString(id[:]) }

// String returns lowercase hex.
func (id ManifestID) String() string { return hex.EncodeToString(id[:]) }

// String returns lowercase hex.
func (id PackID) String() string { return hex.EncodeToString(id[:]) }

// Compare orders ids bytewise.
func (id ChunkID) Compare(o ChunkID) int { return bytes.Compare(id[:], o[:]) }

// ParseManifestID parses 64 lowercase or uppercase hex digits.
func ParseManifestID(s string) (ManifestID, error) {
	var id ManifestID
	if len(s) != 64 {
		return id, errf(CodeInvalidConfig, s, "manifest id must be 64 hex digits")
	}
	if _, err := hex.Decode(id[:], []byte(s)); err != nil {
		return id, wrapErr(CodeInvalidConfig, s, err)
	}
	return id, nil
}

// SumChunk returns the ChunkID of data.
func SumChunk(data []byte) ChunkID { return sha256.Sum256(data) }

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// CRC32C returns the Castagnoli CRC of b (the checksum of every format).
func CRC32C(b []byte) uint32 { return crc32.Checksum(b, castagnoli) }
