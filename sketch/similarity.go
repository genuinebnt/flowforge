package sketch

import (
	"fmt"
	"math"
)

// Method selects how EstimateSimilarity measures the similarity of two
// token-id sequences. All three estimate the same quantity, the Jaccard
// similarity of the sequences' w-shingle sets, so they are
// interchangeable; README "Choosing a method" compares them.
type Method uint8

// Similarity methods. The zero value means DefaultMethod.
const (
	// MethodExact computes JaccardSorted of the two WShingling sets:
	// exact, O(n log n), needs both sequences.
	MethodExact Method = iota + 1
	// MethodMinHash compares DefaultMinHasher signatures of the shingle
	// sets: unbiased, standard error sqrt(J(1-J)/128); a signature is
	// 1 KiB and can be stored and banded into an LSH index.
	MethodMinHash
	// MethodSimHash compares 64-bit SimHashTokens values: with Charikar's
	// P[bit differs] = θ/π it estimates cos θ = cos(π·hamming/64) and
	// converts it to Jaccard as c/(2-c), which assumes the two shingle sets
	// have about the same size. 8 bytes per document and Hamming-indexable
	// (SimHashIndex), but the coarsest estimate.
	MethodSimHash
)

// DefaultMethod is MethodExact: when both sequences are at hand the exact
// value is cheap; the sketches exist for comparing against stored
// documents.
const DefaultMethod = MethodExact

var methodNames = [...]string{MethodExact: "exact", MethodMinHash: "minhash", MethodSimHash: "simhash"}

// Methods lists every method, in a fixed order.
func Methods() []Method { return []Method{MethodExact, MethodMinHash, MethodSimHash} }

// String returns the method's stable configuration name ("exact",
// "minhash", "simhash").
func (m Method) String() string {
	if int(m) < len(methodNames) && methodNames[m] != "" {
		return methodNames[m]
	}
	return fmt.Sprintf("Method(%d)", uint8(m))
}

// ParseMethod is the inverse of String.
func ParseMethod(s string) (Method, error) {
	for _, m := range Methods() {
		if m.String() == s {
			return m, nil
		}
	}
	return 0, fmt.Errorf("%w: unknown similarity method %q", ErrInvalidConfig, s)
}

// SimilarityOptions configures EstimateSimilarity.
type SimilarityOptions struct {
	// Method is the estimator; zero means DefaultMethod.
	Method Method
	// ShingleWidth is w of the w-shingling; zero means DefaultShingleWidth.
	ShingleWidth int
}

// EstimateSimilarity estimates the Jaccard similarity in [0, 1] of the
// w-shingle sets of a and b with the configured method. Two empty
// sequences have similarity 1.
func EstimateSimilarity(a, b []uint32, opts SimilarityOptions) (float64, error) {
	w := opts.ShingleWidth
	if w == 0 {
		w = DefaultShingleWidth
	}
	if w < 0 {
		return 0, fmt.Errorf("%w: shingle width %d", ErrInvalidConfig, w)
	}
	switch opts.Method {
	case 0, MethodExact:
		return JaccardSorted(WShingling(a, w), WShingling(b, w)), nil
	case MethodMinHash:
		if len(a) == 0 && len(b) == 0 {
			return 1, nil
		}
		m := DefaultMinHasher()
		return EstimateJaccard(m.SignatureHashes(WShingling(a, w)), m.SignatureHashes(WShingling(b, w))), nil
	case MethodSimHash:
		if len(a) == 0 && len(b) == 0 {
			return 1, nil
		}
		c := math.Cos(math.Pi * float64(Hamming(SimHashTokens(a, w), SimHashTokens(b, w))) / 64)
		return max(c, 0) / (2 - max(c, 0)), nil
	default:
		return 0, fmt.Errorf("%w: unknown similarity method %d", ErrInvalidConfig, opts.Method)
	}
}
