package sketch

// FNV-1a 64 parameters (http://www.isthe.com/chongo/tech/comp/fnv/).
// PORT: the multiply wraps modulo 2^64 (wrapping_mul in Rust).
const (
	fnvOffset64 uint64 = 14695981039346656037
	fnvPrime64  uint64 = 1099511628211
)

// FNV1a64 is the 64-bit FNV-1a hash of s's bytes, the feature hash of
// SimHash and MinHash.
func FNV1a64(s string) uint64 {
	h := fnvOffset64
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime64
	}
	return h
}

// FNV1a64Bytes is FNV1a64 over a byte slice.
func FNV1a64Bytes(b []byte) uint64 {
	h := fnvOffset64
	for _, c := range b {
		h ^= uint64(c)
		h *= fnvPrime64
	}
	return h
}

// fnvUint32 folds the 4 little-endian bytes of v into h.
func fnvUint32(h uint64, v uint32) uint64 {
	for i := 0; i < 4; i++ {
		h ^= uint64(byte(v >> (8 * i)))
		h *= fnvPrime64
	}
	return h
}

// fnvUint64 folds the 8 little-endian bytes of v into h.
func fnvUint64(h uint64, v uint64) uint64 {
	for i := 0; i < 8; i++ {
		h ^= uint64(byte(v >> (8 * i)))
		h *= fnvPrime64
	}
	return h
}

// TokenHash is FNV-1a 64 over the 4 little-endian bytes of a token id. It
// is the feature hash of a single token in the token-id APIs. Token ids are
// only comparable across documents when they come from one shared interner
// (or another stable numbering).
func TokenHash(id uint32) uint64 { return fnvUint32(fnvOffset64, id) }

// TokensHash is FNV-1a 64 over the concatenated 4-byte little-endian
// encodings of ids: the hash of one token shingle.
func TokensHash(ids []uint32) uint64 {
	h := fnvOffset64
	for _, id := range ids {
		h = fnvUint32(h, id)
	}
	return h
}
