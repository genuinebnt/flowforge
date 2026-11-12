package main

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Module is one [module.<name>] entry of modules.toml (spec 25.10).
type Module struct {
	Name        string
	Layer       int
	Spec        []string
	Dir         string // package directory relative to the repository root
	Depends     []string
	TestPrefix  string   // Go test name prefix after "Test", e.g. "Scope"
	Required    []string // required case ids, e.g. "001"
	Conformance []string
	Properties  []string
	Fuzz        []string
	Bench       []string
	Mutation    bool
}

// Manifest is the parsed modules.toml.
type Manifest struct {
	Modules map[string]*Module
}

// Names returns the module names in manifest order of layer, then name.
func (m *Manifest) Names() []string {
	names := make([]string, 0, len(m.Modules))
	for n := range m.Modules {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := m.Modules[names[i]], m.Modules[names[j]]
		if a.Layer != b.Layer {
			return a.Layer < b.Layer
		}
		return a.Name < b.Name
	})
	return names
}

// parseManifest reads the small TOML subset modules.toml uses: [module.x]
// tables whose values are strings, integers, booleans, arrays of strings and
// one-level inline tables. Comments start with '#'.
func parseManifest(r io.Reader) (*Manifest, error) {
	m := &Manifest{Modules: map[string]*Module{}}
	var cur *Module
	sc := bufio.NewScanner(r)
	for lineNo := 1; sc.Scan(); lineNo++ {
		line := strings.TrimSpace(stripComment(sc.Text()))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") {
			name, ok := strings.CutPrefix(strings.Trim(line, "[]"), "module.")
			if !ok || !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("modules.toml:%d: expected [module.<name>]", lineNo)
			}
			cur = &Module{Name: name, Dir: name}
			m.Modules[name] = cur
			continue
		}
		if cur == nil {
			return nil, fmt.Errorf("modules.toml:%d: key outside a [module.*] table", lineNo)
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("modules.toml:%d: expected key = value", lineNo)
		}
		if err := cur.set(strings.TrimSpace(key), strings.TrimSpace(val)); err != nil {
			return nil, fmt.Errorf("modules.toml:%d: %w", lineNo, err)
		}
	}
	return m, sc.Err()
}

func stripComment(s string) string {
	inStr := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '"':
			inStr = !inStr
		case '#':
			if !inStr {
				return s[:i]
			}
		}
	}
	return s
}

func (mod *Module) set(key, val string) error {
	var err error
	switch key {
	case "layer":
		mod.Layer, err = strconv.Atoi(val)
	case "dir":
		mod.Dir, err = parseString(val)
	case "spec":
		mod.Spec, err = parseStrings(val)
	case "depends":
		mod.Depends, err = parseStrings(val)
	case "conformance":
		mod.Conformance, err = parseStrings(val)
	case "properties":
		mod.Properties, err = parseStrings(val)
	case "fuzz":
		mod.Fuzz, err = parseStrings(val)
	case "bench":
		mod.Bench, err = parseStrings(val)
	case "mutation":
		mod.Mutation, err = strconv.ParseBool(val)
	case "tests":
		var tbl map[string]string
		tbl, err = parseInlineTable(val)
		if err != nil {
			return err
		}
		if mod.TestPrefix, err = parseString(tbl["prefix"]); err != nil {
			return fmt.Errorf("tests.prefix: %w", err)
		}
		mod.Required, err = parseStrings(tbl["required"])
	default:
		// Unknown keys are tolerated so the manifest can grow.
	}
	if err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

func parseString(v string) (string, error) {
	s, err := strconv.Unquote(strings.TrimSpace(v))
	if err != nil {
		return "", fmt.Errorf("bad string %q", v)
	}
	return s, nil
}

func parseStrings(v string) ([]string, error) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "[") || !strings.HasSuffix(v, "]") {
		return nil, fmt.Errorf("bad array %q", v)
	}
	var out []string
	for _, part := range splitTop(v[1 : len(v)-1]) {
		if strings.TrimSpace(part) == "" {
			continue
		}
		s, err := parseString(part)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func parseInlineTable(v string) (map[string]string, error) {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "{") || !strings.HasSuffix(v, "}") {
		return nil, fmt.Errorf("bad inline table %q", v)
	}
	out := map[string]string{}
	for _, part := range splitTop(v[1 : len(v)-1]) {
		k, val, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("bad inline table entry %q", part)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(val)
	}
	return out, nil
}

// splitTop splits on commas that are not nested in brackets or strings.
func splitTop(s string) []string {
	var parts []string
	depth, start, inStr := 0, 0, false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"':
			inStr = !inStr
		case inStr:
		case c == '[' || c == '{':
			depth++
		case c == ']' || c == '}':
			depth--
		case c == ',' && depth == 0:
			parts = append(parts, s[start:i])
			start = i + 1
		}
	}
	return append(parts, s[start:])
}
