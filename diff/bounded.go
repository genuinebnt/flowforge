package diff

import (
	"crypto/sha256"
	"fmt"
)

// Bounds defaults (V2_DESIGN §5).
const (
	// DefaultMaxEditDistance caps token-level Myers at 10,000 token edits.
	DefaultMaxEditDistance = 10_000
	// DefaultMaxInputBytes is the largest side (4 MiB of normalized text)
	// diffed at token level.
	DefaultMaxInputBytes = 4 << 20
	// DefaultMaxLineInputBytes is the largest side diffed with histogram on
	// lines; anything larger is compared by hash only.
	DefaultMaxLineInputBytes = 64 << 20
)

// Level is the granularity a bounded diff ended up at.
type Level uint8

// Levels, from finest to coarsest.
const (
	// LevelTokens: Myers over word tokens, within every bound.
	LevelTokens Level = iota + 1
	// LevelLines: histogram over lines, after the token level was refused
	// (input too large or edit distance over the cap).
	LevelLines
	// LevelHash: only the SHA-256 of each side, after the line level was
	// refused (input too large).
	LevelHash
)

// String returns "tokens", "lines" or "hash".
func (l Level) String() string {
	switch l {
	case LevelTokens:
		return "tokens"
	case LevelLines:
		return "lines"
	case LevelHash:
		return "hash"
	default:
		return fmt.Sprintf("Level(%d)", uint8(l))
	}
}

// Fallback says why a bounded diff left a finer level.
type Fallback uint8

// Fallback reasons.
const (
	// FallbackNone: the diff stayed at LevelTokens.
	FallbackNone Fallback = iota
	// FallbackEditDistance: token-level Myers exceeded MaxEditDistance.
	FallbackEditDistance
	// FallbackInputSize: a side exceeded MaxInputBytes (and, for LevelHash,
	// MaxLineInputBytes).
	FallbackInputSize
)

// String returns "none", "edit_distance" or "input_size".
func (f Fallback) String() string {
	switch f {
	case FallbackNone:
		return "none"
	case FallbackEditDistance:
		return "edit_distance"
	case FallbackInputSize:
		return "input_size"
	default:
		return fmt.Sprintf("Fallback(%d)", uint8(f))
	}
}

// Bounds configures BoundedDiff. Zero fields take the defaults; negative
// fields are invalid.
type Bounds struct {
	MaxEditDistance   int
	MaxInputBytes     int
	MaxLineInputBytes int
}

func (b Bounds) withDefaults() (Bounds, error) {
	if b.MaxEditDistance < 0 || b.MaxInputBytes < 0 || b.MaxLineInputBytes < 0 {
		return b, fmt.Errorf("%w: negative bound", ErrInvalidOptions)
	}
	if b.MaxEditDistance == 0 {
		b.MaxEditDistance = DefaultMaxEditDistance
	}
	if b.MaxInputBytes == 0 {
		b.MaxInputBytes = DefaultMaxInputBytes
	}
	if b.MaxLineInputBytes == 0 {
		b.MaxLineInputBytes = DefaultMaxLineInputBytes
	}
	return b, nil
}

// BoundedResult is the outcome of BoundedDiff.
type BoundedResult struct {
	Level    Level
	Fallback Fallback
	// Capped reports that the line-level histogram hit MaxEditDistance in a
	// Myers fallback range (that range is replaced as a whole). The script
	// is still Apply-correct.
	Capped bool
	// OldHash and NewHash are the SHA-256 of the inputs; Same reports that
	// they are byte-identical.
	OldHash, NewHash [32]byte
	Same             bool
	// Units maps token or line ids back to text; Old and New are the
	// sequences Script was computed over (SplitWords tokens at LevelTokens,
	// SplitLines lines at LevelLines; all nil at LevelHash).
	Units    *Interner
	Old, New []uint32
	Script   EditScript
}

// BoundedDiff diffs two normalized texts within the bounds of V2_DESIGN
// §5: token-level Myers capped at MaxEditDistance when both sides are at
// most MaxInputBytes; otherwise (or past the cap) histogram over lines when
// both sides are at most MaxLineInputBytes; otherwise hash only. The
// result records the level used and why.
func BoundedDiff(oldText, newText []byte, b Bounds) (BoundedResult, error) {
	b, err := b.withDefaults()
	if err != nil {
		return BoundedResult{}, err
	}
	r := BoundedResult{OldHash: sha256.Sum256(oldText), NewHash: sha256.Sum256(newText)}
	r.Same = r.OldHash == r.NewHash
	size := max(len(oldText), len(newText))
	r.Fallback = FallbackInputSize
	if size <= b.MaxInputBytes {
		in := NewInterner()
		a, bb := in.InternAll(SplitWords(string(oldText))), in.InternAll(SplitWords(string(newText)))
		res, err := Diff(a, bb, Options{Algorithm: MyersLinearSpace, MaxEditDistance: b.MaxEditDistance})
		if err != nil {
			return BoundedResult{}, err
		}
		if !res.Capped {
			r.Level, r.Fallback, r.Units, r.Old, r.New, r.Script = LevelTokens, FallbackNone, in, a, bb, res.Script
			return r, nil
		}
		r.Fallback = FallbackEditDistance
	}
	if size <= b.MaxLineInputBytes {
		in := NewInterner()
		a, bb := in.InternAll(SplitLines(oldText)), in.InternAll(SplitLines(newText))
		res, err := Diff(a, bb, Options{Algorithm: Histogram, MaxEditDistance: b.MaxEditDistance})
		if err != nil {
			return BoundedResult{}, err
		}
		r.Level, r.Capped, r.Units, r.Old, r.New, r.Script = LevelLines, res.Capped, in, a, bb, res.Script
		return r, nil
	}
	r.Level = LevelHash
	return r, nil
}
