package vecindex

import (
	"errors"
	"math"
	"slices"
	"strconv"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// hex64 is a uint64 carried as a hex string (JSON numbers lose precision
// above 2^53).
type hex64 uint64

func (h *hex64) UnmarshalText(b []byte) error {
	v, err := strconv.ParseUint(string(b), 16, 64)
	*h = hex64(v)
	return err
}

type bfHit struct {
	ID       core.UUID `json:"id"`
	Distance float64   `json:"distance"`
}

type bfStep struct {
	Op          string    `json:"op"`
	ID          core.UUID `json:"id"`
	Vector      []float32 `json:"vector"`
	Seq         uint64    `json:"seq"`
	K           int       `json:"k"`
	L           int       `json:"l"`
	Expect      []bfHit   `json:"expect"`
	ExpectError string    `json:"expect_error"`
}

type bfCase struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Metric      string   `json:"metric"`
	Dim         int      `json:"dim"`
	Tolerance   float64  `json:"tolerance"`
	Steps       []bfStep `json:"steps"`
}

var errorKinds = map[string]error{
	"dimension":      ErrDimension,
	"invalid_vector": ErrInvalidVector,
	"invalid_params": ErrInvalidParams,
}

// vec_002 (data): exact top-k with ties broken by id, run against the
// brute-force index and, as a differential check, against the segmented
// index both with everything in the memtable and with every search served
// from freshly flushed segments (on sets this small the beam search visits
// every node, so it must be exact too).
func TestVec002_BruteForceConformance(t *testing.T) {
	for _, cf := range testkit.LoadCases[bfCase](t, "vecindex/bruteforce") {
		c := cf.Data
		metric, err := ParseMetric(c.Metric)
		if err != nil {
			t.Fatalf("%s: %v", cf.Name, err)
		}
		impls := map[string]func(t *testing.T) (Index, func() error){
			"bruteforce": func(t *testing.T) (Index, func() error) {
				bf, err := NewBruteForce(metric, c.Dim)
				if err != nil {
					t.Fatal(err)
				}
				return bf, nil
			},
			"segmented_memtable": func(t *testing.T) (Index, func() error) {
				return testSegmented(t, metric, c.Dim, 1000, nil), nil
			},
			"segmented_segments": func(t *testing.T) (Index, func() error) {
				s := testSegmented(t, metric, c.Dim, 1000, func(cfg *Config) { cfg.DisableAutoMerge = true })
				return s, s.Flush
			},
		}
		for name, mk := range impls {
			t.Run(c.ID+"/"+name, func(t *testing.T) {
				idx, beforeSearch := mk(t)
				for i, st := range c.Steps {
					var err error
					var hits []Hit
					switch st.Op {
					case "upsert":
						err = idx.Upsert(st.ID, st.Vector, st.Seq)
					case "delete":
						err = idx.Delete(st.ID, st.Seq)
					case "search":
						if beforeSearch != nil {
							if ferr := beforeSearch(); ferr != nil {
								t.Fatal(ferr)
							}
						}
						hits, err = idx.Search(st.Vector, SearchParams{K: st.K, L: st.L})
					default:
						t.Fatalf("step %d: unknown op %q", i, st.Op)
					}
					if st.ExpectError != "" {
						if want := errorKinds[st.ExpectError]; want == nil || !errors.Is(err, want) {
							t.Fatalf("step %d: err = %v, want %s", i, err, st.ExpectError)
						}
						continue
					}
					if err != nil {
						t.Fatalf("step %d: %v", i, err)
					}
					if st.Op != "search" {
						continue
					}
					if len(hits) != len(st.Expect) {
						t.Fatalf("step %d: got %d hits %+v, want %d", i, len(hits), hits, len(st.Expect))
					}
					for j, h := range hits {
						w := st.Expect[j]
						if h.ID != w.ID || math.Abs(float64(h.Distance)-w.Distance) > c.Tolerance {
							t.Fatalf("step %d hit %d: got %s at %v, want %s at %v", i, j, h.ID, h.Distance, w.ID, w.Distance)
						}
					}
				}
			})
		}
	}
}

type pqCase struct {
	ID        string      `json:"id"`
	Generator *mixture    `json:"generator"`
	Vectors   [][]float32 `json:"vectors"`
	PQ        struct {
		M           int `json:"m"`
		Centroids   int `json:"centroids"`
		Iterations  int `json:"iterations"`
		TrainSample int `json:"train_sample"`
	} `json:"pq"`
	PQSeed         uint64  `json:"pq_seed"`
	MaxRelativeMSE float64 `json:"max_relative_mse"`
}

// vec_003: train on a sample, encode, decode; the relative reconstruction
// error stays below the bound in the data file.
func TestVec003_PQConformance(t *testing.T) {
	for _, cf := range testkit.LoadCases[pqCase](t, "vecindex/pq") {
		c := cf.Data
		t.Run(c.ID, func(t *testing.T) {
			data := c.Vectors
			if c.Generator != nil {
				data, _ = c.Generator.generate()
			}
			cfg := PQConfig{M: c.PQ.M, Centroids: c.PQ.Centroids, Iterations: c.PQ.Iterations, TrainSample: c.PQ.TrainSample}
			pq, err := TrainPQ(data, cfg, core.NewSplitMix64(c.PQSeed))
			if err != nil {
				t.Fatal(err)
			}
			dim := len(data[0])
			mean := make([]float64, dim)
			for _, v := range data {
				for j, x := range v {
					mean[j] += float64(x) / float64(len(data))
				}
			}
			var errSum, varSum float64
			for _, v := range data {
				code, err := pq.Encode(v)
				if err != nil {
					t.Fatal(err)
				}
				dec, err := pq.Decode(code)
				if err != nil {
					t.Fatal(err)
				}
				for j := range v {
					d := float64(v[j]) - float64(dec[j])
					errSum += d * d
					m := float64(v[j]) - mean[j]
					varSum += m * m
				}
			}
			rel := errSum / max(varSum, 1e-300)
			t.Logf("relative MSE %.5f (bound %v)", rel, c.MaxRelativeMSE)
			if rel > c.MaxRelativeMSE {
				t.Fatalf("relative MSE %.5f exceeds %v", rel, c.MaxRelativeMSE)
			}
		})
	}
}

type rrfCase struct {
	ID        string        `json:"id"`
	RRFK      int           `json:"rrf_k"`
	Lists     [][]core.UUID `json:"lists"`
	Tolerance float64       `json:"tolerance"`
	Expect    []struct {
		ID    core.UUID `json:"id"`
		Score float64   `json:"score"`
	} `json:"expect"`
}

// Reciprocal rank fusion reproduces the spec 22.6 example.
func TestVecConformance_RRF(t *testing.T) {
	for _, cf := range testkit.LoadCases[rrfCase](t, "vecindex/rrf") {
		c := cf.Data
		t.Run(c.ID, func(t *testing.T) {
			got := RRF(c.RRFK, c.Lists...)
			if len(got) != len(c.Expect) {
				t.Fatalf("got %d results %+v, want %d", len(got), got, len(c.Expect))
			}
			for i, w := range c.Expect {
				if got[i].ID != w.ID || math.Abs(float64(got[i].Score)-w.Score) > c.Tolerance {
					t.Fatalf("rank %d: got %s %.6f, want %s %.5f", i+1, got[i].ID, got[i].Score, w.ID, w.Score)
				}
			}
		})
	}
}

type simhashCase struct {
	ID      string `json:"id"`
	FNV1a64 []struct {
		S      string `json:"s"`
		Expect hex64  `json:"expect"`
	} `json:"fnv1a64"`
	Tokens []struct {
		Text   string   `json:"text"`
		Expect []string `json:"expect"`
	} `json:"tokens"`
	Shingles []struct {
		Tokens []string `json:"tokens"`
		W      int      `json:"w"`
		Expect []string `json:"expect"`
	} `json:"shingles"`
	CharNGrams []struct {
		S      string   `json:"s"`
		N      int      `json:"n"`
		Expect []string `json:"expect"`
	} `json:"char_ngrams"`
	SimHash []struct {
		Text   string `json:"text"`
		Expect hex64  `json:"expect"`
	} `json:"simhash"`
	Pairs []struct {
		A             string `json:"a"`
		B             string `json:"b"`
		Hamming       int    `json:"hamming"`
		NearDuplicate bool   `json:"near_duplicate"`
	} `json:"pairs"`
	Index struct {
		Entries []struct {
			ID   core.UUID `json:"id"`
			Hash hex64     `json:"hash"`
		} `json:"entries"`
		Query  hex64 `json:"query"`
		Expect []struct {
			ID       core.UUID `json:"id"`
			Distance int       `json:"distance"`
		} `json:"expect"`
	} `json:"index"`
}

func equalStrings(a, b []string) bool { return len(a) == len(b) && (len(a) == 0 || slices.Equal(a, b)) }

// SimHash, shingling and multi-index hashing vectors (spec 22.8).
func TestVecConformance_SimHash(t *testing.T) {
	for _, cf := range testkit.LoadCases[simhashCase](t, "vecindex/simhash") {
		c := cf.Data
		t.Run(c.ID, func(t *testing.T) {
			for _, v := range c.FNV1a64 {
				if got := FNV1a64(v.S); got != uint64(v.Expect) {
					t.Errorf("FNV1a64(%q) = %016x, want %016x", v.S, got, uint64(v.Expect))
				}
			}
			for _, v := range c.Tokens {
				if got := Tokens(v.Text); !equalStrings(got, v.Expect) {
					t.Errorf("Tokens(%q) = %q, want %q", v.Text, got, v.Expect)
				}
			}
			for _, v := range c.Shingles {
				if got := Shingles(v.Tokens, v.W); !equalStrings(got, v.Expect) {
					t.Errorf("Shingles(%q, %d) = %q, want %q", v.Tokens, v.W, got, v.Expect)
				}
			}
			for _, v := range c.CharNGrams {
				if got := CharNGrams(v.S, v.N); !equalStrings(got, v.Expect) {
					t.Errorf("CharNGrams(%q, %d) = %q, want %q", v.S, v.N, got, v.Expect)
				}
			}
			for _, v := range c.SimHash {
				if got := SimHashText(v.Text); got != uint64(v.Expect) {
					t.Errorf("SimHashText(%q) = %016x, want %016x", v.Text, got, uint64(v.Expect))
				}
			}
			for _, p := range c.Pairs {
				d := Hamming(SimHashText(p.A), SimHashText(p.B))
				if d != p.Hamming || (d <= NearDuplicateDistance) != p.NearDuplicate {
					t.Errorf("pair %q / %q: distance %d, want %d (near %v)", p.A, p.B, d, p.Hamming, p.NearDuplicate)
				}
			}
			x := NewSimHashIndex()
			for _, e := range c.Index.Entries {
				x.Add(e.ID, uint64(e.Hash))
			}
			got := x.Near(uint64(c.Index.Query))
			if len(got) != len(c.Index.Expect) {
				t.Fatalf("Near = %+v, want %+v", got, c.Index.Expect)
			}
			for i, w := range c.Index.Expect {
				if got[i].ID != w.ID || got[i].Distance != w.Distance {
					t.Fatalf("Near[%d] = %+v, want %+v", i, got[i], w)
				}
			}
		})
	}
}

type minhashCase struct {
	ID                 string              `json:"id"`
	Seed               hex64               `json:"seed"`
	Permutations       int                 `json:"permutations"`
	Bands              int                 `json:"bands"`
	Rows               int                 `json:"rows"`
	CoefficientsPrefix map[string][]hex64  `json:"coefficients_prefix"`
	Sets               map[string][]string `json:"sets"`
	SignaturePrefix    map[string][]hex64  `json:"signature_prefix"`
	Pairs              []struct {
		A            string  `json:"a"`
		B            string  `json:"b"`
		Jaccard      float64 `json:"jaccard"`
		Estimate     float64 `json:"estimate"`
		LSHCandidate bool    `json:"lsh_candidate"`
	} `json:"pairs"`
	CollisionProbability []struct {
		S float64 `json:"s"`
		P float64 `json:"p"`
	} `json:"collision_probability"`
	Tolerance float64 `json:"tolerance"`
}

// coefEqual reports whether two hashers have the same coefficients.
func coefEqual(x, y *MinHasher) bool {
	xa, xb := x.Coefficients()
	ya, yb := y.Coefficients()
	return slices.Equal(xa, ya) && slices.Equal(xb, yb)
}

// MinHash signatures, Jaccard estimates, LSH banding and the collision
// probability 1 - (1 - s^4)^32 (spec 22.8).
func TestVecConformance_MinHash(t *testing.T) {
	for _, cf := range testkit.LoadCases[minhashCase](t, "vecindex/minhash") {
		c := cf.Data
		t.Run(c.ID, func(t *testing.T) {
			m, err := NewMinHasher(c.Permutations, uint64(c.Seed))
			if err != nil {
				t.Fatal(err)
			}
			if uint64(c.Seed) == DefaultMinHashSeed && c.Permutations == DefaultMinHashPermutations {
				if d := DefaultMinHasher(); !coefEqual(d, m) {
					t.Fatal("DefaultMinHasher differs from NewMinHasher(128, DefaultMinHashSeed)")
				}
			}
			ma, mb := m.Coefficients()
			for i, a := range c.CoefficientsPrefix["a"] {
				if ma[i] != uint64(a) {
					t.Errorf("a[%d] = %016x, want %016x", i, ma[i], uint64(a))
				}
			}
			for i, b := range c.CoefficientsPrefix["b"] {
				if mb[i] != uint64(b) {
					t.Errorf("b[%d] = %016x, want %016x", i, mb[i], uint64(b))
				}
			}
			sigs := map[string][]uint64{}
			for name, set := range c.Sets {
				sigs[name] = m.Signature(set)
			}
			for name, prefix := range c.SignaturePrefix {
				for i, v := range prefix {
					if sigs[name][i] != uint64(v) {
						t.Errorf("signature %s[%d] = %016x, want %016x", name, i, sigs[name][i], uint64(v))
					}
				}
			}
			for _, p := range c.Pairs {
				if j := Jaccard(c.Sets[p.A], c.Sets[p.B]); math.Abs(j-p.Jaccard) > c.Tolerance {
					t.Errorf("Jaccard(%s, %s) = %v, want %v", p.A, p.B, j, p.Jaccard)
				}
				if e := EstimateJaccard(sigs[p.A], sigs[p.B]); math.Abs(e-p.Estimate) > c.Tolerance {
					t.Errorf("EstimateJaccard(%s, %s) = %v, want %v", p.A, p.B, e, p.Estimate)
				}
				l, err := NewLSH(c.Bands, c.Rows)
				if err != nil {
					t.Fatal(err)
				}
				target := idOf(1)
				if err := l.Add(target, sigs[p.B]); err != nil {
					t.Fatal(err)
				}
				cands, err := l.Candidates(sigs[p.A])
				if err != nil {
					t.Fatal(err)
				}
				if got := slices.Contains(cands, target); got != p.LSHCandidate {
					t.Errorf("LSH candidate(%s, %s) = %v, want %v", p.A, p.B, got, p.LSHCandidate)
				}
			}
			for _, v := range c.CollisionProbability {
				if got := CollisionProbability(v.S, c.Bands, c.Rows); math.Abs(got-v.P) > c.Tolerance {
					t.Errorf("CollisionProbability(%v) = %.10f, want %.10f", v.S, got, v.P)
				}
			}
		})
	}
}
