# placement: open questions

1. **Hash function.** Spec 23.3 suggests "xxh3 or siphash with a fixed key"
   but pins none. Chosen: FNV-1a 64 over `uint64_le(len(key)) ‖ key ‖
   worker`, finalized with MurmurHash3 `fmix64`. Standard library only,
   trivially portable, length-prefixed so `(key, worker)` pairs cannot
   collide by concatenation. Pinned by `conformance/placement/hash64.json`.
   Changing it moves every key, so it is part of the wire contract.
2. **Liveness boundary.** "Live for 15 seconds after its last heartbeat":
   chosen as live while `elapsed < 15s` (dead at exactly 15 s), measured on
   the monotonic clock. Pinned by `TestHeartbeatMembership`.
3. **"Within a few percent of uniform"** (plc_002) is read as every worker's
   share within 5 percentage points of `1/n` for 1,000 keys. With 1,000 keys
   the sampling noise alone is about 1.4 points per worker.
4. **Duplicates in `live`** are ignored (treated as a set); `Ranking` never
   repeats a worker.
5. **Consistent-hash positions.** Virtual node `i` of `w` sits at
   `Hash64(uint64_le(i), w)` and a key at `Hash64(key, "")`; equal points are
   ordered by worker id; a key maps to the first point at or after its
   position, wrapping. Weights are clamped to [1, 1024].
