// Package eventstore defines the event-store port of the Flowforge engine
// and its in-memory reference implementation (spec 23.4).
//
// The engine persists every run as an append-only, gapless event log
// (spec 9.3) and guarantees that at most one worker advances a run at a
// time with a lease carrying a fencing token (spec 9.4). [Store] is the
// narrow interface the engine depends on, so the implementation can change
// from Postgres (package eventstore/pgstore, "Implementation A") to a
// replicated log without touching the VM or the engine logic.
//
// The properties every implementation must satisfy (spec 23.4):
//
//  1. after a successful append at head h of n events, the next append
//     must name expectedSeq = h + n;
//  2. an append carrying an older fence fails with [ErrFenced] even if
//     expectedSeq matches;
//  3. a read after a successful append observes it (read-your-writes) and
//     reads are never reordered;
//  4. AcquireLease returns strictly increasing fences, including across
//     restarts of the store.
//
// They are checked by the conformance suite in package eventstore/storetest
// (spec 23.8, test ids es_001 to es_007), which runs unchanged against
// [MemStore] and pgstore.Store.
//
// Spec sections: 9.3 (event log), 9.4 (scheduling and single-writer
// guarantee), 9.9 (crash recovery), 23.4 (EventStore abstraction), 23.8
// (failure scenarios), 25.3 (es_001..es_007), 25.4 (random interleavings
// property), 25.8 (b_es_01, b_es_02).
package eventstore
