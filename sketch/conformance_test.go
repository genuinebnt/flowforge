package sketch

import (
	"fmt"
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
	if err != nil {
		return err
	}
	*h = hex64(v)
	return nil
}

func hexes(v []hex64) []uint64 {
	out := make([]uint64, len(v))
	for i, x := range v {
		out[i] = uint64(x)
	}
	return out
}

func cmpUUID(a, b core.UUID) int { return slices.Compare(a[:], b[:]) }

func uuidOf(n byte) core.UUID { return core.UUID{15: n} }

// sketchFile is the union of every conformance/sketch/*.json shape; each
// file fills the sections it has.
type sketchFile struct {
	ID string `json:"id"`
	// vecindex_compat.json
	Texts []struct {
		Text    string `json:"text"`
		Tokens  int    `json:"tokens"`
		SimHash hex64  `json:"simhash"`
		Char3   hex64  `json:"char3_simhash"`
	} `json:"texts"`
	Sets []struct {
		Name      string   `json:"name"`
		Elements  []string `json:"elements"`
		Signature []hex64  `json:"signature"`
	} `json:"sets"`
	Pairs []struct {
		A            string  `json:"a"`
		B            string  `json:"b"`
		Jaccard      float64 `json:"jaccard"`
		Estimate     float64 `json:"estimate"`
		LSHCandidate bool    `json:"lsh_candidate"`
	} `json:"pairs"`
	// tokens.json
	TokenHash []struct {
		ID     uint32 `json:"id"`
		Expect hex64  `json:"expect"`
	} `json:"token_hash"`
	TokenShingles []struct {
		IDs    []uint32 `json:"ids"`
		K      int      `json:"k"`
		Expect []hex64  `json:"expect"`
	} `json:"token_shingles"`
	WShingling []struct {
		IDs    []uint32 `json:"ids"`
		W      int      `json:"w"`
		Expect []hex64  `json:"expect"`
	} `json:"wshingling"`
	SimHashTokens []struct {
		IDs    []uint32 `json:"ids"`
		K      int      `json:"k"`
		Expect hex64    `json:"expect"`
	} `json:"simhash_tokens"`
	Weighted []struct {
		Tokens []struct {
			Token  uint32 `json:"token"`
			Weight int32  `json:"weight"`
		} `json:"tokens"`
		Expect hex64 `json:"expect"`
	} `json:"weighted"`
	BandKeys []struct {
		Sig    []hex64 `json:"sig"`
		Rows   int     `json:"rows"`
		Expect []hex64 `json:"expect"`
	} `json:"band_keys"`
	JaccardSorted []struct {
		A      []hex64 `json:"a"`
		B      []hex64 `json:"b"`
		Expect float64 `json:"expect"`
	} `json:"jaccard_sorted"`
}

// TestSketchConformance runs every file of conformance/sketch.
func TestSketchConformance(t *testing.T) {
	m := DefaultMinHasher()
	for _, cf := range testkit.LoadCases[sketchFile](t, "sketch") {
		c := cf.Data
		t.Run(c.ID, func(t *testing.T) {
			checkCompat(t, c, m)
			for _, v := range c.TokenHash {
				if got := TokenHash(v.ID); got != uint64(v.Expect) {
					t.Errorf("TokenHash(%d) = %016x, want %016x", v.ID, got, uint64(v.Expect))
				}
			}
			for _, v := range c.TokenShingles {
				if got := TokenShingles(v.IDs, v.K); !equalU64(got, hexes(v.Expect)) {
					t.Errorf("TokenShingles(%v, %d) = %x, want %x", v.IDs, v.K, got, hexes(v.Expect))
				}
			}
			for _, v := range c.WShingling {
				if got := WShingling(v.IDs, v.W); !equalU64(got, hexes(v.Expect)) {
					t.Errorf("WShingling(%v, %d) = %x, want %x", v.IDs, v.W, got, hexes(v.Expect))
				}
			}
			for _, v := range c.SimHashTokens {
				if got := SimHashTokens(v.IDs, v.K); got != uint64(v.Expect) {
					t.Errorf("SimHashTokens(%v, %d) = %016x, want %016x", v.IDs, v.K, got, uint64(v.Expect))
				}
			}
			for _, v := range c.Weighted {
				ws := make([]WeightedToken, len(v.Tokens))
				for i, x := range v.Tokens {
					ws[i] = WeightedToken{Token: x.Token, Weight: x.Weight}
				}
				if got := WeightedSimHashTokens(ws); got != uint64(v.Expect) {
					t.Errorf("WeightedSimHashTokens(%v) = %016x, want %016x", ws, got, uint64(v.Expect))
				}
			}
			for _, v := range c.BandKeys {
				sig := hexes(v.Sig)
				for b, want := range v.Expect {
					if got := BandKey(sig, b, v.Rows); got != uint64(want) {
						t.Errorf("BandKey(band %d, rows %d) = %016x, want %016x", b, v.Rows, got, uint64(want))
					}
				}
			}
			for _, v := range c.JaccardSorted {
				if got := JaccardSorted(hexes(v.A), hexes(v.B)); math.Abs(got-v.Expect) > 1e-12 {
					t.Errorf("JaccardSorted = %v, want %v", got, v.Expect)
				}
			}
		})
	}
}

func equalU64(a, b []uint64) bool { return len(a) == len(b) && (len(a) == 0 || slices.Equal(a, b)) }

func checkCompat(t *testing.T, c sketchFile, m *MinHasher) {
	t.Helper()
	for _, v := range c.Texts {
		if got := len(Tokens(v.Text)); got != v.Tokens {
			t.Errorf("len(Tokens(%q)) = %d, want %d", v.Text, got, v.Tokens)
		}
		if got := SimHashText(v.Text); got != uint64(v.SimHash) {
			t.Errorf("SimHashText(%q) = %016x, want %016x", v.Text, got, uint64(v.SimHash))
		}
		if got := SimHash(CharNGrams(v.Text, 3)); got != uint64(v.Char3) {
			t.Errorf("SimHash(CharNGrams(%q, 3)) = %016x, want %016x", v.Text, got, uint64(v.Char3))
		}
	}
	sigs := map[string][]uint64{}
	sets := map[string][]string{}
	for _, s := range c.Sets {
		sigs[s.Name] = m.Signature(s.Elements)
		sets[s.Name] = s.Elements
		if !slices.Equal(sigs[s.Name], hexes(s.Signature)) {
			t.Errorf("Signature(%s) differs from the pre-refactor signature", s.Name)
		}
	}
	for _, p := range c.Pairs {
		if j := Jaccard(sets[p.A], sets[p.B]); j != p.Jaccard {
			t.Errorf("Jaccard(%s, %s) = %v, want %v", p.A, p.B, j, p.Jaccard)
		}
		if e := EstimateJaccard(sigs[p.A], sigs[p.B]); e != p.Estimate {
			t.Errorf("EstimateJaccard(%s, %s) = %v, want %v", p.A, p.B, e, p.Estimate)
		}
		l, err := NewLSH[core.UUID](DefaultLSHBands, DefaultLSHRows, cmpUUID)
		if err != nil {
			t.Fatal(err)
		}
		if err := l.Add(uuidOf(1), sigs[p.B]); err != nil {
			t.Fatal(err)
		}
		cands, err := l.Candidates(sigs[p.A])
		if err != nil {
			t.Fatal(err)
		}
		if got := len(cands) == 1; got != p.LSHCandidate {
			t.Errorf("LSH candidate(%s, %s) = %v, want %v", p.A, p.B, got, p.LSHCandidate)
		}
	}
}

// vecindex's own SimHash and MinHash vectors, run against sketch directly.
type vecSimhashFile struct {
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

type vecMinhashFile struct {
	ID                 string              `json:"id"`
	Seed               hex64               `json:"seed"`
	Permutations       int                 `json:"permutations"`
	CoefficientsPrefix map[string][]hex64  `json:"coefficients_prefix"`
	Sets               map[string][]string `json:"sets"`
	SignaturePrefix    map[string][]hex64  `json:"signature_prefix"`
}

func equalStrings(a, b []string) bool { return len(a) == len(b) && (len(a) == 0 || slices.Equal(a, b)) }

// TestSketchConformance_Vecindex replays conformance/vecindex/{simhash,
// minhash} against sketch (the data predates the move).
func TestSketchConformance_Vecindex(t *testing.T) {
	for _, cf := range testkit.LoadCases[vecSimhashFile](t, "vecindex/simhash") {
		c := cf.Data
		for _, v := range c.FNV1a64 {
			if got := FNV1a64(v.S); got != uint64(v.Expect) {
				t.Errorf("FNV1a64(%q) = %016x", v.S, got)
			}
			if got := FNV1a64Bytes([]byte(v.S)); got != uint64(v.Expect) {
				t.Errorf("FNV1a64Bytes(%q) = %016x", v.S, got)
			}
		}
		for _, v := range c.Tokens {
			if got := Tokens(v.Text); !equalStrings(got, v.Expect) {
				t.Errorf("Tokens(%q) = %q", v.Text, got)
			}
		}
		for _, v := range c.Shingles {
			if got := Shingles(v.Tokens, v.W); !equalStrings(got, v.Expect) {
				t.Errorf("Shingles(%q, %d) = %q", v.Tokens, v.W, got)
			}
		}
		for _, v := range c.CharNGrams {
			if got := CharNGrams(v.S, v.N); !equalStrings(got, v.Expect) {
				t.Errorf("CharNGrams(%q, %d) = %q", v.S, v.N, got)
			}
		}
		for _, v := range c.SimHash {
			if got := SimHashText(v.Text); got != uint64(v.Expect) {
				t.Errorf("SimHashText(%q) = %016x", v.Text, got)
			}
		}
		x := NewSimHashIndex[core.UUID](cmpUUID)
		for _, e := range c.Index.Entries {
			x.Add(e.ID, uint64(e.Hash))
		}
		got := x.Near(uint64(c.Index.Query))
		want := fmt.Sprint(c.Index.Expect)
		if fmt.Sprint(got) != want {
			t.Errorf("Near = %v, want %v", got, want)
		}
	}
	for _, cf := range testkit.LoadCases[vecMinhashFile](t, "vecindex/minhash") {
		c := cf.Data
		m, err := NewMinHasher(c.Permutations, uint64(c.Seed))
		if err != nil {
			t.Fatal(err)
		}
		a, b := m.Coefficients()
		if !slices.Equal(a[:len(c.CoefficientsPrefix["a"])], hexes(c.CoefficientsPrefix["a"])) ||
			!slices.Equal(b[:len(c.CoefficientsPrefix["b"])], hexes(c.CoefficientsPrefix["b"])) {
			t.Errorf("coefficients differ: a=%x b=%x", a[:2], b[:1])
		}
		for name, prefix := range c.SignaturePrefix {
			sig := m.Signature(c.Sets[name])
			if !slices.Equal(sig[:len(prefix)], hexes(prefix)) {
				t.Errorf("signature %s prefix differs", name)
			}
		}
	}
}

