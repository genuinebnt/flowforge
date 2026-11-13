// Package chunkstore is Flowforge's content-addressed, deduplicating,
// compressed blob store (V2_DESIGN §6.1, §6.3; layer 1: stdlib, core and
// github.com/klauspost/compress/zstd only).
//
// A blob is split by FastCDC (Xia et al., USENIX ATC 2016, normalized
// chunking level 2, gear table from core.SplitMix64 seeded 0x46464344)
// into chunks of 2 KiB to 64 KiB, averaging about 8 KiB. Each chunk is
// identified by the SHA-256 of its bytes, stored once per tenant
// namespace, and kept zstd-compressed (or raw when zstd saves less than
// 1/8) in packfiles ("FFPK") with a binary-searchable index ("FFPI"). A
// manifest ("FFMF": total length plus the ordered chunk ids) is itself
// stored as a chunk, so a blob's ManifestID is a content id. A ref file
// ("FFRF") names the live packs; replacing it atomically is the single
// commit point of every flush and garbage collection. FORMAT.md specifies
// every byte; the Rust port must match it and the conformance data in
// conformance/chunkstore/.
//
// Main API:
//
//	s, err := chunkstore.Open(ctx, backend, "tenant-42", chunkstore.Config{})
//	id, stats, err := s.Put(ctx, r)          // dedupes against the namespace
//	rc, err := s.Get(ctx, id)                 // verified on the fly
//	info, err := s.Stat(ctx, id)
//	rep, err := s.GC(ctx, live, pinned, chunkstore.GCOptions{})
//	frep, err := s.Fsck(ctx, manifests)       // or chunkstore.Fsck(ctx, backend, ns, ids)
//	m := s.Metrics()                          // dedupe and compression ratios as values
//
// Backend is the only port (named objects: Put/Get/GetRange/List/Delete);
// MemBackend is the in-memory implementation, chunkstore/fsstore the
// directory adapter and chunkstore/gcsstore the Cloud Storage adapter;
// chunkstore/backendtest is the contract suite all three run. FastCDC, the format codecs (DecodePack, DecodeIndex,
// DecodeManifest, DecodeRef and their encoders) and the error codes are
// usable on their own.
//
// Every error is an *Error with a stable Code from a closed list; fsck
// reports problems with the same codes. Nothing panics on input.
package chunkstore
