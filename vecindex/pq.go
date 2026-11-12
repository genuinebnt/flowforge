package vecindex

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/genuinebnt/flowforge/core"
)

// PQ defaults (spec 22.5).
const (
	// PQCentroids is the number of centroids per sub-space: one byte per
	// sub-code.
	PQCentroids = 256
	// DefaultPQTrainSample is the training sample size ("train on a sample
	// of 100k points").
	DefaultPQTrainSample = 100_000
	// DefaultKMeansIterations bounds Lloyd iterations per sub-space.
	DefaultKMeansIterations = 12
)

// PQConfig configures product-quantiser training.
type PQConfig struct {
	// M is the number of sub-vectors (bytes per code), 1..dim. When dim is
	// not a multiple of M the first dim%M sub-spaces get one extra
	// coordinate.
	M int
	// Centroids per sub-space, 1..256; zero means 256. Training clamps it
	// to the sample size.
	Centroids int
	// Iterations bounds Lloyd iterations; zero means 12.
	Iterations int
	// TrainSample is the number of vectors sampled for training; zero
	// means 100,000.
	TrainSample int
	// Workers trains sub-spaces in parallel; zero means GOMAXPROCS. The
	// result does not depend on it: each sub-space draws from its own
	// generator seeded up front.
	Workers int
}

// PQ is a trained product quantiser (spec 22.5). It is immutable and safe
// for concurrent use.
type PQ struct {
	dim       int
	m         int
	k         int
	offs      []int     // m+1 sub-space boundaries
	codebooks []float32 // sub-space s, centroid c at offs[s]*k + c*width(s)
}

// subspaceOffsets splits dim coordinates into m contiguous sub-spaces.
func subspaceOffsets(dim, m int) []int {
	offs := make([]int, m+1)
	base, extra := dim/m, dim%m
	for s := range m {
		w := base
		if s < extra {
			w++
		}
		offs[s+1] = offs[s] + w
	}
	return offs
}

// TrainPQ trains a quantiser on vectors (all of the same dimension) with
// k-means++ and Lloyd per sub-space. All randomness (the training sample
// and the seeding) comes from rng, so training is deterministic.
func TrainPQ(vectors [][]float32, cfg PQConfig, rng core.Rng) (*PQ, error) {
	if len(vectors) == 0 {
		return nil, fmt.Errorf("vecindex: train pq: %w", ErrEmpty)
	}
	dim := len(vectors[0])
	flat := make([]float32, 0, len(vectors)*dim)
	for i, v := range vectors {
		if len(v) != dim {
			return nil, fmt.Errorf("vecindex: train pq: vector %d: %w", i, ErrDimension)
		}
		flat = append(flat, v...)
	}
	return trainPQFlat(flat, len(vectors), dim, cfg, rng)
}

// trainPQFlat is TrainPQ over a row-major array.
func trainPQFlat(flat []float32, n, dim int, cfg PQConfig, rng core.Rng) (*PQ, error) {
	if n == 0 {
		return nil, fmt.Errorf("vecindex: train pq: %w", ErrEmpty)
	}
	if dim < 1 || dim > MaxDimension {
		return nil, fmt.Errorf("%w: pq dimension %d", ErrInvalidConfig, dim)
	}
	if cfg.M < 1 || cfg.M > dim {
		return nil, fmt.Errorf("%w: pq m=%d outside 1..%d", ErrInvalidConfig, cfg.M, dim)
	}
	k := cfg.Centroids
	if k == 0 {
		k = PQCentroids
	}
	if k < 1 || k > PQCentroids {
		return nil, fmt.Errorf("%w: pq centroids %d outside 1..256", ErrInvalidConfig, k)
	}
	iters := cfg.Iterations
	if iters == 0 {
		iters = DefaultKMeansIterations
	}
	sampleN := cfg.TrainSample
	if sampleN == 0 {
		sampleN = DefaultPQTrainSample
	}
	if iters < 0 || sampleN < 0 {
		return nil, fmt.Errorf("%w: negative pq iterations or sample", ErrInvalidConfig)
	}
	// Sample without replacement with a partial Fisher-Yates shuffle.
	rows := make([]int, n)
	for i := range rows {
		rows[i] = i
	}
	if sampleN < n {
		for i := range sampleN {
			j := i + core.IntN(rng, n-i)
			rows[i], rows[j] = rows[j], rows[i]
		}
		rows = rows[:sampleN]
	}
	k = min(k, len(rows))
	pq := &PQ{dim: dim, m: cfg.M, k: k, offs: subspaceOffsets(dim, cfg.M)}
	pq.codebooks = make([]float32, k*dim)
	seeds := make([]uint64, cfg.M)
	for s := range seeds {
		seeds[s] = rng.Uint64()
	}
	workers := cfg.Workers
	if workers <= 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(workers, cfg.M) {
		wg.Go(func() {
			for s := range jobs {
				lo, hi := pq.offs[s], pq.offs[s+1]
				w := hi - lo
				sub := make([]float32, 0, len(rows)*w)
				for _, r := range rows {
					sub = append(sub, flat[r*dim+lo:r*dim+hi]...)
				}
				cent := kmeans(sub, len(rows), w, k, iters, core.NewSplitMix64(seeds[s]))
				copy(pq.codebooks[lo*k:hi*k], cent)
			}
		})
	}
	for s := range cfg.M {
		jobs <- s
	}
	close(jobs)
	wg.Wait()
	return pq, nil
}

// Dim returns the vector dimension.
func (pq *PQ) Dim() int { return pq.dim }

// M returns the number of sub-vectors, which is the code size in bytes.
func (pq *PQ) M() int { return pq.m }

// Centroids returns the number of centroids per sub-space.
func (pq *PQ) Centroids() int { return pq.k }

// centroid returns centroid c of sub-space s.
func (pq *PQ) centroid(s, c int) []float32 {
	lo, hi := pq.offs[s], pq.offs[s+1]
	w := hi - lo
	base := lo*pq.k + c*w
	return pq.codebooks[base : base+w : base+w]
}

// Encode returns the m-byte code of v: per sub-space, the nearest centroid
// by squared L2.
func (pq *PQ) Encode(v []float32) ([]byte, error) {
	if len(v) != pq.dim {
		return nil, fmt.Errorf("vecindex: pq encode: %w: got %d, want %d", ErrDimension, len(v), pq.dim)
	}
	code := make([]byte, pq.m)
	pq.encodeInto(v, code)
	return code, nil
}

// encodeInto writes v's code to code (len m); v has length dim.
func (pq *PQ) encodeInto(v []float32, code []byte) {
	for s := range pq.m {
		lo, hi := pq.offs[s], pq.offs[s+1]
		w := hi - lo
		code[s] = byte(nearestCentroid(v[lo:hi], pq.codebooks[lo*pq.k:hi*pq.k], pq.k, w))
	}
}

// Decode reconstructs the vector a code stands for.
func (pq *PQ) Decode(code []byte) ([]float32, error) {
	if len(code) != pq.m {
		return nil, fmt.Errorf("vecindex: pq decode: %w: code length %d, want %d", ErrDimension, len(code), pq.m)
	}
	out := make([]float32, pq.dim)
	for s := range pq.m {
		c := int(code[s])
		if c >= pq.k {
			return nil, fmt.Errorf("vecindex: pq decode: %w: sub-code %d >= %d centroids", ErrCorrupt, c, pq.k)
		}
		copy(out[pq.offs[s]:pq.offs[s+1]], pq.centroid(s, c))
	}
	return out, nil
}

// DistanceTable is the per-query lookup table of asymmetric distance
// computation (spec 22.5): m x k partial distances from each query
// sub-vector to every centroid, so the approximate distance to a code is the
// sum of m table lookups.
type DistanceTable struct {
	m, k int
	bias float32 // 1 for Cosine (distance = 1 - dot), else 0
	lut  []float32
}

// Table builds the distance table of query q under metric. Under Cosine, q
// should already be unit length.
func (pq *PQ) Table(q []float32, metric Metric) (*DistanceTable, error) {
	if len(q) != pq.dim {
		return nil, fmt.Errorf("vecindex: pq table: %w: got %d, want %d", ErrDimension, len(q), pq.dim)
	}
	t := &DistanceTable{m: pq.m, k: pq.k, lut: make([]float32, pq.m*pq.k)}
	pq.fillTable(q, metric, t)
	return t, nil
}

// fillTable computes t's entries for q (len dim); t.lut has m*k entries.
func (pq *PQ) fillTable(q []float32, metric Metric, t *DistanceTable) {
	t.m, t.k, t.bias = pq.m, pq.k, 0
	if metric == Cosine {
		t.bias = 1
	}
	for s := range pq.m {
		sub := q[pq.offs[s]:pq.offs[s+1]]
		row := t.lut[s*pq.k : (s+1)*pq.k]
		for c := range pq.k {
			cent := pq.centroid(s, c)
			if metric == L2 {
				row[c] = L2Squared(sub, cent)
			} else {
				row[c] = -Dot(sub, cent)
			}
		}
	}
}

// Distance returns the approximate distance from the table's query to the
// vector encoded by code (len m). Sub-codes at or above k are treated as
// the last centroid; segments validate codes when they open.
func (t *DistanceTable) Distance(code []byte) float32 {
	s := t.bias
	lut := t.lut
	k := t.k
	code = code[:t.m]
	for i, c := range code {
		s += lut[i*k+min(int(c), k-1)]
	}
	return s
}

// bytes returns the quantiser's RAM footprint (codebooks and offsets).
func (pq *PQ) bytes() uint64 { return uint64(len(pq.codebooks)*4 + len(pq.offs)*8) }
