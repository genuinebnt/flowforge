// Package pgstore implements eventstore.Store on PostgreSQL over the
// engine schema created by the migrations package (engine.runs,
// engine.run_events, engine.snapshots; spec 10.4). It is "Implementation A"
// of spec 23.4:
//
//   - Append is one transaction: SELECT ... FOR UPDATE on the run row,
//     check last_seq = expectedSeq and fence = $fence with the lease held,
//     insert the events, advance last_seq.
//   - AcquireLease is UPDATE engine.runs SET fence = fence + 1, ... WHERE
//     id = $r AND (lease_expires_at IS NULL OR lease_expires_at < now() OR
//     lease_owner = $w) RETURNING fence. now() is the database clock, the
//     single time source for lease expiry, so worker clock skew cannot break
//     ownership (23.8).
//   - ListReady and ClaimReady use FOR UPDATE SKIP LOCKED.
//   - Wakeups NOTIFY engine_wake (9.4); appends NOTIFY engine_events, which
//     Subscribe LISTENs on, with a polling fallback.
//   - Snapshots keep the newest three per run (10.7).
//
// The single bigint fence column maps to eventstore.Fence{Term: 0,
// Counter: fence}.
//
// Spec sections: 9.3, 9.4, 9.9, 10.4, 23.4, 23.8.
package pgstore
