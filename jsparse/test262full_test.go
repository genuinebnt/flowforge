package jsparse

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// test262Unsupported lists test262 features that are not ECMAScript 2024
// syntax (later editions or proposals); tests using them are skipped.
var test262Unsupported = []string{
	"decorators", "import-defer", "source-phase-imports", "source-phase-imports-module-source",
	"regexp-modifiers", "regexp-duplicate-named-groups", "explicit-resource-management",
	"import-assertions", "deferred-import-evaluation", "import-text", "import-bytes",
}

// TestJSTest262Full runs the syntax tests of a local tc39/test262
// checkout (FF_TEST262_DIR; not vendored) for the language/ and
// annexB/language/ directories: tests with `negative: phase: parse` must
// be rejected, all others accepted, in each mode their flags select.
// It is a development aid; the vendored suite is
// TestJSConformanceTest262.
func TestJSTest262Full(t *testing.T) {
	root := os.Getenv("FF_TEST262_DIR")
	if root == "" {
		t.Skip("set FF_TEST262_DIR to a test262 checkout")
	}
	var failures []string
	total, skipped := 0, 0
	for _, sub := range []string{"test/language", "test/annexB/language"} {
		filepath.WalkDir(filepath.Join(root, sub), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".js") || strings.Contains(path, "_FIXTURE") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			meta := string(src)
			if i := strings.Index(meta, "/*---"); i >= 0 {
				if j := strings.Index(meta[i:], "---*/"); j >= 0 {
					meta = meta[i : i+j]
				}
			}
			for _, f := range test262Unsupported {
				if strings.Contains(meta, f) {
					skipped++
					return nil
				}
			}
			negative := strings.Contains(meta, "negative:") && strings.Contains(meta, "phase: parse")
			flagsLine := ""
			if i := strings.Index(meta, "flags:"); i >= 0 {
				flagsLine = meta[i:]
				if j := strings.IndexByte(flagsLine, '\n'); j >= 0 {
					flagsLine = flagsLine[:j]
				}
			}
			module := strings.Contains(flagsLine, "module")
			var modes []string
			switch {
			case module || strings.Contains(flagsLine, "raw") || strings.Contains(flagsLine, "noStrict"):
				modes = []string{""}
			case strings.Contains(flagsLine, "onlyStrict"):
				modes = []string{"\"use strict\";\n"}
			default:
				modes = []string{"", "\"use strict\";\n"}
			}
			st := SourceScript
			if module {
				st = SourceModule
			}
			rel, _ := filepath.Rel(root, path)
			for _, prefix := range modes {
				total++
				_, err := Parse(append([]byte(prefix), src...), Options{SourceType: st})
				if (err != nil) != negative {
					mode := "sloppy"
					if prefix != "" {
						mode = "strict"
					}
					msg := "accepted"
					if err != nil {
						msg = err.Error()
					}
					failures = append(failures, rel+" ["+mode+"] "+msg)
				}
			}
			return nil
		})
	}
	sort.Strings(failures)
	t.Logf("test262: %d runs, %d skipped files, %d failures", total, skipped, len(failures))
	if out := os.Getenv("FF_TEST262_OUT"); out != "" {
		os.WriteFile(out, []byte(strings.Join(failures, "\n")+"\n"), 0o644)
	}
	if len(failures) > 0 {
		t.Errorf("%d test262 failures (first: %s)", len(failures), failures[0])
	}
}
