package placement_test

import (
	"encoding/hex"
	"slices"
	"strconv"
	"testing"

	"github.com/genuinebnt/flowforge/placement"
	"github.com/genuinebnt/flowforge/testkit"
)

// goldenFile is the schema of conformance/placement/*.json.
type goldenFile struct {
	Description  string       `json:"description"`
	Algorithm    string       `json:"algorithm"` // "hash64", "rendezvous" or "consistent"
	VirtualNodes int          `json:"virtual_nodes,omitempty"`
	HashVectors  []hashVector `json:"hash_vectors,omitempty"`
	Sets         []goldenSet  `json:"sets,omitempty"`
}

type hashVector struct {
	KeyHex string `json:"key_hex"`
	Worker string `json:"worker"`
	Hash   string `json:"hash"` // 0x-prefixed, 16 hex digits
}

type goldenSet struct {
	Workers []placement.WorkerID `json:"workers"`
	Cases   []goldenCase         `json:"cases"`
}

type goldenCase struct {
	Key     string               `json:"key,omitempty"`     // UTF-8 key
	KeyHex  string               `json:"key_hex,omitempty"` // binary key; used when key is empty
	Owner   placement.WorkerID   `json:"owner,omitempty"`   // empty when there is none
	Ranking []placement.WorkerID `json:"ranking"`
}

func (c goldenCase) key(t *testing.T) []byte {
	if c.Key != "" {
		return []byte(c.Key)
	}
	b, err := hex.DecodeString(c.KeyHex)
	if err != nil {
		t.Fatalf("bad key_hex %q: %v", c.KeyHex, err)
	}
	return b
}

// TestPlcConformance compares the implementation with the golden data. It
// never rewrites the data.
func TestPlcConformance(t *testing.T) {
	files := testkit.LoadCases[goldenFile](t, "placement")
	seen := map[string]bool{}
	for _, f := range files {
		seen[f.Data.Algorithm] = true
		t.Run(f.Name, func(t *testing.T) {
			var p placement.Placement
			switch f.Data.Algorithm {
			case "hash64":
				for _, v := range f.Data.HashVectors {
					k, err := hex.DecodeString(v.KeyHex)
					if err != nil {
						t.Fatal(err)
					}
					want, err := strconv.ParseUint(v.Hash, 0, 64)
					if err != nil {
						t.Fatal(err)
					}
					if got := placement.Hash64(k, placement.WorkerID(v.Worker)); got != want {
						t.Errorf("Hash64(%s, %q) = %#016x, want %s", v.KeyHex, v.Worker, got, v.Hash)
					}
				}
				return
			default:
				// Through the selection constructor, so every selectable
				// scheme runs the golden data.
				var err error
				if p, err = placement.New(placement.Scheme(f.Data.Algorithm), placement.ConsistentConfig{VirtualNodes: f.Data.VirtualNodes}); err != nil {
					t.Fatal(err)
				}
			}
			for si, s := range f.Data.Sets {
				for _, c := range s.Cases {
					k := c.key(t)
					owner, _ := p.Owner(k, s.Workers)
					if owner != c.Owner {
						t.Errorf("set %d key %q: Owner = %q, want %q", si, k, owner, c.Owner)
					}
					if r := p.Ranking(k, s.Workers); !slices.Equal(r, c.Ranking) {
						t.Errorf("set %d key %q: Ranking = %v, want %v", si, k, r, c.Ranking)
					}
				}
			}
		})
	}
	algs := []string{"hash64"}
	for _, s := range placement.Schemes() {
		algs = append(algs, string(s))
	}
	for _, alg := range algs {
		if !seen[alg] {
			t.Errorf("no conformance file for %s", alg)
		}
	}
}
