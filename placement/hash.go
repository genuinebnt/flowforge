package placement

import "encoding/binary"

// FNV-1a 64-bit parameters.
const (
	fnvOffset64 = 14695981039346656037
	fnvPrime64  = 1099511628211
)

// Hash64 is the placement score of worker for key. It is part of the
// cross-implementation contract (conformance/placement pins test vectors):
//
//	h = FNV-1a-64( uint64_le(len(key)) || key || worker )
//	Hash64 = fmix64(h)
//
// where fmix64 is the MurmurHash3 64-bit finalizer. The length prefix makes
// the (key, worker) encoding unambiguous; the finalizer gives FNV's output
// full avalanche so the ordering of workers that differ only in their last
// byte is uniformly random.
func Hash64(key []byte, worker WorkerID) uint64 {
	return fmix64(fnvString(keyState(key), string(worker)))
}

// keyState returns the FNV-1a state after absorbing the length-prefixed
// key, so callers scoring many workers absorb the key once.
func keyState(key []byte) uint64 {
	var n [8]byte
	binary.LittleEndian.PutUint64(n[:], uint64(len(key)))
	return fnvBytes(fnvBytes(fnvOffset64, n[:]), key)
}

func fnvBytes(h uint64, b []byte) uint64 {
	for _, c := range b {
		h ^= uint64(c)
		h *= fnvPrime64
	}
	return h
}

func fnvString(h uint64, s string) uint64 {
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= fnvPrime64
	}
	return h
}

// fmix64 is the MurmurHash3 64-bit finalizer, a bijection with full
// avalanche.
func fmix64(k uint64) uint64 {
	k ^= k >> 33
	k *= 0xff51afd7ed558ccd
	k ^= k >> 33
	k *= 0xc4ceb9fe1a85ec53
	k ^= k >> 33
	return k
}
