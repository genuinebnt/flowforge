// Package csr implements immutable Compressed Sparse Row graph snapshots
// and the whole-graph analytics that run on them (spec section 21, and the
// traversal operations of section 5.9).
//
// A snapshot is a derived, disposable read model of one (tenant, program)
// graph. Build renumbers entities to dense uint32 indices in entity-id
// order and lays out forward and transposed CSR arrays with an O(V + E)
// counting sort (21.2). WriteTo/Encode produce the "FFCS" file format of
// 21.1 (little endian, 64-byte aligned sections, a crc32c per section and
// a sha256 footer); Open memory-maps a file and Decode parses bytes, both
// validating every checksum and structural invariant before any array is
// used, so a corrupt or hostile file yields an error, never an
// out-of-bounds access.
//
// The algorithms (21.5) are pure functions of a *Graph and parameters:
// BFS with depth bound, label filter and direction; ShortestPath
// (Dijkstra over -ln(confidence)); PageRank (pull-based, parallel,
// deterministic); WeaklyConnectedComponents (union-find);
// StronglyConnectedComponents (iterative Tarjan, reverse topological
// order); CoreNumbers/KCore (Batagelj-Zaversnik); FindCuts (articulation
// points and bridges); TriangleCount and CommonNeighbors (sorted-row
// intersection); and BlastRadius (5.9).
//
// Freshness (21.4) is handled by Overlay, a delta of change events above
// the snapshot watermark that answers point queries over snapshot plus
// delta; Holder swaps snapshots atomically for concurrent readers; and
// CompactionPolicy decides when to rebuild.
package csr
