// Package placement decides which worker owns a key (a run id, a target
// host) given the current live membership. It implements spec section 23.3
// (work distribution) behind the Placement port of section 24.4.
//
// Two algorithms are provided:
//
//   - Rendezvous (highest-random-weight) hashing: the owner of a key is the
//     live worker with the highest Hash64(key, worker) score, ties broken by
//     the smaller worker id. It keeps no state and, when a worker leaves,
//     moves only that worker's keys.
//   - ConsistentHash: a sorted ring of virtual nodes searched with binary
//     search, for key spaces that need per-worker weighting.
//
// Both are pure functions of their inputs: every node computes the same
// owner and the same claim ranking from the same membership, on every run
// and every platform. Ranking returns the preference order used for lease
// claiming; ranks only change who tries first, never who may write (the
// lease and its fence decide that).
//
// Membership is tracked separately: StaticMembership for fixed sets and
// tests, HeartbeatMembership for workers that report a heartbeat every
// 5 seconds and stay live for 15 seconds after the last one.
package placement
