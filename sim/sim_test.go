package sim_test

import (
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/genuinebnt/flowforge/sim"
	"github.com/genuinebnt/flowforge/testkit"
)

var (
	flagSeed     = flag.Uint64("sim.seed", 0, "run only this seed (0: seeds 1..sim.seeds)")
	flagSeeds    = flag.Int("sim.seeds", 0, "seeds per profile (default FF_SIM_SEEDS or 20; CI uses 200, nightly 10000)")
	flagProfile  = flag.String("sim.profile", "", "run only this fault profile")
	flagWorkload = flag.String("sim.workload", "", "workload (default mixed)")
)

func seedsPerProfile() int {
	if *flagSeeds > 0 {
		return *flagSeeds
	}
	if s := os.Getenv("FF_SIM_SEEDS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	if testing.Short() {
		return 5
	}
	return 20
}

func report(t *testing.T, r sim.Report) {
	t.Helper()
	if r.OK() {
		return
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s\n", r.Summary())
	for i, v := range r.Violations {
		if i == 20 {
			fmt.Fprintf(&sb, "  ... %d more\n", len(r.Violations)-20)
			break
		}
		fmt.Fprintf(&sb, "  %s\n", v)
	}
	fmt.Fprintf(&sb, "reproduce with:\n  %s\n", strings.ReplaceAll(r.Repro(), "\n", "\n  "))
	fmt.Fprintf(&sb, "and add {\"seed\": %d, \"profile\": %q, \"workload\": %q, \"note\": \"...\"} to conformance/sim/regress_seeds.json",
		r.Scenario.Seed, r.Scenario.Profile, r.Scenario.Workload)
	t.Error(sb.String())
}

// TestSimProfiles runs every fault profile over a range of seeds and
// checks inv_01..inv_11 on each history (spec 25.7).
func TestSimProfiles(t *testing.T) {
	profiles := sim.ProfileNames
	if *flagProfile != "" {
		profiles = []string{*flagProfile}
	}
	n := seedsPerProfile()
	for _, p := range profiles {
		t.Run(p, func(t *testing.T) {
			t.Parallel()
			var fenced, crashes, dups atomic.Int64
			seeds := make([]uint64, 0, n)
			if *flagSeed != 0 {
				seeds = append(seeds, *flagSeed)
			} else {
				for i := 1; i <= n; i++ {
					seeds = append(seeds, uint64(i))
				}
			}
			for _, seed := range seeds {
				r := sim.Run(sim.Scenario{Seed: seed, Profile: p, Workload: *flagWorkload})
				if r.Err != nil {
					t.Fatalf("seed %d: %v", seed, r.Err)
				}
				fenced.Add(int64(r.History.Stats.FencedAppends))
				crashes.Add(int64(r.History.Stats.Crashes))
				dups.Add(int64(r.History.Stats.PubDuplicates))
				report(t, r)
			}
			t.Logf("%s: %d seeds, %d crashes, %d fenced appends, %d Pub/Sub duplicates", p, len(seeds), crashes.Load(), fenced.Load(), dups.Load())
		})
	}
}

// TestSimRegressSeeds replays every seed that once failed.
func TestSimRegressSeeds(t *testing.T) {
	for _, f := range testkit.LoadCases[[]sim.RegressSeed](t, "sim") {
		for _, s := range f.Data {
			t.Run(fmt.Sprintf("%s_%d_%s", s.Profile, s.Seed, s.Workload), func(t *testing.T) {
				r := sim.Run(s.Scenario())
				if r.Err != nil {
					t.Fatal(r.Err)
				}
				if !r.OK() {
					t.Logf("regression: %s", s.Note)
				}
				report(t, r)
			})
		}
	}
}

// TestSimDeterministic: the same scenario twice yields the same history.
func TestSimDeterministic(t *testing.T) {
	for _, p := range sim.ProfileNames {
		for _, w := range []string{sim.WorkloadMixed, sim.WorkloadSaga} {
			sc := sim.Scenario{Seed: 42, Profile: p, Workload: w}
			a, b := sim.Run(sc), sim.Run(sc)
			if a.Err != nil || b.Err != nil {
				t.Fatal(a.Err, b.Err)
			}
			if a.Hash != b.Hash {
				t.Errorf("%s/%s: history hash %s then %s", p, w, a.Hash[:12], b.Hash[:12])
			}
		}
	}
	if sim.Run(sim.Scenario{Seed: 1, Profile: sim.ProfileChaos}).Hash == sim.Run(sim.Scenario{Seed: 2, Profile: sim.ProfileChaos}).Hash {
		t.Error("different seeds produced the same history")
	}
}

// TestSimWorkloads runs each workload under chaos.
func TestSimWorkloads(t *testing.T) {
	n := max(seedsPerProfile()/4, 2)
	for _, w := range sim.WorkloadNames {
		t.Run(w, func(t *testing.T) {
			t.Parallel()
			for seed := uint64(1); seed <= uint64(n); seed++ {
				r := sim.Run(sim.Scenario{Seed: seed, Profile: sim.ProfileChaos, Workload: w})
				if r.Err != nil {
					t.Fatal(r.Err)
				}
				report(t, r)
			}
		})
	}
}

func TestSimUnknownNames(t *testing.T) {
	if r := sim.Run(sim.Scenario{Profile: "nope"}); r.Err == nil {
		t.Error("unknown profile accepted")
	}
	if r := sim.Run(sim.Scenario{Workload: "nope"}); r.Err == nil {
		t.Error("unknown workload accepted")
	}
}

// BenchmarkSim_Chaos measures scenarios per second under the chaos
// profile (mixed workload, 3 workers, 20 simulated minutes).
func BenchmarkSim_Chaos(b *testing.B) {
	b.ReportAllocs()
	seed := uint64(0)
	for b.Loop() {
		seed++
		if r := sim.Run(sim.Scenario{Seed: seed, Profile: sim.ProfileChaos}); r.Err != nil {
			b.Fatal(r.Err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "scenarios/s")
}
