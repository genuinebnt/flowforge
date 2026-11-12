package compiler_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/genuinebnt/flowforge/compiler"
	"github.com/genuinebnt/flowforge/testkit"
	"github.com/genuinebnt/flowforge/vm"
)

// update rewrites the conformance/vm goldens:
//
//	go test ./compiler -run 'TestVMConformance' -update
var update = flag.Bool("update", false, "rewrite conformance/vm goldens")

// vmCase is one directory of conformance/vm/run or conformance/vm/replay.
type vmCase struct {
	name string
	dir  string
	src  string
	sc   script
}

func loadVMCases(t *testing.T, suite string) []vmCase {
	t.Helper()
	root := testkit.ConformanceDir(t, "vm/"+suite)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []vmCase
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		src, err := os.ReadFile(filepath.Join(dir, "case.ff"))
		if err != nil {
			t.Fatal(err)
		}
		c := vmCase{name: e.Name(), dir: dir, src: string(src)}
		if raw, err := os.ReadFile(filepath.Join(dir, "effects.json")); err == nil {
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&c.sc); err != nil {
				t.Fatalf("%s/effects.json: %v", e.Name(), err)
			}
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	if len(out) == 0 {
		t.Fatalf("conformance/vm/%s has no cases", suite)
	}
	return out
}

// compileCase compiles a case; failure to compile is a test failure.
func compileCase(t *testing.T, c vmCase) *compiler.Result {
	t.Helper()
	res, diags := compiler.Compile(c.src, compiler.Options{FileName: "case.ff", Workflow: c.sc.Workflow, Tools: c.sc.registry()})
	if res == nil {
		t.Fatalf("compile: %v", diags)
	}
	return res
}

func caseInputs(t *testing.T, res *compiler.Result, c vmCase) []vm.Value {
	t.Helper()
	raw := c.sc.Inputs
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	args, err := vm.DecodeInputs(res.Verified, raw)
	if err != nil {
		t.Fatalf("inputs: %v", err)
	}
	return args
}

// runCaseVM runs a case on the VM (with its fuel settings) and renders it.
func runCaseVM(t *testing.T, res *compiler.Result, c vmCase, slice int64) golden {
	t.Helper()
	d := &vmDriver{prog: res.Verified, cfg: c.sc.vmConfig(), sc: &c.sc, h: c.sc.handler(res.Module),
		slice: slice, args: map[string]string{}}
	ro, err := d.run(caseInputs(t, res, c))
	if err != nil {
		t.Fatalf("vm: %v", err)
	}
	g, err := toGolden(res.Module, ro, d.args)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// TestVMConformance_Run runs conformance/vm/run: each case is compiled,
// run on the VM against its scripted effects and compared with
// expected.json (final value or failure code, and the effect trace with
// argument hashes). A case with fuel_slice must give the same golden when
// run without slicing (vm_003 resume).
func TestVMConformance_Run(t *testing.T) {
	for _, c := range loadVMCases(t, "run") {
		t.Run(c.name, func(t *testing.T) {
			res := compileCase(t, c)
			got := runCaseVM(t, res, c, c.sc.FuelSlice)
			if c.sc.FuelSlice > 0 {
				whole := runCaseVM(t, res, c, 0)
				if !reflect.DeepEqual(got, whole) {
					t.Fatalf("fuel slices of %d changed the run:\n%+v\nvs\n%+v", c.sc.FuelSlice, got, whole)
				}
			}
			checkGolden(t, filepath.Join(c.dir, "expected.json"), got)
		})
	}
}

func checkGolden(t *testing.T, path string, got golden) {
	t.Helper()
	b, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, '\n')
	if *update {
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	_ = json.Unmarshal(b, &g)
	if !reflect.DeepEqual(w, g) {
		t.Fatalf("golden mismatch for %s\n--- want\n%s\n--- got\n%s", path, want, b)
	}
}
