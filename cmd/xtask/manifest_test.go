package main

import (
	"slices"
	"strings"
	"testing"
)

func TestParseManifest(t *testing.T) {
	src := `
# comment
[module.scope]
layer = 1
spec = ["4"]          # trailing comment
depends = ["core", "core/netnorm"]
tests = { prefix = "Scope", required = ["001","002"] }
conformance = ["scope/normalize", "scope/decide"]
fuzz = ["FuzzScopeNormalize"]
mutation = true

[module.vm]
layer = 3
dir = "vm"
tests = { prefix = "VM", required = [] }
`
	m, err := parseManifest(strings.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	s := m.Modules["scope"]
	if s == nil || s.Layer != 1 || s.TestPrefix != "Scope" || !slices.Equal(s.Required, []string{"001", "002"}) ||
		!slices.Equal(s.Depends, []string{"core", "core/netnorm"}) || !s.Mutation || len(s.Conformance) != 2 {
		t.Fatalf("scope parsed wrong: %+v", s)
	}
	if got := m.Names(); !slices.Equal(got, []string{"scope", "vm"}) {
		t.Fatalf("names = %v", got)
	}
	if _, err := parseManifest(strings.NewReader("layer = 1")); err == nil {
		t.Fatal("key outside table accepted")
	}
}
