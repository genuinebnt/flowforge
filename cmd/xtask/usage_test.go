package main

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// TestUsageCore runs the usage report over the real tree and checks the
// cases a text search gets wrong: a method called through an interface
// value (core.Clock.Now), a method on a value type (core.UUID.String), a
// field (core.Error.Kind) and the exclusion of the package's own uses.
func TestUsageCore(t *testing.T) {
	if testing.Short() {
		t.Skip("type-checks the whole module")
	}
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open("modules.toml")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	man, err := parseManifest(f)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := collectUsage(man, "core", false)
	if err != nil {
		t.Fatal(err)
	}
	usedBy := func(id, pkg string) bool { return slices.Contains(consumers(rep.sites[id]), pkg) }
	for _, c := range []struct{ id, pkg string }{
		{"Clock.Now", "alerts"},   // interface method call
		{"UUID.String", "auth"},   // method on a value type
		{"Clock", "placement"},    // type used in a signature
		{"SplitMix64", "testkit"}, // concrete type
		{"Error.Kind", "engine"},  // struct field
	} {
		if !usedBy(c.id, c.pkg) {
			t.Errorf("%s: want a use in %s, got %v", c.id, c.pkg, consumers(rep.sites[c.id]))
		}
	}
	if rep.kinds["UUID.String"] != "method" || rep.kinds["Clock"] != "type" || rep.kinds["Error.Kind"] != "field" {
		t.Errorf("kinds: UUID.String=%q Clock=%q", rep.kinds["UUID.String"], rep.kinds["Clock"])
	}
	for id, sites := range rep.sites {
		for _, s := range sites {
			if s.Pkg == "core" || strings.HasPrefix(s.Pos, "core/") && !strings.HasPrefix(s.Pos, "core/netnorm/") {
				t.Fatalf("%s: use inside the target package reported: %s", id, s.Pos)
			}
			if s.Test {
				t.Fatalf("%s: test use reported without -tests: %s", id, s.Pos)
			}
		}
	}
	if got := rep.implicitVia("SystemClock.Now"); got != "Clock.Now" {
		t.Errorf("SystemClock.Now should be reached via Clock.Now, got %q", got)
	}
	if got := rep.implicitVia("UUID.Scan"); got != "sql.Scanner" {
		t.Errorf("UUID.Scan should be called implicitly by sql.Scanner, got %q", got)
	}
	withTests, err := collectUsage(man, "core", true)
	if err != nil {
		t.Fatal(err)
	}
	if n, m := len(withTests.sites["UUID"]), len(rep.sites["UUID"]); n <= m {
		t.Errorf("-tests should add test uses of UUID: %d with, %d without", n, m)
	}
}
