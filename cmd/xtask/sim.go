package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/genuinebnt/flowforge/sim"
)

// simCommand implements `xtask sim` (spec 25.10 `cargo xtask sim
// --profile chaos --seeds 200`): it runs the deterministic simulation for
// seeds 1..n (or one seed) under the given fault profiles and workloads,
// checks the invariants of spec 25.7 on every history, and fails with
// the list of failing seeds and how to reproduce each.
func simCommand(args []string) error {
	fs := flag.NewFlagSet("sim", flag.ContinueOnError)
	profile := fs.String("profile", sim.ProfileChaos, "fault profile, or all ("+strings.Join(sim.ProfileNames, ", ")+")")
	seeds := fs.Int("seeds", 200, "number of seeds per profile and workload (seeds first..first+n-1)")
	first := fs.Uint64("first", 1, "first seed")
	seed := fs.Uint64("seed", 0, "run only this seed")
	workload := fs.String("workload", sim.WorkloadMixed, "workload, or all ("+strings.Join(sim.WorkloadNames, ", ")+")")
	workers := fs.Int("workers", 0, "worker processes per cluster (default 3)")
	verbose := fs.Bool("v", false, "print every scenario")
	if err := fs.Parse(args); err != nil {
		return err
	}
	profiles := []string{*profile}
	if *profile == "all" {
		profiles = sim.ProfileNames
	}
	workloads := []string{*workload}
	if *workload == "all" {
		workloads = sim.WorkloadNames
	}
	var list []uint64
	if *seed != 0 {
		list = []uint64{*seed}
	} else {
		for i := range uint64(max(*seeds, 0)) {
			list = append(list, *first+i)
		}
	}
	type job struct{ sc sim.Scenario }
	var jobs []job
	for _, p := range profiles {
		if _, err := sim.Profile(p); err != nil {
			return err
		}
		for _, w := range workloads {
			for _, s := range list {
				jobs = append(jobs, job{sim.Scenario{Seed: s, Profile: p, Workload: w, Workers: *workers}})
			}
		}
	}
	reports := make([]sim.Report, len(jobs))
	var wg sync.WaitGroup
	next := make(chan int)
	for range runtime.GOMAXPROCS(0) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				reports[i] = sim.Run(jobs[i].sc)
			}
		}()
	}
	for i := range jobs {
		next <- i
	}
	close(next)
	wg.Wait()

	type key struct{ p, w string }
	total := map[key]int{}
	bad := map[key]int{}
	var failed []sim.Report
	for _, r := range reports {
		k := key{r.Scenario.Profile, r.Scenario.Workload}
		total[k]++
		if *verbose {
			fmt.Println(r.Summary())
		}
		if !r.OK() {
			bad[k]++
			failed = append(failed, r)
		}
	}
	keys := make([]key, 0, len(total))
	for k := range total {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b key) int { return strings.Compare(a.p+"/"+a.w, b.p+"/"+b.w) })
	for _, k := range keys {
		fmt.Printf("sim profile=%-11s workload=%-11s seeds=%-5d violations in %d\n", k.p, k.w, total[k], bad[k])
	}
	if len(failed) == 0 {
		return nil
	}
	fmt.Fprintf(os.Stderr, "\n%d failing scenarios:\n", len(failed))
	for _, r := range failed {
		fmt.Fprintln(os.Stderr, r.Summary())
		for i, v := range r.Violations {
			if i == 10 {
				fmt.Fprintf(os.Stderr, "    ... %d more\n", len(r.Violations)-10)
				break
			}
			fmt.Fprintln(os.Stderr, "    "+v.String())
		}
		if r.Err != nil {
			fmt.Fprintln(os.Stderr, "    "+r.Err.Error())
		}
		fmt.Fprintln(os.Stderr, "  reproduce: "+strings.ReplaceAll(r.Repro(), "\n", "\n             "))
		fmt.Fprintf(os.Stderr, "  regress entry: {\"seed\": %d, \"profile\": %q, \"workload\": %q, \"note\": \"...\"}\n",
			r.Scenario.Seed, r.Scenario.Profile, r.Scenario.Workload)
	}
	seedsList := make([]string, 0, len(failed))
	for _, r := range failed {
		seedsList = append(seedsList, fmt.Sprintf("%s/%s/%d", r.Scenario.Profile, r.Scenario.Workload, r.Scenario.Seed))
	}
	return fmt.Errorf("sim: %d of %d scenarios violate invariants: %s", len(failed), len(reports), strings.Join(seedsList, " "))
}
