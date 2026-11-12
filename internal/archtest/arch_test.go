// Package archtest enforces the dependency rules of spec 24.3 and 25.2
// ("arch/deps"): packages import only lower layers, pure packages never pull
// in adapters or heavy third-party modules, test-support packages are
// imported only by tests, and no package in layers 1-5 reads the system
// clock or a global random source.
package archtest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const modPath = "github.com/genuinebnt/flowforge"

// unit describes one architectural unit: a directory prefix (relative to
// the module root), its layer and the third-party module prefixes its
// non-test code may import.
type unit struct {
	prefix     string
	layer      int
	thirdParty []string
	// testSupport packages (testkit, storetest) may only be imported from
	// _test.go files and other test-support packages.
	testSupport bool
	// adapter packages may import heavy dependencies; pure packages must
	// never import an adapter.
	adapter bool
}

// layerAdapters is the layer number given to adapters and generated
// contracts: above every pure layer, below services.
const layerAdapters = 5

// units is ordered from most to least specific prefix.
var units = []unit{
	{prefix: "core/netnorm", layer: 0, thirdParty: []string{"golang.org/x/net"}},
	{prefix: "core", layer: 0},
	{prefix: "testkit", layer: 0, testSupport: true},
	{prefix: "internal/archtest", layer: 0, testSupport: true},

	{prefix: "scope", layer: 1},
	{prefix: "oam", layer: 1},
	{prefix: "limiter/redislimiter", layer: layerAdapters, adapter: true, thirdParty: []string{"github.com/redis/go-redis"}},
	{prefix: "limiter", layer: 1},
	{prefix: "csr", layer: 1},
	{prefix: "placement", layer: 1},
	{prefix: "auth", layer: 1, thirdParty: []string{"golang.org/x/crypto"}},

	{prefix: "dsl", layer: 2},
	{prefix: "bytecode", layer: 2},
	{prefix: "vecindex", layer: 2},
	{prefix: "alerts", layer: 2},

	{prefix: "vm", layer: 3, thirdParty: []string{"github.com/fxamacker/cbor", "github.com/x448/float16"}},
	{prefix: "eventstore/storetest", layer: 3, testSupport: true},
	{prefix: "eventstore/pgstore", layer: layerAdapters, adapter: true, thirdParty: []string{"github.com/jackc/"}},
	{prefix: "eventstore", layer: 3},

	{prefix: "compiler", layer: 4},
	{prefix: "engine/enginetest", layer: 4, testSupport: true},
	{prefix: "engine/pgrepo", layer: layerAdapters, adapter: true, thirdParty: []string{"github.com/jackc/"}},
	{prefix: "engine", layer: 4},
	{prefix: "agent", layer: 4},
	{prefix: "graphstore/pg", layer: layerAdapters, adapter: true, thirdParty: []string{"github.com/jackc/"}},
	{prefix: "graphstore", layer: 4},

	{prefix: "migrations", layer: layerAdapters, adapter: true, thirdParty: []string{"github.com/jackc/"}},
	{prefix: "gen", layer: layerAdapters, adapter: true, thirdParty: []string{"google.golang.org/"}},
	{prefix: "sim", layer: 5, testSupport: false},

	{prefix: "services", layer: 6, thirdParty: []string{""}},
	// cmd mains sit above the services they start (cmd/<service> imports
	// services/..., cmd/flowforge imports services/devstack).
	{prefix: "cmd", layer: 7, thirdParty: []string{""}},
}

// pureLayers are the layers whose non-test code must be deterministic.
func pureLayer(u unit) bool { return !u.adapter && !u.testSupport && u.layer >= 1 && u.layer <= 5 }

func unitFor(rel string) (unit, bool) {
	for _, u := range units {
		if rel == u.prefix || strings.HasPrefix(rel, u.prefix+"/") {
			return u, true
		}
	}
	return unit{}, false
}

type pkg struct {
	ImportPath   string
	Dir          string
	GoFiles      []string
	Imports      []string
	TestImports  []string
	XTestImports []string
	Error        *struct{ Err string }
}

func listPackages(t *testing.T) []pkg {
	t.Helper()
	root := repoRoot(t)
	cmd := exec.Command("go", "list", "-e", "-json", "./...")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v\n%s", err, stderr.String())
	}
	var pkgs []pkg
	dec := json.NewDecoder(bytes.NewReader(out))
	for {
		var p pkg
		if err := dec.Decode(&p); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		pkgs = append(pkgs, p)
	}
	return pkgs
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func rel(importPath string) (string, bool) {
	if importPath == modPath {
		return "", true
	}
	r, ok := strings.CutPrefix(importPath, modPath+"/")
	return r, ok
}

func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

func allowedThirdParty(u unit, imp string) bool {
	for _, p := range u.thirdParty {
		if p == "" || strings.HasPrefix(imp, p) {
			return true
		}
	}
	return false
}

// TestArchDeps checks the layer rule, the third-party allowlist and the
// test-support rule for every package's non-test imports.
func TestArchDeps(t *testing.T) {
	for _, p := range listPackages(t) {
		r, ok := rel(p.ImportPath)
		if !ok {
			continue
		}
		u, ok := unitFor(r)
		if !ok {
			t.Errorf("%s: package is not assigned to a layer in internal/archtest", r)
			continue
		}
		for _, imp := range p.Imports {
			if isStdlib(imp) {
				continue
			}
			ir, internal := rel(imp)
			if !internal {
				if !allowedThirdParty(u, imp) {
					t.Errorf("%s (layer %d) imports third-party %s, which is not on its allowlist", r, u.layer, imp)
				}
				continue
			}
			iu, ok := unitFor(ir)
			if !ok {
				continue // reported on its own
			}
			if iu.prefix == u.prefix {
				continue // within one unit
			}
			if iu.testSupport && !u.testSupport {
				t.Errorf("%s imports test-support package %s from non-test code", r, ir)
			}
			if iu.adapter && !u.adapter && u.layer < 6 && !u.testSupport {
				t.Errorf("%s (pure) imports adapter %s", r, ir)
			}
			lower := iu.layer < u.layer
			if u.adapter {
				// Adapters sit beside each other above the pure layers.
				lower = iu.layer <= layerAdapters
			}
			if u.testSupport {
				// Test fixtures may use anything that is not a service.
				lower = iu.layer < 6
			}
			if !lower {
				t.Errorf("%s (layer %d) imports %s (layer %d): dependencies must point to lower layers", r, u.layer, ir, iu.layer)
			}
		}
	}
}

// TestArchTestImports checks that tests import test-support packages and
// lower layers only (never a service).
func TestArchTestImports(t *testing.T) {
	for _, p := range listPackages(t) {
		r, ok := rel(p.ImportPath)
		if !ok {
			continue
		}
		u, ok := unitFor(r)
		if !ok || u.layer >= 6 {
			continue
		}
		for _, imp := range slices.Concat(p.TestImports, p.XTestImports) {
			ir, internal := rel(imp)
			if !internal {
				continue
			}
			if iu, ok := unitFor(ir); ok && iu.layer >= 6 {
				t.Errorf("tests of %s import service/command package %s", r, ir)
			}
		}
	}
}

var forbidden = []struct {
	re   *regexp.Regexp
	what string
}{
	{regexp.MustCompile(`\btime\.(Now|Since|Until)\(`), "reads the wall clock (take a core.Clock or a time argument)"},
	{regexp.MustCompile(`"math/rand(/v2)?"`), "imports math/rand (take a core.Rng)"},
	{regexp.MustCompile(`\bos\.(Getenv|LookupEnv|Environ)\(`), "reads the environment (configuration arrives as a struct)"},
}

// TestArchNoAmbientEffects scans non-test sources of layers 1-5 for clock,
// randomness and environment access (spec 24.5, 25.2).
func TestArchNoAmbientEffects(t *testing.T) {
	for _, p := range listPackages(t) {
		r, ok := rel(p.ImportPath)
		if !ok {
			continue
		}
		u, ok := unitFor(r)
		if !ok || !pureLayer(u) {
			continue
		}
		for _, f := range p.GoFiles {
			path := filepath.Join(p.Dir, f)
			for _, hit := range scan(t, path) {
				t.Errorf("%s/%s:%d %s", r, f, hit.line, hit.what)
			}
		}
	}
}

type hit struct {
	line int
	what string
}

func scan(t *testing.T, path string) []hit {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var hits []hit
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		code, _, _ := strings.Cut(line, "//")
		for _, fb := range forbidden {
			if fb.re.MatchString(code) {
				hits = append(hits, hit{n, fmt.Sprintf("%s: %q", fb.what, strings.TrimSpace(line))})
			}
		}
	}
	return hits
}
