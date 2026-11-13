package htmlparse

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

// datCase is one html5lib tree-construction test (.dat format).
type datCase struct {
	ID        string // "<file>#<index>" (index is 0-based within the file)
	Data      string
	Fragment  string // #document-fragment context ("" for a document test)
	HasFrag   bool
	ScriptOn  bool // #script-on
	ScriptOff bool // #script-off
	Document  string
}

// readDat parses a .dat file. Sections start with a line that is exactly
// one of the known headings; the #document section runs until the next
// #data line, minus trailing empty lines.
func readDat(t testing.TB, path string) []datCase {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(path)
	var cases []datCase
	var cur *datCase
	section := ""
	var buf []string
	flush := func() {
		if cur == nil {
			buf = nil
			return
		}
		switch section {
		case "data":
			cur.Data = strings.Join(buf, "\n")
		case "document-fragment":
			cur.Fragment = strings.TrimSpace(strings.Join(buf, "\n"))
			cur.HasFrag = true
		case "document":
			for len(buf) > 0 && buf[len(buf)-1] == "" {
				buf = buf[:len(buf)-1]
			}
			cur.Document = strings.Join(buf, "\n")
		}
		buf = nil
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			h := line[1:]
			switch h {
			case "data", "errors", "new-errors", "document-fragment", "script-off", "script-on", "document", "description":
				flush()
				if h == "data" {
					cases = append(cases, datCase{ID: fmt.Sprintf("%s#%d", base, len(cases))})
					cur = &cases[len(cases)-1]
				}
				switch h {
				case "script-on":
					cur.ScriptOn = true
				case "script-off":
					cur.ScriptOff = true
				}
				section = h
				continue
			}
		}
		buf = append(buf, line)
	}
	flush()
	return cases
}

func loadDatSuite(t testing.TB) []datCase {
	t.Helper()
	dir := testkit.ConformanceDir(t, "third_party/wpt-html-parsing")
	names, err := filepath.Glob(filepath.Join(dir, "*.dat"))
	if err != nil || len(names) == 0 {
		t.Fatalf("no .dat files in %s: %v", dir, err)
	}
	slices.Sort(names)
	var all []datCase
	for _, n := range names {
		all = append(all, readDat(t, n)...)
	}
	return all
}

// parseContext turns a #document-fragment line ("td", "svg path",
// "math mi") into a ContextElement.
func parseContext(s string) ContextElement {
	if ns, name, ok := strings.Cut(s, " "); ok {
		switch ns {
		case "svg":
			return ContextElement{Namespace: SVGNamespace, Name: name}
		case "math":
			return ContextElement{Namespace: MathMLNamespace, Name: name}
		}
	}
	return ContextElement{Namespace: HTMLNamespace, Name: s}
}

// runDatCase parses one case and returns our dump.
func runDatCase(c datCase) string {
	opts := Options{Scripting: c.ScriptOn}
	var doc *Document
	if c.HasFrag {
		doc, _ = ParseFragment([]byte(c.Data), parseContext(c.Fragment), opts)
	} else {
		doc, _ = Parse([]byte(c.Data), opts)
	}
	return Dump(doc)
}

// knownTreeFailures lists .dat cases that do not match (see
// OPEN_QUESTIONS.md). The test fails if this list is not exact, so a newly
// passing or newly failing case is noticed.
//
// All six need script execution (document.write, DOM mutation from
// <script>), which this parser never does; see OPEN_QUESTIONS.md Q-TREE-1.
var knownTreeFailures = []string{
	"scripted_adoption01.dat#0",
	"scripted_ark.dat#0",
	"scripted_foster01.dat#0",
	"scripted_foster01.dat#1",
	"scripted_webkit01.dat#0",
	"scripted_webkit01.dat#1",
}

func TestHTMLConformanceTree(t *testing.T) {
	cases := loadDatSuite(t)
	var failed []string
	var details []string
	counts := map[string]int{}
	for _, c := range cases {
		kind := "document"
		if c.HasFrag {
			kind = "fragment"
		}
		if c.ScriptOn {
			kind += "/script-on"
		} else if c.ScriptOff {
			kind += "/script-off"
		}
		counts[kind+" total"]++
		got := runDatCase(c)
		if got == c.Document {
			counts[kind+" pass"]++
			continue
		}
		failed = append(failed, c.ID)
		details = append(details, fmt.Sprintf("%s (ctx %q)\n#data\n%s\n#got\n%s\n#want\n%s", c.ID, c.Fragment, c.Data, got, c.Document))
	}
	pass := len(cases) - len(failed)
	t.Logf("tree construction: %d/%d pass; %v", pass, len(cases), counts)
	if !slices.Equal(failed, knownTreeFailures) {
		for _, d := range details[:min(len(details), 25)] {
			t.Log(d)
		}
		t.Errorf("tree failure list changed: %d failures:\n%s", len(failed), quoteList(failed))
	}
}
