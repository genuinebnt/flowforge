# chunkstore open questions

Choices made where V2_DESIGN §6.1 is silent or ambiguous, with the
conservative option taken and the test that pins it.

1. **Record header widened.** §6.1 lists records as `{len, crc32c,
   frame}`; the brief for this package asked for `{raw_len, comp_len,
   crc32c}` and the later amendment added a codec byte. Version 1 uses a
   16-byte header `raw_len | stored_len | codec | reserved[3] | crc32c`,
   with the CRC over the first 12 header bytes plus the payload, so a
   range read of one record verifies its lengths and codec without the
   pack footer. Pinned by CHUNK-INV-08/14.

2. **Cut index semantics.** The paper's Algorithm returns `i` after
   hashing `data[i]`; we keep that (the deciding byte starts the next
   chunk) rather than `i+1`, so chunks are exactly in `[min, max]`.
   Pinned by `boundaries.json`.

3. **Masks for non-default averages.** The paper gives masks only for
   8 KiB. Other averages take prefixes of a fixed bit-position list whose
   first 15 entries reproduce the paper's MaskL/MaskA/MaskS (FORMAT.md
   §1). The 2020 TPDS "rolling two bytes" optimisation is not used: it
   changes cut points and the speed of the one-byte loop (1.4–1.9 GB/s)
   is sufficient.

4. **Compressed bytes are not portable.** C zstd (Rust `zstd` crate) and
   klauspost produce different frames, so pack/index/ref bytes match
   across implementations only when frames are copied. The format is
   designed for this (ids hash raw bytes; any valid frame is accepted).
   A Go dependency upgrade that changes klauspost's output changes new
   packs' bytes but no ids; `zstd.json`'s frame hash flags such a change.

5. **Manifest size.** One flat manifest per blob, at most
   `(64 MiB - 24) / 32` ≈ 2.1 M chunks (≈ 19 GB at the measured 9.4 KiB
   mean chunk). Larger inputs fail with `TOO_LARGE`. A tree manifest
   (flag bit in `flags`) is the planned extension; v1 readers reject
   non-zero flags.

6. **Presence cache is a full in-memory map** (≈ 60 bytes per chunk:
   12 M chunks per 100 GB). A future version can serve lookups from the
   indexes' fan-out tables (mmap) with a Bloom filter in front.

7. **GC grace instead of a lease protocol.** Callers pass the live set;
   the store protects running Puts and blobs whose Put returned since the
   previous GC began. A caller that records a manifest id later than the
   next-but-one GC loses it. contentstore must record ids before
   triggering the second GC after a Put (document there).

8. **Single writer per namespace.** Orphan deletion in GC assumes no
   other process is writing packs in the namespace. Multi-writer needs a
   lock object or leases in the Backend (not in v1; G-CST-03, v3).
   `gcsstore` detects a second writer instead: generation preconditions
   turn the ref replacement into a compare-and-swap, and the sweep checks
   the ref generation before each delete (see its package comment for the
   window that remains).

9. **Put durability.** By default each Put seals its own pack (small
   packs for small blobs: about 1.1 KiB of index overhead each). With
   `DeferFlush` a Put is not durable until `Flush`/`Close`. GC does not
   yet coalesce small fully-live packs; a size-based merge rule would be
   a GC option, not a format change.

10. **Live ratio is measured in record bytes** (header + payload), not
    raw bytes, and compared with integers (`live*100 < pct*total`) so the
    Rust port makes the same decisions.

11. **Locking granularity.** Sealing writes a 16 MiB pack while holding
    the store's write lock, so Gets stall during a seal, and GC is stop
    the world for the namespace. Acceptable for v1 throughput; a
    copy-on-write location table would remove it.

12. **Metrics scope.** Ingest counters (`LogicalBytes`, dedupe ratio)
    count Puts through this Store value since Open; content counters
    (`RawBytes`, `CompressedBytes`, `ObjectBytes`) describe the backend.
    V2_DESIGN asks for ratios "per program": a Store is per tenant, so
    contentstore aggregates per program from Put Stats.

13. **Single-segment frames for manifests** require the decoder to
    allocate the full content size (≤ 64 MiB); we cap decoder memory at
    MaxRecordRaw and decode into a buffer of exactly raw_len.
