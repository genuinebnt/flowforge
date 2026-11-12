package vecindex

import (
	"encoding/binary"
	"io"
	"math"
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

// gaussian draws a standard normal value (Box-Muller) from r.
func gaussian(r core.Rng) float64 {
	u1 := core.Float64(r)
	for u1 == 0 {
		u1 = core.Float64(r)
	}
	u2 := core.Float64(r)
	return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
}

// mixture describes the synthetic "SIFT-like" corpus (generator
// gaussian_mixture_v1, also named by the PQ conformance data): clusters
// centres uniform in [0, 100)^dim; each point is its centre plus a
// low-rank component (a shared dim x latent Gaussian basis times latent
// N(0, spread^2) coordinates, scaled by 1/sqrt(latent)) plus isotropic
// N(0, noise^2) noise. Points and queries draw from the same mixture.
type mixture struct {
	Seed     uint64  `json:"seed"`
	N        int     `json:"n"`
	Queries  int     `json:"queries"`
	Dim      int     `json:"dim"`
	Clusters int     `json:"clusters"`
	Latent   int     `json:"latent"`
	Spread   float64 `json:"spread"`
	Noise    float64 `json:"noise"`
}

// siftLike is the default 128-d mixture used by vec_004, vec_005 and the
// benchmarks.
func siftLike(seed uint64, n, queries, dim int) mixture {
	return mixture{Seed: seed, N: n, Queries: queries, Dim: dim, Clusters: 64, Latent: 16, Spread: 20, Noise: 10}
}

// generate returns (base, queries).
func (m mixture) generate() ([][]float32, [][]float32) {
	r := core.NewSplitMix64(m.Seed)
	centres := make([][]float64, m.Clusters)
	for c := range centres {
		centres[c] = make([]float64, m.Dim)
		for j := range centres[c] {
			centres[c][j] = core.Float64(r) * 100
		}
	}
	basis := make([]float64, m.Dim*m.Latent)
	for i := range basis {
		basis[i] = gaussian(r) / math.Sqrt(float64(m.Latent))
	}
	point := func() []float32 {
		c := centres[core.IntN(r, m.Clusters)]
		z := make([]float64, m.Latent)
		for i := range z {
			z[i] = gaussian(r) * m.Spread
		}
		v := make([]float32, m.Dim)
		for j := range v {
			x := c[j] + gaussian(r)*m.Noise
			for l, zl := range z {
				x += basis[j*m.Latent+l] * zl
			}
			v[j] = float32(x)
		}
		return v
	}
	base := make([][]float32, m.N)
	for i := range base {
		base[i] = point()
	}
	qs := make([][]float32, m.Queries)
	for i := range qs {
		qs[i] = point()
	}
	return base, qs
}

// idOf is a deterministic id for vector i.
func idOf(i int) core.UUID {
	var u core.UUID
	u[0] = 0x01
	binary.BigEndian.PutUint64(u[8:], uint64(i)+1)
	return u
}

// randomVector draws dim N(0, 1) components.
func randomVector(r core.Rng, dim int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = float32(gaussian(r))
	}
	return v
}

// memFile is an in-memory io.WriterAt.
type memFile struct{ b []byte }

func (m *memFile) WriteAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, io.ErrShortWrite
	}
	if end := int(off) + len(p); end > len(m.b) {
		m.b = append(m.b, make([]byte, end-len(m.b))...)
	}
	copy(m.b[off:], p)
	return len(p), nil
}

// groundTruth returns, per query, the exact top-k node ids.
func groundTruth(t testing.TB, metric Metric, base, queries [][]float32, k int) [][]core.UUID {
	t.Helper()
	bf, err := NewBruteForce(metric, len(base[0]))
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range base {
		if err := bf.Upsert(idOf(i), v, 1); err != nil {
			t.Fatal(err)
		}
	}
	out := make([][]core.UUID, len(queries))
	for i, q := range queries {
		hits, err := bf.Search(q, SearchParams{K: k})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range hits {
			out[i] = append(out[i], h.ID)
		}
	}
	return out
}

// recallAt returns |got ∩ truth| / |truth| averaged over queries.
func recallAt(truth [][]core.UUID, got [][]core.UUID) float64 {
	var sum float64
	for i, tr := range truth {
		want := make(map[core.UUID]struct{}, len(tr))
		for _, id := range tr {
			want[id] = struct{}{}
		}
		hit := 0
		for _, id := range got[i] {
			if _, ok := want[id]; ok {
				hit++
			}
		}
		if len(tr) > 0 {
			sum += float64(hit) / float64(len(tr))
		}
	}
	return sum / float64(len(truth))
}

// buildTestSegment writes base into an in-memory segment and opens it.
func buildTestSegment(t testing.TB, base [][]float32, cfg SegmentConfig, seed uint64) (*Segment, []byte) {
	t.Helper()
	entries := make([]SegmentEntry, len(base))
	for i, v := range base {
		entries[i] = SegmentEntry{ID: idOf(i), Seq: uint64(i + 1), Vector: v}
	}
	var f memFile
	n, err := WriteSegment(&f, entries, cfg, core.NewSplitMix64(seed))
	if err != nil {
		t.Fatal(err)
	}
	if int(n) != len(f.b) {
		t.Fatalf("WriteSegment reported %d bytes, wrote %d", n, len(f.b))
	}
	seg, err := OpenSegment(bytesReaderAt(f.b), n)
	if err != nil {
		t.Fatal(err)
	}
	return seg, f.b
}

// bytesReaderAt adapts a byte slice to io.ReaderAt without bytes.Reader's
// seek state.
type bytesReaderAt []byte

func (b bytesReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off > int64(len(b)) {
		return 0, io.EOF
	}
	n := copy(p, b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
