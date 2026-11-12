package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
)

// check is one line of the verify-module report.
type check struct {
	name   string
	ok     bool
	detail string
}

// verifyModule checks the mechanical items of the definition of done
// (spec 24.6) and prints a table. It returns an error if any item fails.
func verifyModule(mod *Module) error {
	var checks []check
	add := func(name string, ok bool, detail string) {
		checks = append(checks, check{name, ok, detail})
	}

	dir := mod.Dir
	_, err := os.Stat(dir)
	add("package directory", err == nil, "./"+dir)
	if err != nil {
		return report(mod, checks)
	}
	for _, f := range []string{"doc.go", "README.md"} {
		_, err := os.Stat(filepath.Join(dir, f))
		add(f, err == nil, "")
	}

	sources, err := testSources(dir)
	if err != nil {
		return err
	}
	var missing []string
	for _, id := range mod.Required {
		re := regexp.MustCompile(`func Test` + regexp.QuoteMeta(mod.TestPrefix+id) + `[_A-Z(]`)
		if !re.Match(sources) {
			missing = append(missing, mod.TestPrefix+id)
		}
	}
	add(fmt.Sprintf("required tests (%d)", len(mod.Required)), len(missing) == 0, strings.Join(missing, " "))

	for kind, names := range map[string][]string{"properties": mod.Properties, "fuzz targets": mod.Fuzz, "benchmarks": mod.Bench} {
		var miss []string
		for _, n := range names {
			if !regexp.MustCompile(`func ` + regexp.QuoteMeta(n) + `[_A-Za-z0-9]*\(`).Match(sources) {
				miss = append(miss, n)
			}
		}
		add(fmt.Sprintf("%s (%d)", kind, len(names)), len(miss) == 0, strings.Join(miss, " "))
	}

	var emptySuites []string
	for _, suite := range mod.Conformance {
		files, _ := filepath.Glob(filepath.Join("conformance", filepath.FromSlash(suite), "*"))
		if len(files) == 0 {
			emptySuites = append(emptySuites, suite)
		}
	}
	add(fmt.Sprintf("conformance suites (%d)", len(mod.Conformance)), len(emptySuites) == 0, strings.Join(emptySuites, " "))

	baselines, _ := filepath.Glob(filepath.Join("bench", "results", mod.Name, "*.json"))
	add("benchmark baseline", len(mod.Bench) == 0 || len(baselines) > 0, fmt.Sprintf("bench/results/%s (%d files)", mod.Name, len(baselines)))

	out, err := exec.Command("gofmt", "-l", dir).CombinedOutput()
	add("gofmt", err == nil && len(bytes.TrimSpace(out)) == 0, strings.TrimSpace(string(out)))

	pkgs := "./" + dir + "/..."
	out, err = exec.Command("go", "vet", pkgs).CombinedOutput()
	add("go vet", err == nil, firstLine(out))

	out, err = exec.Command("go", "test", "-count=1", pkgs).CombinedOutput()
	add("go test", err == nil, lastLine(out))

	out, err = exec.Command("go", "test", "-count=1", "./internal/archtest").CombinedOutput()
	add("arch/deps", err == nil, firstFailure(out))

	return report(mod, checks)
}

func report(mod *Module, checks []check) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(w, "verify-module %s (spec %s)\n", mod.Name, strings.Join(mod.Spec, ", "))
	failed := 0
	for _, c := range checks {
		mark := "ok"
		if !c.ok {
			mark = "MISSING"
			failed++
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\n", mark, c.name, c.detail)
	}
	w.Flush()
	if failed > 0 {
		return fmt.Errorf("%s: %d item(s) missing", mod.Name, failed)
	}
	return nil
}

// testSources concatenates every _test.go file below dir.
func testSources(dir string) ([]byte, error) {
	var buf bytes.Buffer
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, "_test.go") {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
		return nil
	})
	return buf.Bytes(), err
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func lastLine(b []byte) string {
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return lines[len(lines)-1]
}

func firstFailure(b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "arch_test.go") {
			return strings.TrimSpace(l)
		}
	}
	return lastLine(b)
}
