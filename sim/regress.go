package sim

import (
	"encoding/json"
	"io"
)

// RegressSeed is one entry of conformance/sim/regress_seeds.json: a
// scenario that once failed (spec 25.7).
type RegressSeed struct {
	Seed     uint64 `json:"seed"`
	Profile  string `json:"profile"`
	Workload string `json:"workload"`
	Note     string `json:"note"`
}

// Scenario returns the seed's scenario with default sizes.
func (r RegressSeed) Scenario() Scenario {
	return Scenario{Seed: r.Seed, Profile: r.Profile, Workload: r.Workload}
}

// ReadRegressSeeds decodes a regress_seeds.json document.
func ReadRegressSeeds(r io.Reader) ([]RegressSeed, error) {
	var out []RegressSeed
	err := json.NewDecoder(r).Decode(&out)
	return out, err
}
