// Package graphstore is the Flowforge graph store: the GraphService API
// (spec 11.6) over the Open Asset Model graph, with two implementations
// that behave identically.
//
// It implements spec sections 5.7 (ingest and merge, via oam), 5.9
// (neighbors, path, blast radius, diff, components, search), 6.1
// (idempotent observations), 10.5 and 10.7 (graph tables, tenant
// isolation, trigram search), 21.2/21.4/21.6 (CSR snapshot build at a
// consistent watermark, snapshot catalogue, blob storage) and 23.5
// (convergent merge), plus benchmark b_sys_01 of 25.8.
//
// Pieces:
//
//   - Store, the port every implementation satisfies, with request and
//     result types that map one to one onto graph.proto;
//   - Mem, an in-memory Store built on oam.Graph (the reference merge),
//     one graph per (tenant, program), for tests and the dev stack;
//   - graphstore/pg, the Postgres Store (one transaction per observation,
//     merge rules in INSERT ... ON CONFLICT DO UPDATE plus the oam
//     primitives in Go, change events as the outbox, RLS);
//   - the shared algorithms both run on: TraverseNeighbors, TraversePath
//     and TraverseBlastRadius over a Reader, the trigram matcher
//     (Trigrams, Similarity, MatchSearch, SearchText) and BuildCSR;
//   - BlobStore with MemBlobs and DirBlobs, PolicyScope (the scope port),
//     page tokens and id canonicalization.
//
// Every operation takes the tenant from the verified Caller (IngestRequest
// carries it in TenantID) and filters by it; ids of another tenant are
// NOT_FOUND. Errors at the boundary are *core.Error values with stable
// reasons. No ambient time: ingest uses observed_at, everything else the
// injected core.Clock.
package graphstore
