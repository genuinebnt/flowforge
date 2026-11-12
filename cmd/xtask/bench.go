package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// BenchFile is the result format of spec 25.8: one file per run under
// bench/results/<module>/<date>-<commit>.json.
type BenchFile struct {
	Module  string        `json:"module"`
	Date    string        `json:"date"`
	Commit  string        `json:"commit"`
	Env     BenchEnv      `json:"env"`
	Results []BenchResult `json:"results"`
}

// BenchEnv records the machine a benchmark ran on.
type BenchEnv struct {
	CPU       string `json:"cpu"`
	Cores     int    `json:"cores"`
	MemoryKB  int64  `json:"memory_kb"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Kernel    string `json:"kernel"`
	GoVersion string `json:"go_version"`
	BenchTime string `json:"benchtime"`
}

// BenchResult is one benchmark line.
type BenchResult struct {
	BenchID    string             `json:"bench_id"`
	Package    string             `json:"package"`
	Iterations int64              `json:"iterations"`
	Metrics    map[string]float64 `json:"metrics"` // unit -> value, e.g. "ns/op"
}

var benchLine = regexp.MustCompile(`^(Benchmark\S+?)(-\d+)?\s+(\d+)\s+(.*)$`)

func benchModule(mod *Module, benchtime string) error {
	cmd := exec.Command("go", "test", "-run=^$", "-bench=.", "-benchmem", "-count=1", "-benchtime="+benchtime, "./"+mod.Dir+"/...")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = os.Stderr
	runErr := cmd.Run()
	os.Stdout.Write(out.Bytes())

	file := BenchFile{
		Module: mod.Name,
		Date:   time.Now().UTC().Format("2006-01-02"),
		Commit: gitCommit(),
		Env:    benchEnv(benchtime),
	}
	pkg := ""
	sc := bufio.NewScanner(&out)
	for sc.Scan() {
		line := sc.Text()
		if p, ok := strings.CutPrefix(line, "pkg: "); ok {
			pkg = p
			continue
		}
		m := benchLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		iters, _ := strconv.ParseInt(m[3], 10, 64)
		res := BenchResult{BenchID: m[1], Package: pkg, Iterations: iters, Metrics: map[string]float64{}}
		fields := strings.Fields(m[4])
		for i := 0; i+1 < len(fields); i += 2 {
			if v, err := strconv.ParseFloat(fields[i], 64); err == nil {
				res.Metrics[fields[i+1]] = v
			}
		}
		file.Results = append(file.Results, res)
	}
	if runErr != nil {
		return fmt.Errorf("benchmarks failed: %w", runErr)
	}
	if len(file.Results) == 0 {
		return fmt.Errorf("no benchmark results for %s", mod.Name)
	}
	dir := filepath.Join("bench", "results", mod.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, file.Date+"-"+file.Commit+".json")
	b, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(b, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	return nil
}

func benchCompare(aPath, bPath string) error {
	load := func(p string) (*BenchFile, error) {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var f BenchFile
		return &f, json.Unmarshal(b, &f)
	}
	a, err := load(aPath)
	if err != nil {
		return err
	}
	b, err := load(bPath)
	if err != nil {
		return err
	}
	index := map[string]BenchResult{}
	for _, r := range a.Results {
		index[r.BenchID] = r
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "benchmark\tmetric\tbaseline\tnew\tdelta\t")
	for _, r := range b.Results {
		base, ok := index[r.BenchID]
		if !ok {
			continue
		}
		units := make([]string, 0, len(r.Metrics))
		for u := range r.Metrics {
			units = append(units, u)
		}
		sort.Strings(units)
		for _, u := range units {
			old, ok := base.Metrics[u]
			if !ok {
				continue
			}
			delta := "n/a"
			if old != 0 {
				delta = fmt.Sprintf("%+.1f%%", (r.Metrics[u]-old)/old*100)
			}
			fmt.Fprintf(w, "%s\t%s\t%.4g\t%.4g\t%s\t\n", r.BenchID, u, old, r.Metrics[u], delta)
		}
	}
	return w.Flush()
}

func gitCommit() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func benchEnv(benchtime string) BenchEnv {
	env := BenchEnv{
		Cores:     runtime.NumCPU(),
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		GoVersion: runtime.Version(),
		BenchTime: benchtime,
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, l := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(l, "model name"); ok {
				env.CPU = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), ":"))
				break
			}
		}
	}
	if b, err := os.ReadFile("/proc/meminfo"); err == nil {
		fields := strings.Fields(strings.SplitN(string(b), "\n", 2)[0])
		if len(fields) >= 2 {
			env.MemoryKB, _ = strconv.ParseInt(fields[1], 10, 64)
		}
	}
	if out, err := exec.Command("uname", "-r").Output(); err == nil {
		env.Kernel = strings.TrimSpace(string(out))
	}
	return env
}

var fuzzFunc = regexp.MustCompile(`(?m)^func (Fuzz\w+)\(`)

func fuzzSmoke(man *Manifest, names []string, fuzztime string) error {
	var failed []string
	for _, n := range names {
		mod, ok := man.Modules[n]
		if !ok {
			return fmt.Errorf("unknown module %q", n)
		}
		err := filepath.WalkDir(mod.Dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, "_test.go") {
				return err
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, m := range fuzzFunc.FindAllSubmatch(src, -1) {
				target := string(m[1])
				pkg := "./" + filepath.Dir(path)
				fmt.Printf("== %s %s (%s)\n", pkg, target, fuzztime)
				if err := goCmd("test", "-run=^$", "-fuzz=^"+target+"$", "-fuzztime="+fuzztime, pkg); err != nil {
					failed = append(failed, pkg+"."+target)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("fuzz findings in: %s", strings.Join(failed, ", "))
	}
	return nil
}
