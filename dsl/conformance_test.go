package dsl

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// update rewrites the conformance goldens: go test ./dsl -run X -update
var update = flag.Bool("update", false, "rewrite conformance/dsl goldens")

// parseCaseResult is the shape of conformance/dsl/parse/<case>.expected.json.
type parseCaseResult struct {
	AST         any          `json:"ast,omitempty"`
	Diagnostics []Diagnostic `json:"diagnostics,omitempty"`
}

// checkCaseResult is the shape of conformance/dsl/check/<case>.expected.json.
type checkCaseResult = Analysis

// suiteCase is one .ff file of a suite.
type suiteCase struct {
	Name string // base name without extension
	Path string // absolute path of the .ff file
	Src  []byte
}

func loadSuite(t *testing.T, suite string, prefixes ...string) []suiteCase {
	t.Helper()
	dir := testkit.ConformanceDir(t, suite)
	names, err := filepath.Glob(filepath.Join(dir, "*.ff"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	var out []suiteCase
	for _, n := range names {
		base := strings.TrimSuffix(filepath.Base(n), ".ff")
		if len(prefixes) > 0 {
			match := false
			for _, p := range prefixes {
				if strings.HasPrefix(base, p) {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		src, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, suiteCase{Name: base, Path: n, Src: src})
	}
	if len(out) == 0 {
		t.Fatalf("suite %s has no cases with prefixes %v", suite, prefixes)
	}
	return out
}

// casePolicy reads <case>.policy.json, or a first-line comment
// `// policy: {...}`.
func casePolicy(t *testing.T, c suiteCase) Policy {
	t.Helper()
	var raw []byte
	if b, err := os.ReadFile(strings.TrimSuffix(c.Path, ".ff") + ".policy.json"); err == nil {
		raw = b
	} else {
		sc := bufio.NewScanner(bytes.NewReader(c.Src))
		if sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if rest, ok := strings.CutPrefix(line, "// policy:"); ok {
				raw = []byte(rest)
			}
		}
	}
	if raw == nil {
		return Policy{}
	}
	var p Policy
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("%s: bad policy: %v", c.Name, err)
	}
	return p
}

// golden compares got with <case>.expected.json, or rewrites it with
// -update. Comparison is on the decoded JSON data.
func golden(t *testing.T, c suiteCase, got any) {
	t.Helper()
	canon, err := core.CanonicalJSON(got)
	if err != nil {
		t.Fatal(err)
	}
	var gotData any
	if err := json.Unmarshal(canon, &gotData); err != nil {
		t.Fatal(err)
	}
	path := strings.TrimSuffix(c.Path, ".ff") + ".expected.json"
	if *update {
		pretty, err := json.MarshalIndent(gotData, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(pretty, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: missing golden (run with -update): %v", c.Name, err)
	}
	var wantData any
	if err := json.Unmarshal(want, &wantData); err != nil {
		t.Fatalf("%s: bad golden: %v", c.Name, err)
	}
	if !reflect.DeepEqual(gotData, wantData) {
		pretty, _ := json.MarshalIndent(gotData, "", "  ")
		t.Errorf("%s: result differs from golden %s\ngot:\n%s", c.Name, filepath.Base(path), pretty)
	}
}

// runParseCase parses a case and compares the AST or diagnostics.
func runParseCase(t *testing.T, c suiteCase) parseCaseResult {
	t.Helper()
	f, diags := Parse(c.Name+".ff", c.Src)
	var res parseCaseResult
	if len(diags) > 0 {
		res.Diagnostics = diags
	} else {
		res.AST = ASTValue(f)
		// Every successful parse must survive a format round trip.
		assertRoundTrip(t, f)
	}
	golden(t, c, res)
	return res
}

// runCheckCase analyzes a case and compares the result.
func runCheckCase(t *testing.T, c suiteCase) Analysis {
	t.Helper()
	opts := Options{Policy: casePolicy(t, c), FileName: c.Name + ".ff"}
	a := Analyze(string(c.Src), opts)
	golden(t, c, a)
	for _, d := range a.Diagnostics {
		assertSpanInSource(t, c.Src, d)
	}
	return a
}

func assertRoundTrip(t *testing.T, f *File) {
	t.Helper()
	want, err := ASTJSON(f)
	if err != nil {
		t.Fatal(err)
	}
	out := Format(f)
	g, diags := Parse("roundtrip.ff", []byte(out))
	if len(diags) > 0 {
		t.Fatalf("formatted output does not parse: %v\n%s", diags, out)
	}
	got, _ := ASTJSON(g)
	if !bytes.Equal(want, got) {
		t.Fatalf("format round trip changed the AST\nformatted:\n%s\nwant %s\ngot  %s", out, want, got)
	}
}

func assertSpanInSource(t *testing.T, src []byte, d Diagnostic) {
	t.Helper()
	spans := append([]SourceSpan{d.Primary}, d.Secondary...)
	for _, s := range spans {
		if s.File == registryFile {
			continue
		}
		if s.Start.Offset < 0 || s.End.Offset > len(src) || s.Start.Offset > s.End.Offset {
			t.Errorf("%s: span %+v outside source of %d bytes", d.Code, s, len(src))
		}
		if s.Start.Line < 1 || s.Start.Col < 1 || s.End.Line < s.Start.Line {
			t.Errorf("%s: bad span positions %+v", d.Code, s)
		}
	}
}

func codes(ds []Diagnostic) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.Code)
	}
	return out
}

// TestDSLConformance_Parse runs every case of conformance/dsl/parse.
func TestDSLConformance_Parse(t *testing.T) {
	for _, c := range loadSuite(t, "dsl/parse") {
		t.Run(c.Name, func(t *testing.T) { runParseCase(t, c) })
	}
}

// TestDSLConformance_Check runs every case of conformance/dsl/check.
func TestDSLConformance_Check(t *testing.T) {
	for _, c := range loadSuite(t, "dsl/check") {
		t.Run(c.Name, func(t *testing.T) { runCheckCase(t, c) })
	}
}

// expectCodeInName asserts that a case named after a code (e_E0102_...,
// t_E0301_..., f_W0402_...) reports that code.
func expectCodeInName(t *testing.T, name string, ds []Diagnostic) {
	t.Helper()
	for _, part := range strings.Split(name, "_") {
		if len(part) == 5 && (part[0] == 'E' || part[0] == 'W') {
			for _, d := range ds {
				if d.Code == part {
					return
				}
			}
			t.Errorf("%s: expected a %s diagnostic, got %v", name, part, codes(ds))
		}
	}
}
