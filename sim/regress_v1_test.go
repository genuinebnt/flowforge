package sim_test

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

// TestRegress_G_OPS_05_CISeeds pins the simulation depth of spec 25.11:
// the CI workflow runs ff-sim with FF_SIM_SEEDS=200 and the nightly one
// with 10000, and seedsPerProfile honours the variable (the default of 20
// applies only to a local go test).
func TestRegress_G_OPS_05_CISeeds(t *testing.T) {
	root := testkit.RepoRoot(t)
	for file, want := range map[string]string{"ci.yml": "200", "nightly.yml": "10000"} {
		b, err := os.ReadFile(filepath.Join(root, ".github", "workflows", file))
		if err != nil {
			t.Fatal(err)
		}
		// The job that sets the variable must also run the sim tests.
		re := regexp.MustCompile(`(?s)FF_SIM_SEEDS:\s*"(\d+)".*?go test [^\n]*-run 'TestSim' \./sim`)
		m := re.FindSubmatch(b)
		if m == nil || string(m[1]) != want {
			t.Errorf("%s: no sim job with FF_SIM_SEEDS %s (found %q)", file, want, m)
		}
	}
	t.Setenv("FF_SIM_SEEDS", "7")
	if n := seedsPerProfile(); n != 7 {
		t.Errorf("seedsPerProfile with FF_SIM_SEEDS=7 = %d", n)
	}
}
