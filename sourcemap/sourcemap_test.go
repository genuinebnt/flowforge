package sourcemap

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

type vlqFile struct {
	Values []struct {
		Value   int32  `json:"value"`
		Encoded string `json:"encoded"`
	} `json:"values"`
	DecodeErrors []struct {
		Input string `json:"input"`
		Error string `json:"error"`
	} `json:"decode_errors"`
	Noncanonical []struct {
		Input string `json:"input"`
		Value int32  `json:"value"`
	} `json:"noncanonical"`
}

type mappingsFile struct {
	Cases []struct {
		Name      string    `json:"name"`
		Mappings  string    `json:"mappings"`
		Sources   int       `json:"sources"`
		Names     int       `json:"names"`
		Lines     int       `json:"lines"`
		Canonical bool      `json:"canonical"`
		Expected  [][]int32 `json:"expected"`
		Error     string    `json:"error"`
		Offset    int       `json:"offset"`
	} `json:"cases"`
}

type mapsFile struct {
	Cases []struct {
		Name    string `json:"name"`
		Text    string `json:"text"`
		Options struct {
			MaxInputBytes int `json:"max_input_bytes"`
			MaxSources    int `json:"max_sources"`
		} `json:"options"`
		Error        string       `json:"error"`
		Grade        string       `json:"grade"`
		MapURL       string       `json:"map_url"`
		Resolved     []string     `json:"resolved"`
		Contents     []*string    `json:"contents"`
		Ignored      []bool       `json:"ignored"`
		MappingCount *int         `json:"mapping_count"`
		SourceCount  *int         `json:"source_count"`
		Lookups      []lookupCase `json:"lookups"`
	} `json:"cases"`
}

type lookupCase struct {
	Line     int32 `json:"line"`
	Col      int32 `json:"col"`
	Found    bool  `json:"found"`
	Source   int32 `json:"source"`
	OrigLine int32 `json:"orig_line"`
	OrigCol  int32 `json:"orig_col"`
	Name     int32 `json:"name"`
}

func row(m Mapping) []int32 {
	return []int32{m.GenLine, m.GenCol, m.Source, m.OrigLine, m.OrigCol, m.Name}
}

// TestSMapConformanceVLQ runs conformance/sourcemap/vlq.json.
func TestSMapConformanceVLQ(t *testing.T) {
	var f vlqFile
	if err := json.Unmarshal(testkit.ReadFile(t, "sourcemap", "vlq.json"), &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Values {
		if got := string(AppendVLQ(nil, c.Value)); got != c.Encoded {
			t.Errorf("encode %d = %q, want %q", c.Value, got, c.Encoded)
		}
		v, n, err := DecodeVLQ(c.Encoded)
		if err != nil || v != c.Value || n != len(c.Encoded) {
			t.Errorf("decode %q = %d,%d,%v want %d", c.Encoded, v, n, err, c.Value)
		}
	}
	for _, c := range f.DecodeErrors {
		if _, _, err := DecodeVLQ(c.Input); err == nil || err.Code.String() != c.Error {
			t.Errorf("decode %q: %v, want %s", c.Input, err, c.Error)
		}
	}
	for _, c := range f.Noncanonical {
		if v, _, err := DecodeVLQ(c.Input); err != nil || v != c.Value {
			t.Errorf("decode %q = %d %v, want %d", c.Input, v, err, c.Value)
		}
	}
	if len(f.Values) < 20 {
		t.Fatal("vlq suite too small")
	}
}

// TestSMapConformanceMappings runs conformance/sourcemap/mappings.json.
func TestSMapConformanceMappings(t *testing.T) {
	var f mappingsFile
	if err := json.Unmarshal(testkit.ReadFile(t, "sourcemap", "mappings.json"), &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ms, lines, err := DecodeMappings(c.Mappings, c.Sources, c.Names)
			if c.Error != "" {
				if err == nil || err.Code.String() != c.Error || err.Offset != c.Offset {
					t.Fatalf("got %v, want %s at %d", err, c.Error, c.Offset)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if lines != c.Lines || len(ms) != len(c.Expected) {
				t.Fatalf("got %d lines %d mappings, want %d %d", lines, len(ms), c.Lines, len(c.Expected))
			}
			for i, m := range ms {
				if fmt.Sprint(row(m)) != fmt.Sprint(c.Expected[i]) {
					t.Errorf("mapping %d = %v, want %v", i, row(m), c.Expected[i])
				}
			}
			enc, e := EncodeMappings(ms, lines)
			if e != nil {
				t.Fatal(e)
			}
			if (enc == c.Mappings) != c.Canonical || isCanonical(c.Mappings) != c.Canonical {
				t.Errorf("re-encoded %q (canonical %v)", enc, c.Canonical)
			}
		})
	}
}

// TestSMapConformanceMaps runs conformance/sourcemap/maps.json.
func TestSMapConformanceMaps(t *testing.T) {
	var f mapsFile
	if err := json.Unmarshal(testkit.ReadFile(t, "sourcemap", "maps.json"), &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			m, err := Parse([]byte(c.Text), Options{MaxInputBytes: c.Options.MaxInputBytes, MaxSources: c.Options.MaxSources})
			code := ""
			if err != nil {
				code = err.(*Error).Code.String()
			}
			if code != c.Error {
				t.Fatalf("error %v, want %q", err, c.Error)
			}
			if c.Grade != "" && GradeOf(err).String() != c.Grade {
				t.Errorf("grade %s, want %s", GradeOf(err), c.Grade)
			}
			if m == nil {
				return
			}
			if m.Grade != GradeOf(err) {
				t.Errorf("map grade %s, error grade %s", m.Grade, GradeOf(err))
			}
			if c.Resolved != nil && strings.Join(m.ResolveSources(c.MapURL), "|") != strings.Join(c.Resolved, "|") {
				t.Errorf("resolved %q, want %q", m.ResolveSources(c.MapURL), c.Resolved)
			}
			for i, w := range c.Contents {
				s := m.Sources[i]
				if (w == nil) == s.HasContent || (w != nil && *w != s.Content) {
					t.Errorf("content %d = %q/%v, want %v", i, s.Content, s.HasContent, w)
				}
			}
			for i, w := range c.Ignored {
				if m.Sources[i].Ignored != w {
					t.Errorf("source %d ignored %v, want %v", i, m.Sources[i].Ignored, w)
				}
			}
			if c.MappingCount != nil && len(m.Mappings) != *c.MappingCount {
				t.Errorf("%d mappings, want %d", len(m.Mappings), *c.MappingCount)
			}
			if c.SourceCount != nil && len(m.Sources) != *c.SourceCount {
				t.Errorf("%d sources, want %d", len(m.Sources), *c.SourceCount)
			}
			for _, l := range c.Lookups {
				got, ok := m.OriginalPositionFor(l.Line, l.Col)
				if ok != l.Found || (ok && (got.Source != l.Source || got.OrigLine != l.OrigLine || got.OrigCol != l.OrigCol || got.Name != l.Name)) {
					t.Errorf("lookup (%d,%d) = %v %v, want %+v", l.Line, l.Col, row(got), ok, l)
				}
			}
		})
	}
}

// --- invariants ---------------------------------------------------------

func randInt32(r core.Rng) int32 {
	switch core.IntN(r, 4) {
	case 0:
		return []int32{0, 1, -1, 15, 16, -16, math.MaxInt32, math.MinInt32, math.MinInt32 + 1, 1 << 20}[core.IntN(r, 10)]
	case 1:
		return int32(r.Uint64())
	default:
		return int32(core.IntN(r, 2000)) - 1000
	}
}

// TestSMapInv01_VLQRoundTrip: decode(encode(v)) = v for random and
// boundary int32 values (including MinInt32), the encoding is minimal,
// and it uses at most 7 digits.
func TestSMapInv01_VLQRoundTrip(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases()*10; i++ {
		v := randInt32(r)
		enc := AppendVLQ(nil, v)
		got, n, err := DecodeVLQ(string(enc))
		if err != nil || got != v || n != len(enc) || len(enc) > 7 {
			t.Fatalf("v=%d enc=%q -> %d,%d,%v", v, enc, got, n, err)
		}
		if !isCanonical(string(enc)) {
			t.Fatalf("encoding of %d not canonical", v)
		}
	}
}

func genMappings(r core.Rng) ([]Mapping, int, int, int) {
	lines := core.IntN(r, 8) + 1
	nSrc, nNames := core.IntN(r, 5)+1, core.IntN(r, 5)
	var ms []Mapping
	for l := 0; l < lines; l++ {
		col := int32(0)
		for k := core.IntN(r, 6); k > 0; k-- {
			col += int32(core.IntN(r, 50))
			if core.IntN(r, 8) == 0 {
				col = int32(core.IntN(r, math.MaxInt32))
			}
			m := Mapping{GenLine: int32(l), GenCol: col, Source: -1, OrigLine: -1, OrigCol: -1, Name: -1}
			if core.IntN(r, 5) > 0 {
				m.Source = int32(core.IntN(r, nSrc))
				m.OrigLine = int32(core.IntN(r, 1000))
				m.OrigCol = int32(core.IntN(r, 200))
				if nNames > 0 && core.IntN(r, 2) == 0 {
					m.Name = int32(core.IntN(r, nNames))
				}
			}
			ms = append(ms, m)
		}
	}
	return ms, lines, nSrc, nNames
}

// TestSMapInv02_MappingsRoundTrip: decode(encode(ms)) = ms for generated
// mappings (columns up to MaxInt32, unsorted columns within a line), and
// encode(decode(s)) = s for every canonical s.
func TestSMapInv02_MappingsRoundTrip(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		ms, lines, nSrc, nNames := genMappings(r)
		if core.IntN(r, 3) == 0 { // shuffle within lines: order must survive
			for j := 1; j < len(ms); j++ {
				if ms[j].GenLine == ms[j-1].GenLine && core.IntN(r, 2) == 0 {
					ms[j], ms[j-1] = ms[j-1], ms[j]
				}
			}
		}
		s, err := EncodeMappings(ms, lines)
		if err != nil {
			t.Fatal(err)
		}
		back, l2, derr := DecodeMappings(s, nSrc, nNames)
		if derr != nil || l2 != lines || fmt.Sprint(back) != fmt.Sprint(ms) {
			t.Fatalf("%q: %v lines %d vs %d\n%v\n%v", s, derr, l2, lines, back, ms)
		}
		again, _ := EncodeMappings(back, l2)
		if again != s || !isCanonical(s) {
			t.Fatalf("encode(decode(%q)) = %q", s, again)
		}
	}
}

// bruteLookup is the linear-scan reference for OriginalPositionFor: the
// mapping with the greatest GenCol <= col on the line, the last of equals.
func bruteLookup(ms []Mapping, line, col int32) (Mapping, bool) {
	var best Mapping
	found := false
	for _, m := range ms {
		if m.GenLine == line && m.GenCol <= col && (!found || m.GenCol >= best.GenCol) {
			best, found = m, true
		}
	}
	return best, found
}

// TestSMapInv03_LookupMatchesBruteForce: the binary-search lookup agrees
// with a linear scan on generated maps and random positions.
func TestSMapInv03_LookupMatchesBruteForce(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		ms, lines, nSrc, nNames := genMappings(r)
		for j := range ms {
			ms[j].GenCol %= 300 // dense columns make ties and gaps likely
		}
		m := mapFrom(t, ms, lines, nSrc, nNames)
		for k := 0; k < 50; k++ {
			line, col := int32(core.IntN(r, lines+1)), int32(core.IntN(r, 320))
			got, ok := m.OriginalPositionFor(line, col)
			want, wok := bruteLookup(m.Mappings, line, col)
			if ok != wok || got != want {
				t.Fatalf("(%d,%d): %v %v, brute %v %v", line, col, got, ok, want, wok)
			}
		}
	}
}

func mapFrom(t *testing.T, ms []Mapping, lines, nSrc, nNames int) *Map {
	t.Helper()
	s, err := EncodeMappings(ms, lines)
	if err != nil {
		t.Fatal(err)
	}
	srcs := make([]string, nSrc)
	for i := range srcs {
		srcs[i] = fmt.Sprintf("s%d.js", i)
	}
	names := make([]string, nNames)
	for i := range names {
		names[i] = fmt.Sprintf("n%d", i)
	}
	text, _ := json.Marshal(map[string]any{"version": 3, "sources": srcs, "names": names, "mappings": s})
	m, perr := Parse(text, Options{})
	if perr != nil {
		t.Fatalf("%s: %v", text, perr)
	}
	return m
}

// TestSMapInv04_SortedAndInBounds: every successfully parsed map (regular
// or index, generated or mutated) has mappings sorted by generated
// position and every index in range.
func TestSMapInv04_SortedAndInBounds(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		text := genMapText(r)
		if i%2 == 1 {
			text = mutateText(r, text)
		}
		m, err := Parse(text, Options{})
		if err != nil {
			if m != nil && GradeOf(err) != GradePartial {
				t.Fatalf("non-nil map with %v", err)
			}
			continue
		}
		checkMap(t, m)
	}
}

func checkMap(t testing.TB, m *Map) {
	t.Helper()
	for i, x := range m.Mappings {
		if i > 0 && lessPos(x, m.Mappings[i-1]) {
			t.Fatalf("mappings unsorted at %d", i)
		}
		if x.GenLine < 0 || x.GenCol < 0 || int(x.GenLine) >= m.Lines {
			t.Fatalf("mapping %v out of range (lines %d)", x, m.Lines)
		}
		if x.Source >= int32(len(m.Sources)) || x.Name >= int32(len(m.Names)) || (x.Source < 0 && x.Name >= 0) {
			t.Fatalf("mapping %v index out of range", x)
		}
		if x.Source >= 0 && (x.OrigLine < 0 || x.OrigCol < 0) {
			t.Fatalf("negative original position %v", x)
		}
	}
}

// genMapText builds a regular or index map JSON text.
func genMapText(r core.Rng) []byte {
	regular := func(ms []Mapping, lines, nSrc, nNames int) map[string]any {
		s, _ := EncodeMappings(ms, lines)
		srcs := make([]any, nSrc)
		content := make([]any, nSrc)
		for i := range srcs {
			srcs[i] = fmt.Sprintf("src/%d.ts", i)
			if core.IntN(r, 2) == 0 {
				content[i] = "code " + fmt.Sprint(i)
			}
		}
		names := make([]string, nNames)
		for i := range names {
			names[i] = fmt.Sprintf("n%d", i)
		}
		return map[string]any{"version": 3, "sources": srcs, "sourcesContent": content, "names": names, "mappings": s}
	}
	if core.IntN(r, 3) > 0 {
		ms, lines, nSrc, nNames := genMappings(r)
		b, _ := json.Marshal(regular(ms, lines, nSrc, nNames))
		return b
	}
	var sections []any
	line := 0
	for k := core.IntN(r, 4) + 1; k > 0; k-- {
		ms, lines, nSrc, nNames := genMappings(r)
		for j := range ms {
			ms[j].GenCol %= 1000
		}
		sections = append(sections, map[string]any{"offset": map[string]int{"line": line, "column": core.IntN(r, 5)}, "map": regular(ms, lines, nSrc, nNames)})
		line += lines + core.IntN(r, 2)
	}
	b, _ := json.Marshal(map[string]any{"version": 3, "sections": sections})
	return b
}

func mutateText(r core.Rng, b []byte) []byte {
	out := append([]byte(nil), b...)
	if len(out) == 0 {
		return out
	}
	i := core.IntN(r, len(out))
	alphabet := []byte("AB/+gC,;\"{}[]:0-9 ")
	out[i] = alphabet[core.IntN(r, len(alphabet))]
	return out
}

// TestSMapInv05_IndexMapAgreesWithSections: looking up a position in a
// flattened index map gives the same original position as looking it up
// (offset-adjusted) in the section's own map.
func TestSMapInv05_IndexMapAgreesWithSections(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases()/2; i++ {
		var sections []any
		type sec struct {
			line, col int32
			m         *Map
			srcBase   int32
		}
		var secs []sec
		line, srcBase := 0, 0
		for k := core.IntN(r, 3) + 1; k > 0; k-- {
			ms, lines, nSrc, nNames := genMappings(r)
			for j := range ms {
				ms[j].GenCol %= 200
			}
			sub := mapFrom(t, ms, lines, nSrc, nNames)
			s, _ := EncodeMappings(ms, lines)
			col := core.IntN(r, 5)
			srcs := make([]string, nSrc)
			for j := range srcs {
				srcs[j] = fmt.Sprintf("s%d.js", j)
			}
			names := make([]string, nNames)
			sections = append(sections, map[string]any{"offset": map[string]int{"line": line, "column": col},
				"map": map[string]any{"version": 3, "sources": srcs, "names": names, "mappings": s}})
			secs = append(secs, sec{int32(line), int32(col), sub, int32(srcBase)})
			line += lines
			srcBase += nSrc
		}
		text, _ := json.Marshal(map[string]any{"version": 3, "sections": sections})
		idx, err := Parse(text, Options{})
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		checkMap(t, idx)
		for k := 0; k < 40; k++ {
			s := secs[core.IntN(r, len(secs))]
			l, c := int32(core.IntN(r, s.m.Lines)), int32(core.IntN(r, 220))
			want, wok := s.m.OriginalPositionFor(l, c)
			gl, gc := s.line+l, c
			if l == 0 {
				gc += s.col
			}
			got, ok := idx.OriginalPositionFor(gl, gc)
			if wok != ok || (ok && (got.OrigLine != want.OrigLine || got.OrigCol != want.OrigCol ||
				(want.Source >= 0) != (got.Source >= 0) || (want.Source >= 0 && got.Source != want.Source+s.srcBase))) {
				t.Fatalf("section lookup (%d,%d): %v %v, flattened (%d,%d): %v %v", l, c, want, wok, gl, gc, got, ok)
			}
		}
	}
}

// TestSMapInv06_LimitsGradePartial: an input or source limit hit grades
// the result partial, never complete, and the partial map still satisfies
// SMAP-INV-04.
func TestSMapInv06_LimitsGradePartial(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		text := genMapText(r)
		opts := Options{MaxInputBytes: core.IntN(r, len(text)+50) + 1, MaxSources: core.IntN(r, 6) + 1}
		m, err := Parse(text, opts)
		full, ferr := Parse(text, Options{})
		if ferr != nil {
			t.Fatalf("generated map invalid: %v", ferr)
		}
		over := len(text) > opts.MaxInputBytes || len(full.Sources) > opts.MaxSources
		if over != (GradeOf(err) == GradePartial) {
			t.Fatalf("over limit %v, grade %s (%v)", over, GradeOf(err), err)
		}
		if m != nil {
			checkMap(t, m)
			if len(m.Sources) > opts.MaxSources {
				t.Fatalf("%d sources over %d", len(m.Sources), opts.MaxSources)
			}
		}
	}
}

// TestSMapInv07_LinearTime: decoding time per byte at 4n stays within
// 1.5x of n, and allocations grow at most linearly.
func TestSMapInv07_LinearTime(t *testing.T) {
	small, big := genBundleMap(1<<18), genBundleMap(1<<20)
	as := testing.AllocsPerRun(2, func() { Parse(small, Options{}) })
	ab := testing.AllocsPerRun(2, func() { Parse(big, Options{}) })
	if ab > 4.4*as*float64(len(big))/float64(4*len(small)) {
		t.Fatalf("allocations %.0f -> %.0f", as, ab)
	}
	if testing.Short() || raceEnabled {
		t.Skip("timing half skipped in -short and -race runs")
	}
	per := func(b []byte) float64 {
		best := time.Duration(1 << 62)
		for i := 0; i < 5; i++ {
			s := time.Now()
			Parse(b, Options{})
			best = min(best, time.Since(s))
		}
		return float64(best) / float64(len(b))
	}
	for attempt := 0; ; attempt++ {
		a, b := per(small), per(big)
		if b <= 1.5*a+1 {
			return
		}
		if attempt == 2 {
			t.Fatalf("per-byte cost grew from %.2f to %.2f ns", a, b)
		}
	}
}

// genBundleMap builds a bundle-like source map of about n bytes.
func genBundleMap(n int) []byte {
	r := core.NewSplitMix64(0x534d4150) // "SMAP"
	var ms []Mapping
	line := int32(0)
	var s string
	for len(s) < n {
		col := int32(0)
		for k := 0; k < 2000; k++ {
			col += int32(core.IntN(r, 40)) + 1
			ms = append(ms, Mapping{GenLine: line, GenCol: col, Source: int32(core.IntN(r, 50)),
				OrigLine: int32(core.IntN(r, 3000)), OrigCol: int32(core.IntN(r, 80)), Name: int32(core.IntN(r, 100))})
		}
		line++
		s, _ = EncodeMappings(ms, int(line))
	}
	srcs := make([]string, 50)
	for i := range srcs {
		srcs[i] = fmt.Sprintf("webpack://app/src/m%d.js", i)
	}
	names := make([]string, 100)
	for i := range names {
		names[i] = fmt.Sprintf("name%d", i)
	}
	b, _ := json.Marshal(map[string]any{"version": 3, "file": "bundle.js", "sources": srcs, "names": names, "mappings": s})
	return b
}

// FuzzSourceMap asserts totality, SMAP-INV-04 on every accepted map and
// the grade rules, and SMAP-INV-02 on accepted canonical mappings.
func FuzzSourceMap(f *testing.F) {
	f.Add([]byte(`{"version":3,"sources":["a.js"],"names":["x"],"mappings":"AAAAA,CAAC;;AACA"}`))
	f.Add([]byte(`{"version":3,"sections":[{"offset":{"line":0,"column":0},"map":{"version":3,"sources":[],"mappings":"A"}}]}`))
	f.Add([]byte(")]}'\n{\"version\":3,\"sources\":[],\"mappings\":\";;\"}"))
	f.Fuzz(func(t *testing.T, text []byte) {
		m, err := Parse(text, Options{MaxSources: 8})
		if err != nil {
			var e *Error
			if !strings.HasPrefix(err.Error(), "sourcemap: ") || !errorsAs(err, &e) {
				t.Fatalf("unexpected error %v", err)
			}
			if m != nil && GradeOf(err) != GradePartial {
				t.Fatalf("map returned with %v", err)
			}
		}
		if m != nil {
			checkMap(t, m)
		}
		// The mappings decoder alone, on the raw text.
		ms, lines, derr := DecodeMappings(string(text), -1, -1)
		if derr == nil {
			s, eerr := EncodeMappings(ms, lines)
			if eerr != nil {
				t.Fatalf("re-encode: %v", eerr)
			}
			if isCanonical(string(text)) && s != string(text) {
				t.Fatalf("canonical %q re-encoded as %q", text, s)
			}
			back, _, berr := DecodeMappings(s, -1, -1)
			if berr != nil || fmt.Sprint(back) != fmt.Sprint(ms) {
				t.Fatalf("decode(encode) differs")
			}
		}
	})
}

func errorsAs(err error, e **Error) bool {
	x, ok := err.(*Error)
	*e = x
	return ok
}

// BenchmarkSMap01_Parse measures map decoding at n and 4n.
func BenchmarkSMap01_Parse(b *testing.B) {
	for _, n := range []int{256 << 10, 1 << 20} {
		text := genBundleMap(n)
		b.Run(fmt.Sprintf("%dKiB", n>>10), func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Parse(text, Options{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkSMap02_Lookup measures OriginalPositionFor.
func BenchmarkSMap02_Lookup(b *testing.B) {
	m, err := Parse(genBundleMap(1<<20), Options{})
	if err != nil {
		b.Fatal(err)
	}
	r := core.NewSplitMix64(1)
	b.ReportAllocs()
	for b.Loop() {
		m.OriginalPositionFor(int32(core.IntN(r, m.Lines)), int32(core.IntN(r, 40000)))
	}
}
