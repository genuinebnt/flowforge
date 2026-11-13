package diff

import "fmt"

// Algorithm selects a diff algorithm. Every algorithm returns a canonical
// EditScript over the same token ids, so they are interchangeable behind
// Diff (and BoundedDiff's fallback chain); README "Choosing an algorithm"
// compares them.
type Algorithm uint8

// Algorithms. The zero value means DefaultAlgorithm.
const (
	// MyersLinearSpace is Myers with the linear-space refinement
	// (middle-snake divide and conquer): a shortest edit script in
	// O((N+M)D) time and O(N+M) memory. The default.
	MyersLinearSpace Algorithm = iota + 1
	// Myers is the basic greedy forward Myers algorithm with a full
	// backtrack trace: shortest, O((N+M)D) time but O(D^2) memory.
	Myers
	// Patience is git's patience diff (unique-line anchors). Not minimal.
	Patience
	// Histogram is git's histogram diff (low-occurrence anchors). Not
	// minimal.
	Histogram
	// Reference is the textbook O(NM) dynamic-programming LCS with a
	// backtrack: shortest, O(NM) time and memory. Inputs whose N*M exceeds
	// MaxReferenceCells fail with ErrInputTooLarge. For small inputs and as
	// an oracle.
	Reference
)

// DefaultAlgorithm is used when Options.Algorithm is zero.
const DefaultAlgorithm = MyersLinearSpace

// MaxReferenceCells bounds N*M for Reference (64 MiB of int32 cells).
const MaxReferenceCells = 1 << 24

var algorithmNames = [...]string{
	MyersLinearSpace: "myers-linear",
	Myers:            "myers",
	Patience:         "patience",
	Histogram:        "histogram",
	Reference:        "reference",
}

// Algorithms lists every algorithm, in a fixed order.
func Algorithms() []Algorithm {
	return []Algorithm{MyersLinearSpace, Myers, Patience, Histogram, Reference}
}

// String returns the algorithm's stable name ("myers-linear", "myers",
// "patience", "histogram", "reference"), as used in configuration.
func (a Algorithm) String() string {
	if int(a) < len(algorithmNames) && algorithmNames[a] != "" {
		return algorithmNames[a]
	}
	return fmt.Sprintf("Algorithm(%d)", uint8(a))
}

// ParseAlgorithm is the inverse of String.
func ParseAlgorithm(s string) (Algorithm, error) {
	for _, a := range Algorithms() {
		if a.String() == s {
			return a, nil
		}
	}
	return 0, fmt.Errorf("%w: unknown algorithm %q", ErrInvalidOptions, s)
}

// Options configures Diff.
type Options struct {
	// Algorithm is the algorithm; zero means DefaultAlgorithm.
	Algorithm Algorithm
	// MaxEditDistance caps the search: when the shortest script needs more
	// than this many inserted plus deleted tokens, MyersLinearSpace, Myers
	// and Reference stop and the result is Capped (the common prefix and
	// suffix kept, the middle replaced). For Patience and Histogram it caps
	// their Myers fallback ranges the same way. Zero means unlimited;
	// negative is invalid.
	MaxEditDistance int
	// GitCompact applies CompactGit (git's change compaction without the
	// indent heuristic) to the result, as git does after every algorithm.
	GitCompact bool
}

// Result is the outcome of Diff.
type Result struct {
	// Script transforms the old sequence into the new one. It is always
	// Apply-correct, also when Capped.
	Script EditScript
	// Capped reports that the MaxEditDistance cap was hit somewhere, so
	// Script may be longer than a shortest one.
	Capped bool
}

// Diff computes an edit script from a to b.
func Diff(a, b []uint32, opts Options) (Result, error) {
	r, err := diffRaw(a, b, opts)
	if err != nil || !opts.GitCompact {
		return r, err
	}
	s, err := CompactGit(a, b, r.Script)
	if err != nil {
		return Result{}, err
	}
	r.Script = s
	return r, nil
}

func diffRaw(a, b []uint32, opts Options) (Result, error) {
	if opts.MaxEditDistance < 0 {
		return Result{}, fmt.Errorf("%w: negative MaxEditDistance %d", ErrInvalidOptions, opts.MaxEditDistance)
	}
	limit := -1
	if opts.MaxEditDistance > 0 {
		limit = opts.MaxEditDistance
	}
	switch opts.Algorithm {
	case 0, MyersLinearSpace:
		var bld builder
		l := newLinear(&bld, a, b)
		if !l.run(0, len(a), 0, len(b), limit) {
			bld = builder{}
			fallback(&bld, a, b, 0, len(a), 0, len(b))
			return Result{Script: bld.finish(a, b), Capped: true}, nil
		}
		return Result{Script: bld.finish(a, b)}, nil
	case Myers:
		var bld builder
		if !myersGreedy(&bld, a, b, 0, len(a), 0, len(b), limit) {
			bld = builder{}
			fallback(&bld, a, b, 0, len(a), 0, len(b))
			return Result{Script: bld.finish(a, b), Capped: true}, nil
		}
		return Result{Script: bld.finish(a, b)}, nil
	case Reference:
		if len(a) > 0 && len(b) > MaxReferenceCells/len(a) {
			return Result{}, fmt.Errorf("%w: reference diff of %d x %d tokens exceeds %d cells", ErrInputTooLarge, len(a), len(b), MaxReferenceCells)
		}
		var bld builder
		if !referenceDiff(&bld, a, b, limit) {
			bld = builder{}
			fallback(&bld, a, b, 0, len(a), 0, len(b))
			return Result{Script: bld.finish(a, b), Capped: true}, nil
		}
		return Result{Script: bld.finish(a, b)}, nil
	case Patience:
		s, capped := patienceDiff(a, b, limit)
		return Result{Script: s, Capped: capped}, nil
	case Histogram:
		s, capped := histogramDiff(a, b, limit)
		return Result{Script: s, Capped: capped}, nil
	default:
		return Result{}, fmt.Errorf("%w: unknown algorithm %d", ErrInvalidOptions, opts.Algorithm)
	}
}

// DiffStrings interns a and b with a fresh interner and diffs them; it
// returns the interner so callers can map tokens back.
func DiffStrings(a, b []string, opts Options) (Result, *Interner, error) {
	in := NewInterner()
	ia, ib := in.InternAll(a), in.InternAll(b)
	r, err := Diff(ia, ib, opts)
	return r, in, err
}
