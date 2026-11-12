# core

Layer-0 building blocks shared by every Flowforge package (spec 1.2, 11.1,
24.4). Standard library only.

* **Ports:** `Clock` (wall time for display, monotonic time for deadlines
  and limiters) and `Rng`, with the production adapters `SystemClock` and
  `SystemRng`. These are the only code allowed to read the system clock or
  OS randomness (enforced by `internal/archtest`). `SplitMix64` is the
  seeded generator that tests, the simulator and anything that must
  reproduce from a seed use.
* **Identifiers:** `UUID` (version 7 via `NewUUIDv7`/`IDGen`) and public
  ids `<prefix>_<26 Crockford base32>` (`PublicID`, `ParsePublicID`),
  which sort in creation order.
* **Errors:** `Error{Kind, Reason, Message, ...}` with the stable reason
  codes of spec 11.8 and sentinels for `errors.Is`. Services map kinds to
  gRPC and HTTP codes; modules never do.
* **Encodings:** `CanonicalJSON` (sorted keys, no HTML escaping, numbers
  kept verbatim) for hashing, `DecodeStrict` for request bodies (unknown
  fields and trailing data rejected), `FormatTime`/`ParseTime` for the
  millisecond RFC 3339 wire format.
* **ActivityClass:** none < passive < active < intrusive.

`core/netnorm` holds the spec 4.2 target normalization; see its README.

Tests: `go test ./core/...`. The exact byte formats (UUIDv7 layout,
Crockford ids, SplitMix64, canonical JSON) are cross-language contracts,
described in `docs/PORTING_RUST.md` section 4.
