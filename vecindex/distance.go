package vecindex

import (
	"fmt"
	"math"
)

// Metric is the distance function of an index, fixed at creation (spec
// 22.2). The numeric values match the Metric enum of similarity.proto.
type Metric uint8

// Metrics. Smaller distances are always closer.
const (
	// MetricUnspecified is the zero value; it is rejected everywhere.
	MetricUnspecified Metric = 0
	// L2 is the squared Euclidean distance sum((a_i - b_i)^2). The square
	// root is skipped because it does not change the ranking.
	L2 Metric = 1
	// Cosine normalises vectors at insert time and uses 1 - dot(a, b).
	Cosine Metric = 2
	// InnerProduct uses -dot(a, b) (maximum inner product search).
	InnerProduct Metric = 3
)

// String returns the name used in graph.vector_indexes.metric.
func (m Metric) String() string {
	switch m {
	case L2:
		return "l2"
	case Cosine:
		return "cosine"
	case InnerProduct:
		return "ip"
	}
	return fmt.Sprintf("metric(%d)", uint8(m))
}

// Valid reports whether m is one of L2, Cosine or InnerProduct.
func (m Metric) Valid() bool { return m >= L2 && m <= InnerProduct }

// ParseMetric parses "l2", "cosine" or "ip" (the graph.vector_indexes
// spellings).
func ParseMetric(s string) (Metric, error) {
	switch s {
	case "l2":
		return L2, nil
	case "cosine":
		return Cosine, nil
	case "ip":
		return InnerProduct, nil
	}
	return MetricUnspecified, fmt.Errorf("%w: %q", ErrInvalidMetric, s)
}

// Distance returns the distance between a and b under m using the
// optimised kernels. For Cosine both vectors must already be unit length
// (indexes normalise at insert and at query time). a and b must have the
// same length: like copy-free slice kernels everywhere, a shorter b is a
// programmer error and panics with an index-out-of-range.
func Distance(m Metric, a, b []float32) float32 {
	switch m {
	case Cosine:
		return 1 - Dot(a, b)
	case InnerProduct:
		return -Dot(a, b)
	default:
		return L2Squared(a, b)
	}
}

// DistanceScalar is Distance computed with the scalar reference kernels.
// It is the oracle the optimised kernels are tested against (vec_001).
func DistanceScalar(m Metric, a, b []float32) float32 {
	switch m {
	case Cosine:
		return 1 - DotScalar(a, b)
	case InnerProduct:
		return -DotScalar(a, b)
	default:
		return L2SquaredScalar(a, b)
	}
}

// DotScalar is the reference inner product: one accumulator, in index
// order. It is deliberately naive.
//
// PORT: the Go compiler may fuse s += a*b into one FMA on arm64 (and on
// amd64 with GOAMD64=v3), here and in the unrolled kernels, so even the
// scalar results can differ in the last bits between platforms. Compare
// kernels with the vec_001 relative tolerance (1e-4), never bit for bit;
// a Rust port is free to use std::arch or std::simd kernels.
func DotScalar(a, b []float32) float32 {
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// L2SquaredScalar is the reference squared Euclidean distance.
func L2SquaredScalar(a, b []float32) float32 {
	var s float32
	for i := range a {
		d := a[i] - b[i]
		s += d * d
	}
	return s
}

// Dot is the optimised inner product.
//
// Go has no portable SIMD intrinsics, so this "SIMD" kernel is a pure-Go
// loop unrolled eight ways with eight independent accumulators. The
// independent accumulators break the floating-point add dependency chain,
// so the CPU's out-of-order core keeps several multiply-adds in flight, and
// the loop shape (both lengths in the loop condition, each block re-sliced
// to a constant length of eight) lets the compiler prove every index in
// bounds, so the loop carries no bounds checks. On arm64 the compiler also
// fuses each a*b+s into an FMA. A real AVX2 or NEON kernel (eight or four
// lanes per instruction) needs Go assembly, which this reference
// implementation does not use; see README.md.
func Dot(a, b []float32) float32 {
	b = b[:len(a)] // panics, like DotScalar, when b is shorter than a
	var s0, s1, s2, s3, s4, s5, s6, s7 float32
	for len(a) >= 8 && len(b) >= 8 {
		x := a[:8:8]
		y := b[:8:8]
		s0 += x[0] * y[0]
		s1 += x[1] * y[1]
		s2 += x[2] * y[2]
		s3 += x[3] * y[3]
		s4 += x[4] * y[4]
		s5 += x[5] * y[5]
		s6 += x[6] * y[6]
		s7 += x[7] * y[7]
		a, b = a[8:], b[8:]
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		s0 += a[i] * b[i]
	}
	return ((s0 + s1) + (s2 + s3)) + ((s4 + s5) + (s6 + s7))
}

// L2Squared is the optimised squared Euclidean distance; see Dot for how
// it is optimised.
func L2Squared(a, b []float32) float32 {
	b = b[:len(a)]
	var s0, s1, s2, s3, s4, s5, s6, s7 float32
	for len(a) >= 8 && len(b) >= 8 {
		x := a[:8:8]
		y := b[:8:8]
		d0 := x[0] - y[0]
		d1 := x[1] - y[1]
		d2 := x[2] - y[2]
		d3 := x[3] - y[3]
		d4 := x[4] - y[4]
		d5 := x[5] - y[5]
		d6 := x[6] - y[6]
		d7 := x[7] - y[7]
		s0 += d0 * d0
		s1 += d1 * d1
		s2 += d2 * d2
		s3 += d3 * d3
		s4 += d4 * d4
		s5 += d5 * d5
		s6 += d6 * d6
		s7 += d7 * d7
		a, b = a[8:], b[8:]
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		d := a[i] - b[i]
		s0 += d * d
	}
	return ((s0 + s1) + (s2 + s3)) + ((s4 + s5) + (s6 + s7))
}

// Normalize scales v in place to unit Euclidean length. It returns false,
// leaving v unchanged, when v is the zero vector or its norm is not finite.
func Normalize(v []float32) bool {
	var ss float64
	for _, x := range v {
		ss += float64(x) * float64(x)
	}
	if ss == 0 || math.IsInf(ss, 0) || math.IsNaN(ss) {
		return false
	}
	inv := 1 / math.Sqrt(ss)
	for i, x := range v {
		v[i] = float32(float64(x) * inv)
	}
	return true
}

// checkFinite reports whether every component of v is finite.
func checkFinite(v []float32) bool {
	for _, x := range v {
		if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
			return false
		}
	}
	return true
}

// prepareVector validates v against dim and returns the copy an index
// stores or searches with: identical for L2 and InnerProduct, unit length
// for Cosine.
func prepareVector(m Metric, dim int, v []float32) ([]float32, error) {
	if len(v) != dim {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrDimension, len(v), dim)
	}
	if !checkFinite(v) {
		return nil, fmt.Errorf("%w: non-finite component", ErrInvalidVector)
	}
	out := make([]float32, dim)
	copy(out, v)
	if m == Cosine && !Normalize(out) {
		return nil, fmt.Errorf("%w: zero vector cannot be normalised for cosine", ErrInvalidVector)
	}
	return out, nil
}

// sanitize maps a NaN distance (possible only when finite inputs overflow
// to opposite infinities) to +Inf so every ordering stays total.
func sanitize(d float32) float32 {
	if d != d {
		return float32(math.Inf(1))
	}
	return d
}
