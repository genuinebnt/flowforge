// Package content turns captured HTTP responses into normalized,
// versioned, comparable artifacts: it sniffs what a body really is,
// checks the capture's integrity, normalizes it with a versioned
// normalizer, extracts typed level-3 features, links renamed bundles to
// their predecessors (file-level SimHash and module-level hashing),
// diffs and classifies two versions of one artifact, scores the change
// with stored reasons, redacts credential-shaped strings, and emits oam
// observations with coverage-gated authority.
//
// It implements spec 33: docs/V2_DESIGN.md §5 (content) together with
// the §12 amendments, which win over §5 where they differ: gated
// authority and coverage (§3.4, §12.1), cause and baseline (§12.2,
// §12.3), scores with reasons that rank but never send (§12.4), noise and
// collection integrity (§12.5), module-level lineage "level 2.5" (§12.6),
// documents inside documents (§12.7), headers as artifacts (§12.8),
// sniffing (§12.9), parser assurance grades and amplification bounds
// (§12.10), redaction (§12.11) and change classes and newness (§12.13).
// Decision D2
// (docs/DECISIONS.md) holds: TypeScript, TSX and JSX sources are
// normalized and diffed as text and never handed to the JS parser.
//
// The package is pure (layer 2): no I/O, no clock, no randomness, no
// goroutines. Every function is deterministic, so the same inputs give
// byte-identical outputs (CONTENT-INV-12). Parsers are the layer-1
// packages htmlparse, jsparse, cssparse, xmlparse (+ sitemap), sourcemap
// and wellknown; diffs come from diff, sketches from sketch, base64, gzip
// and blob decoding from codec, graph types from oam.
//
// Pipeline (README.md has the diagram):
//
//	Capture ──Sniff──▶ Kind ──Normalize──▶ Normalized (+ derived documents)
//	   │                  └──Extract────▶ FeatureSet (+ grades, findings)
//	   └──CheckIntegrity(features, previous, host bodies)──▶ Outcome
//	Normalized ──Redact──▶ indexable text
//	(old, new) ──DiffArtifacts──▶ hunks, levels, class, newness, cause, score
//	Observe ──▶ oam.Observation with Authority{coverage, covered_rules}
//
// PORT: the package uses no maps whose iteration order reaches an
// output; every map is either a lookup table or iterated over sorted
// keys. Strings are UTF-8 byte strings; offsets are byte offsets.
package content
