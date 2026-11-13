package jsparse

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

type test262Exception struct {
	File   string `json:"file"`
	Reason string `json:"reason"`
}

type test262Exceptions struct {
	Pass     []test262Exception `json:"pass"`
	Explicit []test262Exception `json:"explicit"`
	Fail     []test262Exception `json:"fail"`
	Early    []test262Exception `json:"early"`
}

func loadExceptions(t *testing.T) test262Exceptions {
	t.Helper()
	var ex test262Exceptions
	if err := json.Unmarshal(testkit.ReadFile(t, "jsparse", "test262_exceptions.json"), &ex); err != nil {
		t.Fatal(err)
	}
	return ex
}

func exceptionNames(list []test262Exception) []string {
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.File)
	}
	sort.Strings(out)
	return out
}

// test262File is one file of the vendored suite.
type test262File struct {
	name string
	src  []byte
}

func readSuiteDir(t *testing.T, sub string) []test262File {
	t.Helper()
	dir := filepath.Join(testkit.ConformanceDir(t, "third_party/test262-parser-tests"), sub)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []test262File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".js") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, test262File{name: e.Name(), src: b})
	}
	if len(out) == 0 {
		t.Fatalf("no files in %s", dir)
	}
	return out
}

func sourceTypeOf(name string) SourceType {
	if strings.HasSuffix(name, ".module.js") {
		return SourceModule
	}
	return SourceScript
}

func parseFile(f test262File) (*Program, error) { return parseFileWith(f, Algorithms{}) }

func parseFileWith(f test262File, alg Algorithms) (*Program, error) {
	return Parse(f.src, Options{SourceType: sourceTypeOf(f.name), Algorithms: alg})
}

// TestJSConformanceTest262 runs the tc39 test262-parser-tests suite: pass
// files parse, fail and early files are rejected, and pass/X.js has the
// same canonical AST as pass-explicit/X.js. The deviations allowed are
// exactly those in conformance/jsparse/test262_exceptions.json. The suite
// runs under every candidate of Algorithms (docs/CONVENTIONS.md
// "Pluggable algorithms"): each must deviate exactly as the default.
func TestJSConformanceTest262(t *testing.T) {
	for _, cand := range algoCandidates {
		t.Run(cand.name, func(t *testing.T) { test262Suite(t, cand.alg) })
	}
}

func test262Suite(t *testing.T, alg Algorithms) {
	parseFile := func(f test262File) (*Program, error) { return parseFileWith(f, alg) }
	ex := loadExceptions(t)
	check := func(t *testing.T, what string, got []string, want []test262Exception) {
		t.Helper()
		sort.Strings(got)
		w := exceptionNames(want)
		if strings.Join(got, ",") != strings.Join(w, ",") {
			t.Errorf("%s deviations = %d %v\nwant exactly %d %v", what, len(got), got, len(w), w)
		}
	}

	t.Run("pass+explicit", func(t *testing.T) {
		pass := readSuiteDir(t, "pass")
		explicit := readSuiteDir(t, "pass-explicit")
		byName := map[string]test262File{}
		for _, f := range explicit {
			byName[f.name] = f
		}
		var bad, badExplicit []string
		ok := 0
		for _, f := range pass {
			prog, err := parseFile(f)
			if err != nil {
				bad = append(bad, f.name)
				t.Logf("pass/%s: %v", f.name, err)
				continue
			}
			e, found := byName[f.name]
			if !found {
				t.Errorf("pass-explicit/%s missing", f.name)
				continue
			}
			eprog, err := parseFile(e)
			if err != nil {
				badExplicit = append(badExplicit, f.name)
				t.Logf("pass-explicit/%s: %v", f.name, err)
				continue
			}
			a, b := explicitEquivalent(t, prog), explicitEquivalent(t, eprog)
			if a != b {
				badExplicit = append(badExplicit, f.name)
				t.Logf("AST mismatch %s\n pass:     %.400s\n explicit: %.400s", f.name, a, b)
				continue
			}
			ok++
		}
		t.Logf("pass: %d files, %d parsed with equal explicit AST", len(pass), ok)
		check(t, "pass", bad, ex.Pass)
		check(t, "pass-explicit", badExplicit, ex.Explicit)
	})

	for _, sub := range []string{"fail", "early"} {
		t.Run(sub, func(t *testing.T) {
			files := readSuiteDir(t, sub)
			var accepted []string
			for _, f := range files {
				if _, err := parseFile(f); err == nil {
					accepted = append(accepted, f.name)
				}
			}
			t.Logf("%s: %d files, %d rejected", sub, len(files), len(files)-len(accepted))
			want := ex.Fail
			if sub == "early" {
				want = ex.Early
			}
			check(t, sub, accepted, want)
		})
	}
}

// explicitEquivalent returns the canonical JSON of prog normalized by the
// equivalence relation of the upstream suite, whose explicit files were
// produced from the Shift AST: (1) a non-computed property key is its
// PropName string whether written as an identifier, string or number
// ({"type":"PropName","value":...}); (2) nested SequenceExpressions are
// flattened (Shift's comma is a left-associative binary operator, so
// `a, b, c` and `((a, b), c)` are the same tree). The relation is
// specified in conformance/jsparse/README.md.
func explicitEquivalent(t *testing.T, prog *Program) string {
	t.Helper()
	// Rule 1 on the arena: a non-computed key becomes an Identifier named
	// by its PropName (string value, or Number::toString of a number).
	for i := range prog.Nodes {
		n := &prog.Nodes[i]
		switch n.Kind {
		case KProperty, KMethodDefinition, KPropertyDefinition:
			if n.Flags&NComputed != 0 || n.A == NoNode {
				continue
			}
			k := &prog.Nodes[n.A]
			if k.Kind != KLiteral {
				continue
			}
			switch k.Op {
			case LitString:
				k.Kind = KIdentifier
			case LitNumber:
				prog.Strings = append(prog.Strings, NumberToString(k.Num))
				k.Kind, k.Str = KIdentifier, StrID(len(prog.Strings)-1)
			}
		}
	}
	var v any
	if err := json.Unmarshal(prog.JSON(JSONOptions{}), &v); err != nil {
		t.Fatal(err)
	}
	v = normalizeExplicit(v)
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func normalizeExplicit(v any) any {
	switch x := v.(type) {
	case []any:
		for i := range x {
			x[i] = normalizeExplicit(x[i])
		}
		return x
	case map[string]any:
		for k, c := range x {
			x[k] = normalizeExplicit(c)
		}
		switch x["type"] {
		case "SequenceExpression":
			var flat []any
			for _, e := range x["expressions"].([]any) {
				if m, ok := e.(map[string]any); ok && m["type"] == "SequenceExpression" {
					flat = append(flat, m["expressions"].([]any)...)
					continue
				}
				flat = append(flat, e)
			}
			x["expressions"] = flat
		}
		return x
	}
	return v
}
