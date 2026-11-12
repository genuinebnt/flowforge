# csr: open questions and resolved ambiguities

Each item records the spec gap, the choice made and the test pinning it.

1. **Offset width (21.1 "u64 (u32 if E < 2^32)").** `in_edge` stores
   forward edge indices as u32, so E < 2^32 always holds and the rule would
   always pick u32. Offsets are always written as u64 to keep one layout
   and simple mmap views; no header flag for u32 offsets is defined, and
   unknown flags are rejected (`TestCSRDecodeRejects/unknown flag`). V and
   E are both limited to 2^32 - 1.
2. **Header details.** `created_at` is Unix milliseconds (matching the
   JSON timestamp precision); `Build` truncates `Meta.CreatedAt` so
   round trips are exact. Flag bit 0 = `FlagConfU8` (edge_conf is u8,
   value q/255), bit 1 = `FlagStaleExcluded` (21.4). Version is 1.
3. **Section table and footer.** Exactly the ten section kinds, each once,
   in any order; unknown kinds rejected. Sections and the footer start at
   multiples of 64; the sha256 covers every byte before the footer,
   padding included. Padding is written as zeros but not checked
   separately (the digest covers it).
4. **Check order.** The sha256 is verified before anything else, so any
   single corrupted byte, header included, yields `ErrChecksum`
   (`TestCSR003`). `OpenOptions.SkipDigest` skips only the sha256; crc32c
   and structural checks always run.
5. **Label dictionary encoding.** `count u16`, then per label a `u16` byte
   length and UTF-8 bytes. At most 256 labels (edge_label is u8), no
   duplicates, valid UTF-8, no trailing bytes. The dictionary order is the
   caller's; byte-identical builds (csr_009) assume the same dictionary.
6. **Canonical layout the decoder enforces.** Node ids strictly ascending
   (21.2 dense id order; reverse lookup is a binary search), rows sorted by
   target, and the transpose exactly equal to the one derived from the
   forward arrays (in-rows by source, ties by forward edge index). Foreign
   writers must follow 21.2. Within a row, duplicate targets are ordered by
   (label, confidence) so the build is a pure function of the content.
7. **Confidence domain.** [0, 1]; NaN and out-of-range values are rejected
   on build and decode. Confidence 0 is allowed but such an edge is never
   used by Dijkstra (infinite cost). With `FlagConfU8` `Build` quantises in
   memory so the decoded graph equals the built one.
8. **SCC output order.** 21.5 requires reverse topological order; the 21.3
   listing `{0} {2} {1,3,4}` is in topological order. We emit Tarjan's
   natural reverse topological order (`{1,3,4} {2} {0}`); conformance data
   stores the groups as sets and tests check the ordering property
   separately (`checkReverseTopological`).
9. **Undirected view.** k-core, articulation points, bridges, triangles and
   common neighbours use the simple undirected view: self loops dropped,
   parallel and reciprocal edges collapsed. A 2-cycle u<->v is therefore a
   bridge if it is the only connection (`TestCSR002/duplicate_edges`).
   Weakly connected components use all edges (equivalent).
10. **PageRank details.** Parallel edges count with multiplicity; self
    loops are ordinary out-edges. Zero option fields mean the defaults, so
    damping 0 cannot be requested. Results are bit-identical for any worker
    count (fixed 4096-node chunks summed in order).
11. **Direction.** `DIRECTION_UNSPECIFIED` is rejected (`ErrInvalidInput`)
    instead of defaulting to OUT.
12. **Depth limits.** Analytics BFS takes any depth (negative = unbounded);
    point queries (`BlastRadius`) enforce the 5.9 maximum of 6 and reject
    larger depths. blast_radius follows edges forward ("from has label
    to"); labels missing from the dictionary simply never match.
13. **Overlay semantics.** Edge removal deletes every edge with the same
    (source, target, label), base or added; the CSR has no relation
    discriminator (5.7). Events at or below the snapshot watermark are
    skipped, out-of-order events fail with `ErrSeqOrder`, edges naming an
    unknown entity fail with `ErrUnknownNode`. New labels are appended to
    the overlay's dictionary. Overlay "size" for the 1% trigger is the
    number of applied events. The 1% threshold is strict ("exceeds").
14. **Cold open cost (b_csr_03).** The benchmark asks for open cost
    independent of file size, but csr_003 and fuzz_csr_open require
    checksum and bounds validation before use, which is linear. We chose
    safety: arrays are used in place (no copy) but verified on open. A
    future option could verify lazily per section.
15. **Conformance location.** 21.5 says `conformance/graph/*.json`; the
    module manifest (`conformance = ["csr"]`) and the conventions use
    `conformance/csr/`, which is what this package reads.
16. **Dijkstra MaxCost.** Zero means unbounded (the proto has no max_cost
    field; 5.9 does). Negative or NaN is rejected.
