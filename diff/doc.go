// Package diff computes differences between token sequences, JSON
// documents and ordered labelled trees (V2_DESIGN §2 and §5; Myers 1986;
// RFC 6902; Zhang & Shasha 1989).
//
// Text is first interned (Interner, SplitLines, SplitWords) so every
// sequence algorithm works on []uint32 and compares tokens as integers:
//
//   - Myers: linear-space O(ND) Myers (middle-snake divide and conquer),
//     shortest edit script, optional edit-distance cap with a Capped result.
//   - MyersGreedy: the basic greedy forward algorithm, the reference.
//   - Patience and Histogram: git's xpatience.c and xhistogram.c, falling
//     back to Myers where git falls back to its classic diff.
//
// All return an EditScript (Equal/Delete/Insert over index ranges, with the
// changed tokens), which supports Apply, Invert, Compose, Hunks, unified
// rendering and parsing (Unified, ParseUnified, ApplyUnified), a
// structured side-by-side form with word-level refinement (SideBySideOf,
// RefineLine) and diff-match-patch style semantic cleanup
// (CleanupSemantic). BoundedDiff applies the §5 bounds: token Myers with a
// D cap, then histogram on lines, then hash only, recording the level.
//
// JSONDiff compares two ParseJSON trees and emits RFC 6902 add, remove and
// replace operations (arrays aligned by Myers over hash-consed element
// ids); ApplyPatch applies any RFC 6902 patch. TreeDiff is the Zhang–Shasha
// tree edit distance with an optimal mapping over an arena Tree interface.
//
// The package is pure and deterministic: no goroutines, no map iteration
// in outputs, and every error wraps a sentinel with a stable text. It is
// layer 1 and imports only the standard library.
package diff
