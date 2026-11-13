package sourcemap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Options configure Parse. The zero value uses the defaults.
type Options struct {
	// MaxInputBytes bounds the JSON text (0 = DefaultMaxInputBytes).
	MaxInputBytes int
	// MaxSources bounds the sources kept (0 = DefaultMaxSources); mappings
	// into dropped sources are dropped too.
	MaxSources int
}

// Default limits, identical in every port (spec 26.4, 29.6).
const (
	DefaultMaxInputBytes = 8 << 20
	DefaultMaxSources    = 20000
)

// Code is a stable error code; the list (spec 40.1) is closed and String
// gives the stable names.
type Code uint8

// Error codes. CodeNone is the zero value and never appears in an Error.
const (
	CodeNone                 Code = iota
	CodeInvalidJSON               // not JSON text
	CodeInvalidField              // a known field with the wrong JSON type
	CodeUnsupportedVersion        // "version" is not the number 3
	CodeMissingMappings           // a regular map without "mappings"
	CodeMissingSources            // a regular map without "sources"
	CodeAmbiguousMap              // both "sections" and "mappings"/"sources"
	CodeInvalidVLQChar            // a byte outside the Base64 alphabet and separators
	CodeVLQTruncated              // a VLQ ending in a continuation digit
	CodeVLQOverflow               // a VLQ, line count or section-shifted position beyond 32 bits
	CodeInvalidSegment            // a segment with 0 (between commas), 2, 3 or more than 5 fields
	CodeIndexOutOfRange           // a source or name index past the list
	CodeNegativeValue             // a column, line or index that resolves below 0 or above 2^31-1
	CodeInvalidSectionOffset      // a section without a valid offset
	CodeSectionOrder              // section offsets not strictly increasing
	CodeSectionOverlap            // a section's mappings reach the next section's offset
	CodeNestedIndexMap            // a section whose map is itself an index map
	CodeSectionURL                // a section given by "url" (refused: no fetching)
	CodeInvalidIgnoreList         // an ignoreList entry that is not a source index
	CodeUnsortedMappings          // EncodeMappings input with decreasing GenLine
	CodeInputTooLarge             // JSON text longer than MaxInputBytes
	CodeLimitExceeded             // more sources than MaxSources
)

var codeNames = [...]string{
	"none", "invalid_json", "invalid_field", "unsupported_version",
	"missing_mappings", "missing_sources", "ambiguous_map", "invalid_vlq_char",
	"vlq_truncated", "vlq_overflow", "invalid_segment", "index_out_of_range",
	"negative_value", "invalid_section_offset", "section_order",
	"section_overlap", "nested_index_map", "section_url", "invalid_ignore_list",
	"unsorted_mappings", "input_too_large", "limit_exceeded",
}

// String returns the stable name of c.
func (c Code) String() string {
	if int(c) < len(codeNames) {
		return codeNames[c]
	}
	return "code(" + strconv.Itoa(int(c)) + ")"
}

// Grade maps c to the grade of a result that stopped with it.
func (c Code) Grade() Grade {
	switch c {
	case CodeNone:
		return GradeComplete
	case CodeInputTooLarge, CodeLimitExceeded:
		return GradePartial
	case CodeSectionURL:
		return GradeUnsupported
	}
	return GradeFailed
}

// Error is a source map error. Offset is a byte offset into the
// "mappings" string for VLQ errors, the mapping index for EncodeMappings
// errors, and -1 otherwise; Section is the section index in an index map,
// or -1.
type Error struct {
	Code    Code
	Offset  int
	Section int
	Detail  string
}

func errAt(c Code, off int) *Error { return &Error{Code: c, Offset: off, Section: -1} }

func errf(c Code, format string, args ...any) *Error {
	return &Error{Code: c, Offset: -1, Section: -1, Detail: fmt.Sprintf(format, args...)}
}

// Error returns "sourcemap: <code>" with the section, offset and detail
// where set.
func (e *Error) Error() string {
	s := "sourcemap: " + e.Code.String()
	if e.Section >= 0 {
		s += fmt.Sprintf(" in section %d", e.Section)
	}
	if e.Offset >= 0 {
		s += fmt.Sprintf(" at %d", e.Offset)
	}
	if e.Detail != "" {
		s += ": " + e.Detail
	}
	return s
}

// Grade says how completely a result reflects its input (spec 26.4). The
// same four grades exist in cssparse, xmlparse and wellknown.
type Grade uint8

// Grades, in order of decreasing completeness.
const (
	GradeComplete    Grade = iota // the whole map was read
	GradePartial                  // a limit cut the read (the map may be nil for input_too_large)
	GradeUnsupported              // a refused feature (sections by URL)
	GradeFailed                   // the map is invalid
)

var gradeNames = [...]string{"complete", "partial", "unsupported", "failed"}

// String returns the stable name of g.
func (g Grade) String() string {
	if int(g) < len(gradeNames) {
		return gradeNames[g]
	}
	return "grade(" + strconv.Itoa(int(g)) + ")"
}

// GradeOf returns the grade of a Parse result that ended with err.
func GradeOf(err error) Grade {
	if err == nil {
		return GradeComplete
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Code.Grade()
	}
	return GradeFailed
}

// Source is one entry of "sources" with its "sourcesContent".
type Source struct {
	// URL is the source as written ("" when Null). For a section of an
	// index map the section's own sourceRoot is already joined in.
	URL string
	// Null is set for a JSON null entry of "sources".
	Null bool
	// Content is the embedded source text when HasContent; HasContent is
	// false for a null or missing "sourcesContent" entry.
	Content    string
	HasContent bool
	// Ignored is set when the source is listed in "ignoreList" (or the
	// legacy "x_google_ignoreList").
	Ignored bool
}

// Map is a decoded source map. Index maps are flattened: sources and
// names of all sections are concatenated in section order and mapping
// positions are shifted by the section offsets.
type Map struct {
	// File is the top-level "file" ("" when absent).
	File string
	// SourceRoot is the top-level "sourceRoot", applied by ResolveSources
	// (always "" for an index map, whose sections' roots are applied to
	// their own sources while flattening).
	SourceRoot string
	// Sources and Names are indexed by Mapping.Source and Mapping.Name.
	Sources []Source
	Names   []string
	// Mappings are sorted by (GenLine, GenCol), stable, with all indices
	// in range (SMAP-INV-04).
	Mappings []Mapping
	// Lines is the number of generated lines the mappings span.
	Lines int
	// Sections is the number of sections of an index map (0 for a
	// regular map).
	Sections int
	// Grade is complete unless a limit dropped sources.
	Grade Grade
}

// rawMap is the JSON shape of ECMA-426 §"Source map format". Pointers and
// RawMessage tell "absent" from "null" and check types strictly.
type rawMap struct {
	Version           json.RawMessage `json:"version"`
	File              *string         `json:"file"`
	SourceRoot        *string         `json:"sourceRoot"`
	Sources           *[]*string      `json:"sources"`
	SourcesContent    []*string       `json:"sourcesContent"`
	Names             []string        `json:"names"`
	Mappings          *string         `json:"mappings"`
	IgnoreList        []int           `json:"ignoreList"`
	XGoogleIgnoreList []int           `json:"x_google_ignoreList"`
	Sections          *[]rawSection   `json:"sections"`
}

type rawSection struct {
	Offset *struct {
		Line   *int `json:"line"`
		Column *int `json:"column"`
	} `json:"offset"`
	Map json.RawMessage `json:"map"`
	URL *string         `json:"url"`
}

// Parse decodes a Source Map v3 (ECMA-426) JSON text, regular or index
// map. A leading ")]}'" line (the XSSI guard of ECMA-426 §"JSON over
// HTTP transport") is skipped. With more than MaxSources sources it
// returns the truncated map (GradePartial) with a CodeLimitExceeded error;
// otherwise either a complete map and nil, or nil and the error (input
// over MaxInputBytes included: nothing is parsed then). Parse is a pure
// function and safe for concurrent use.
func Parse(data []byte, opts Options) (*Map, error) {
	maxIn := opts.MaxInputBytes
	if maxIn <= 0 {
		maxIn = DefaultMaxInputBytes
	}
	if len(data) > maxIn {
		return nil, errf(CodeInputTooLarge, "%d bytes over the %d-byte limit", len(data), maxIn)
	}
	if bytes.HasPrefix(data, []byte(")]}'")) {
		if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
			data = data[i+1:]
		} else {
			data = nil
		}
	}
	var raw rawMap
	if e := decodeJSON(data, &raw); e != nil {
		return nil, e
	}
	maxSrc := opts.MaxSources
	if maxSrc <= 0 {
		maxSrc = DefaultMaxSources
	}
	m := &Map{}
	if raw.Sections != nil {
		if e := m.fromIndex(&raw, maxSrc); e != nil {
			return nil, e
		}
	} else if e := m.fromRegular(&raw, 0, 0, -1); e != nil {
		return nil, e
	}
	if len(m.Sources) > maxSrc {
		m.truncateSources(maxSrc)
		return m, errf(CodeLimitExceeded, "more than %d sources", maxSrc)
	}
	return m, nil
}

func decodeJSON(data []byte, v *rawMap) *Error {
	// PORT: encoding/json is the JSON layer (serde_json in Rust). Unknown
	// fields are ignored, as ECMA-426 requires. For a duplicate key
	// encoding/json keeps the last value while serde's derive rejects the
	// document; the port must keep the last value (OPEN_QUESTIONS 8).
	if err := json.Unmarshal(data, v); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return errf(CodeInvalidField, "field %q: %s", te.Field, te.Value)
		}
		return errf(CodeInvalidJSON, "%v", err)
	}
	return nil
}

// checkVersion requires the JSON number 3 exactly; "3.0" and "\"3\"" are
// refused (ECMA-426: version is the integer 3).
func checkVersion(v json.RawMessage) *Error {
	if string(bytes.TrimSpace(v)) != "3" {
		return errf(CodeUnsupportedVersion, "version %s", v)
	}
	return nil
}

// fromRegular appends a regular map's sources, names and mappings,
// shifted by (lineOff, colOff) on its first line. section is -1 for a
// top-level map.
func (m *Map) fromRegular(raw *rawMap, lineOff, colOff int64, section int) *Error {
	fail := func(e *Error) *Error {
		e.Section = section
		return e
	}
	if e := checkVersion(raw.Version); e != nil {
		return fail(e)
	}
	if raw.Mappings == nil {
		return fail(errf(CodeMissingMappings, ""))
	}
	if raw.Sources == nil {
		return fail(errf(CodeMissingSources, ""))
	}
	if section < 0 {
		if raw.File != nil {
			m.File = *raw.File
		}
		if raw.SourceRoot != nil {
			m.SourceRoot = *raw.SourceRoot
		}
	}
	srcs := *raw.Sources
	ms, lines, e := DecodeMappings(*raw.Mappings, len(srcs), len(raw.Names))
	if e != nil {
		return fail(e)
	}
	base := len(m.Sources)
	for i, s := range srcs {
		src := Source{Null: s == nil}
		if s != nil {
			src.URL = *s
			if section >= 0 && raw.SourceRoot != nil && *raw.SourceRoot != "" {
				// A section's sourceRoot applies to its own sources only.
				src.URL = joinRoot(*raw.SourceRoot, src.URL)
			}
		}
		// ECMA-426: a missing or short sourcesContent means "no content".
		if i < len(raw.SourcesContent) && raw.SourcesContent[i] != nil {
			src.Content, src.HasContent = *raw.SourcesContent[i], true
		}
		m.Sources = append(m.Sources, src)
	}
	for _, list := range [][]int{raw.IgnoreList, raw.XGoogleIgnoreList} {
		for _, k := range list {
			if k < 0 || k >= len(srcs) {
				return fail(errf(CodeInvalidIgnoreList, "index %d", k))
			}
			m.Sources[base+k].Ignored = true
		}
	}
	nameBase := int32(len(m.Names))
	m.Names = append(m.Names, raw.Names...)
	for _, x := range ms {
		// ECMA-426: the column offset applies to the section's first line
		// only.
		col := int64(x.GenCol)
		if x.GenLine == 0 {
			col += colOff
		}
		if int64(x.GenLine)+lineOff > 1<<31-1 || col > 1<<31-1 {
			return fail(errf(CodeVLQOverflow, "shifted position out of range"))
		}
		x.GenCol = int32(col)
		x.GenLine += int32(lineOff)
		if x.Source >= 0 {
			x.Source += int32(base)
		}
		if x.Name >= 0 {
			x.Name += nameBase
		}
		m.Mappings = append(m.Mappings, x)
	}
	if l := int(lineOff) + lines; l > m.Lines {
		m.Lines = l
	}
	sortMappings(m.Mappings)
	return nil
}

// fromIndex flattens an index map (ECMA-426 §"Index source map").
func (m *Map) fromIndex(raw *rawMap, maxSrc int) *Error {
	if e := checkVersion(raw.Version); e != nil {
		return e
	}
	if raw.Mappings != nil || raw.Sources != nil {
		return errf(CodeAmbiguousMap, "index map with mappings or sources")
	}
	if raw.File != nil {
		m.File = *raw.File
	}
	secs := *raw.Sections
	m.Sections = len(secs)
	prevLine, prevCol := int64(-1), int64(-1)
	for i, s := range secs {
		if s.URL != nil {
			return &Error{Code: CodeSectionURL, Offset: -1, Section: i}
		}
		if s.Offset == nil || s.Offset.Line == nil || s.Offset.Column == nil || *s.Offset.Line < 0 || *s.Offset.Column < 0 ||
			*s.Offset.Line > 1<<31-1 || *s.Offset.Column > 1<<31-1 {
			return &Error{Code: CodeInvalidSectionOffset, Offset: -1, Section: i}
		}
		line, col := int64(*s.Offset.Line), int64(*s.Offset.Column)
		if line < prevLine || (line == prevLine && col <= prevCol) {
			return &Error{Code: CodeSectionOrder, Offset: -1, Section: i}
		}
		// Every mapping so far must lie before this section's offset.
		if n := len(m.Mappings); n > 0 {
			last := m.Mappings[n-1]
			if int64(last.GenLine) > line || (int64(last.GenLine) == line && int64(last.GenCol) >= col) {
				return &Error{Code: CodeSectionOverlap, Offset: -1, Section: i - 1}
			}
		}
		prevLine, prevCol = line, col
		var sub rawMap
		if e := decodeJSON(s.Map, &sub); e != nil {
			e.Section = i
			return e
		}
		if sub.Sections != nil {
			return &Error{Code: CodeNestedIndexMap, Offset: -1, Section: i}
		}
		if e := m.fromRegular(&sub, line, col, i); e != nil {
			return e
		}
		if len(m.Sources) > maxSrc {
			// Later sections are not read; Parse truncates and reports
			// limit_exceeded.
			return nil
		}
	}
	return nil
}

// truncateSources keeps the first n sources and drops mappings into the
// others.
func (m *Map) truncateSources(n int) {
	m.Sources = m.Sources[:n]
	out := m.Mappings[:0]
	for _, x := range m.Mappings {
		if x.Source < int32(n) {
			out = append(out, x)
		}
	}
	m.Mappings = out
	m.Grade = GradePartial
}

// sortMappings sorts by generated position; the common already-sorted
// case costs one pass.
func sortMappings(ms []Mapping) {
	if sort.SliceIsSorted(ms, func(i, j int) bool { return lessPos(ms[i], ms[j]) }) {
		return
	}
	// PORT: stable, so equal positions keep their encoded order (Rust:
	// slice::sort_by is stable too).
	sort.SliceStable(ms, func(i, j int) bool { return lessPos(ms[i], ms[j]) })
}

func lessPos(a, b Mapping) bool {
	if a.GenLine != b.GenLine {
		return a.GenLine < b.GenLine
	}
	return a.GenCol < b.GenCol
}

// OriginalPositionFor returns the mapping covering generated position
// (line, col), 0-based: the last mapping on that line whose GenCol is at
// most col (mozilla's GREATEST_LOWER_BOUND bias). ok is false when no
// mapping on the line starts at or before col. A 1-field mapping is
// returned as found (Source -1): that range maps to nothing.
func (m *Map) OriginalPositionFor(line, col int32) (Mapping, bool) {
	ms := m.Mappings
	// First index whose position is after (line, col).
	i := sort.Search(len(ms), func(i int) bool {
		return ms[i].GenLine > line || (ms[i].GenLine == line && ms[i].GenCol > col)
	})
	if i == 0 || ms[i-1].GenLine != line {
		return Mapping{}, false
	}
	// Among equal positions the last one wins, as in a linear scan.
	return ms[i-1], true
}

// joinRoot prepends sourceRoot to a relative source (ECMA-426 §"Resolving
// sources": the root gets a trailing "/" if it lacks one). Absolute URLs
// are left alone.
func joinRoot(root, src string) string {
	if root == "" || isAbsoluteURL(src) {
		return src
	}
	if !strings.HasSuffix(root, "/") {
		root += "/"
	}
	return root + src
}

// isAbsoluteURL reports whether s parses as a URL with a scheme.
func isAbsoluteURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != ""
}

// ResolveSources returns each source's URL with sourceRoot applied and
// resolved against mapURL (the source map's own URL; "" to skip). Null
// sources give "". Unparseable references are returned unresolved.
func (m *Map) ResolveSources(mapURL string) []string {
	var base *url.URL
	if mapURL != "" {
		if b, err := url.Parse(mapURL); err == nil {
			base = b
		}
	}
	out := make([]string, len(m.Sources))
	for i, s := range m.Sources {
		if s.Null {
			continue
		}
		u := joinRoot(m.SourceRoot, s.URL)
		if base != nil {
			if ref, err := url.Parse(u); err == nil {
				u = base.ResolveReference(ref).String()
			}
		}
		out[i] = u
	}
	return out
}
