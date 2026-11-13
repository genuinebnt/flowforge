package jsparse

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestJSOracleDump supports the differential check against acorn
// (conformance/jsparse/tools/acorn_diff.js). It runs only when
// FF_JSPARSE_DUMP names a file listing source paths (one per line,
// "module:" prefix for modules); for each it writes <path>.jsparse.json
// next to FF_JSPARSE_DUMP_OUT with our canonical JSON, or an "ERROR" line.
func TestJSOracleDump(t *testing.T) {
	list := os.Getenv("FF_JSPARSE_DUMP")
	out := os.Getenv("FF_JSPARSE_DUMP_OUT")
	if list == "" || out == "" {
		t.Skip("set FF_JSPARSE_DUMP and FF_JSPARSE_DUMP_OUT to run the acorn differential dump")
	}
	f, err := os.Open(list)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		st := SourceScript
		if p, ok := strings.CutPrefix(line, "module:"); ok {
			line, st = p, SourceModule
		}
		src, err := os.ReadFile(filepath.Clean(line))
		if err != nil {
			t.Fatal(err)
		}
		bw.WriteString(line)
		bw.WriteByte('\t')
		prog, err := Parse(src, Options{SourceType: st})
		if err != nil {
			bw.WriteString("ERROR " + strings.ReplaceAll(err.Error(), "\n", " "))
		} else {
			bw.Write(prog.JSON(JSONOptions{}))
		}
		bw.WriteByte('\n')
	}
}
