// Package pg is the Postgres implementation of graphstore.Store over the
// graph schema of spec 10.5 (migrations/sql/0005_graph.sql) and the
// snapshot catalogue of spec 21.6.
//
// Ingest runs one transaction per observation (spec 5.7): the tenant is
// set with SET LOCAL app.tenant_id for row-level security (10.7), a
// per-(tenant, program) advisory lock orders writers so change-event seqs
// commit in order, the observation row is inserted with ON CONFLICT DO
// NOTHING (idempotency per observation id), the touched rows are loaded
// FOR UPDATE, merged in Go by the kernel (the oam rules) and written back
// with INSERT ... ON CONFLICT DO UPDATE whose update expressions keep
// created_at least and last_seen greatest (23.5), together with tags,
// search_text and the change events, which double as the outbox
// (published_at NULL; see Relay).
//
// Entity and edge ids are oam's derived ids, so this store and
// graphstore.Mem produce identical ids and identical state for identical
// input; the graphstore/storetest suite checks that.
package pg
