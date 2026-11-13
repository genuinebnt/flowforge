// Package sketch holds the similarity sketches shared by content (chunk
// lineage, V2_DESIGN §5) and vecindex (near-duplicate detection, spec
// 22.8): 64-bit SimHash with multi-index Hamming lookup, a weighted SimHash
// over (token, weight) pairs, MinHash signatures with banded LSH, and
// shingling over strings, characters and interned token-id sequences.
//
// Everything is a pure, deterministic function of its input. Strings are
// hashed as their UTF-8 bytes with 64-bit FNV-1a; token ids are hashed as
// their 4 little-endian bytes. The seeds and formulas are part of the
// conformance contract (conformance/sketch, conformance/vecindex/simhash,
// conformance/vecindex/minhash): changing any of them changes every stored
// sketch.
//
// The bit-exact contract is spec 30.4 (MinHash and LSH parameters also
// spec 22.8). sketch is layer 1: it imports only the standard library and
// core.
package sketch
