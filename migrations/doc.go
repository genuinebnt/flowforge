// Package migrations holds the forward-only, numbered SQL migrations of the
// Flowforge database (spec section 10, 10.7 "Migrations") and a runner that
// applies them.
//
// The files in sql/ are embedded. 0002 to 0006 transcribe the DDL of spec
// sections 10.2 to 10.6 (plus graph.snapshots from section 21); 0007 adds
// row-level security (10.7); later files add the tables the spec implies
// but does not spell out (outboxes, inboxes, worker heartbeats, the
// gateway's HTTP idempotency store, scope decision partitions) and the
// pgvector tables of section 22.9, which are applied only when the vector
// extension is available. migrations/OPEN_QUESTIONS.md lists every
// deviation and addition.
//
// [Apply] records each file in public.schema_migrations with its SHA-256,
// runs each file in its own transaction under a transaction-level advisory
// lock (so concurrent migrators are safe, even through a pool), skips files
// already applied and refuses to run if an applied file was edited.
package migrations
