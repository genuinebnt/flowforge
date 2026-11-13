package sitemap

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/xmlparse"
)

// Namespace is the sitemap protocol 0.9 namespace.
const Namespace = "http://www.sitemaps.org/schemas/sitemap/0.9"

// Limits. The protocol caps a sitemap at 50,000 entries and 50 MiB
// (52,428,800 bytes) uncompressed; the defaults follow the v2 assurance
// bounds (spec 26.4, 29.5: 20,000 facts, 8 MiB per document) and options
// can raise them up to, never past, the protocol limits.
const (
	ProtocolMaxEntries = 50000
	ProtocolMaxBytes   = 52428800
	DefaultMaxEntries  = 20000
	DefaultMaxBytes    = 8 << 20
	// MaxLocLength is the protocol's limit on a <loc> value (fewer than
	// 2,048 characters).
	MaxLocLength = 2047
)

// Options configure Parse and ParseText.
type Options struct {
	// MaxEntries bounds the entries read (0 = DefaultMaxEntries, clamped
	// to ProtocolMaxEntries).
	MaxEntries int
	// MaxBytes bounds the (decompressed) input (0 = DefaultMaxBytes,
	// clamped to ProtocolMaxBytes).
	MaxBytes int
	// RequireNamespace rejects documents whose root is not in Namespace.
	// By default a root without any namespace is accepted with a
	// missing_namespace diagnostic.
	RequireNamespace bool
}

func (o Options) maxEntries() int {
	if o.MaxEntries <= 0 {
		return DefaultMaxEntries
	}
	return min(o.MaxEntries, ProtocolMaxEntries)
}

func (o Options) maxBytes() int {
	if o.MaxBytes <= 0 {
		return DefaultMaxBytes
	}
	return min(o.MaxBytes, ProtocolMaxBytes)
}

// Kind is the type of sitemap.
type Kind uint8

// Sitemap kinds.
const (
	// KindUnknown: no sitemap root was read (empty, refused or not a
	// sitemap).
	KindUnknown Kind = iota
	KindURLSet       // <urlset>: entries are pages
	KindIndex        // <sitemapindex>: entries are sitemaps
	KindText         // plain-text sitemap: one URL per line
)

var kindNames = [...]string{"unknown", "urlset", "sitemapindex", "text"}

// String returns the stable name of k.
func (k Kind) String() string {
	if int(k) < len(kindNames) {
		return kindNames[k]
	}
	return "kind?"
}

// ChangeFreq is the <changefreq> value.
type ChangeFreq uint8

// ChangeFreq values: the protocol's seven, plus ChangeFreqNone for an
// absent or invalid field.
const (
	ChangeFreqNone ChangeFreq = iota
	ChangeFreqAlways
	ChangeFreqHourly
	ChangeFreqDaily
	ChangeFreqWeekly
	ChangeFreqMonthly
	ChangeFreqYearly
	ChangeFreqNever
)

var changeFreqNames = [...]string{"", "always", "hourly", "daily", "weekly", "monthly", "yearly", "never"}

// String returns the protocol spelling of c ("" for none).
func (c ChangeFreq) String() string {
	if int(c) < len(changeFreqNames) {
		return changeFreqNames[c]
	}
	return "changefreq?"
}

// Entry is one <url> (urlset), <sitemap> (index) or text line.
type Entry struct {
	// Loc is the <loc> URL as written (whitespace-trimmed): absolute
	// http(s), fewer than 2,048 characters.
	Loc string
	// LastMod is the W3C Datetime value in UTC; zero when absent or
	// invalid.
	LastMod    time.Time
	ChangeFreq ChangeFreq
	// Priority is in [0, 1]; HasPriority tells 0.0 from absent.
	Priority    float64
	HasPriority bool
	// Start and End are the entry's byte span (in the decompressed input).
	Start, End int32
}

// Code is a stable diagnostic code.
type Code uint8

// Diagnostic codes; each says what happened to the entry or field.
const (
	CodeNone              Code = iota
	CodeXMLError               // the XML was not well-formed or was refused (see Sitemap.XMLErr)
	CodeGzipInvalid            // gzip header or stream corrupt
	CodeNotSitemap             // root is neither urlset nor sitemapindex
	CodeMissingNamespace       // root has no namespace (accepted unless RequireNamespace)
	CodeMissingLoc             // entry without <loc>: dropped
	CodeInvalidLoc             // <loc> not an absolute http(s) URL or too long: entry dropped
	CodeInvalidLastMod         // <lastmod> not a W3C Datetime: field ignored
	CodeInvalidChangeFreq      // unknown <changefreq>: field ignored
	CodeInvalidPriority        // <priority> not a number in [0, 1]: field ignored
	CodeDuplicateField         // a field repeated in one entry: the first is kept
	CodeLimitExceeded          // more entries than MaxEntries: the rest were not read
	CodeInputTooLarge          // (decompressed) input over MaxBytes: the rest was not read
	CodeInvalidUTF8            // a text sitemap line that is not UTF-8: skipped
)

var codeNames = [...]string{
	"none", "xml_error", "gzip_invalid", "not_sitemap", "missing_namespace",
	"missing_loc", "invalid_loc", "invalid_lastmod", "invalid_changefreq",
	"invalid_priority", "duplicate_field", "limit_exceeded", "input_too_large",
	"invalid_utf8",
}

// String returns the stable name of c.
func (c Code) String() string {
	if int(c) < len(codeNames) {
		return codeNames[c]
	}
	return "code(" + strconv.Itoa(int(c)) + ")"
}

// Diag is one diagnostic.
type Diag struct {
	Code   Code
	Offset int // byte offset in the (decompressed) input
}

// Sitemap is the result of reading one sitemap.
type Sitemap struct {
	Kind    Kind
	Entries []Entry // valid entries, in document order
	Diags   []Diag  // in document order
	// Grade is complete, partial (a limit), unsupported (not a sitemap,
	// or the XML refused) or failed (malformed XML or gzip).
	Grade      xmlparse.Grade
	Compressed bool // the input was gzip
	// XMLErr is the XML error behind CodeXMLError (or a limit), if any.
	XMLErr *xmlparse.SyntaxError
}

func (s *Sitemap) diag(c Code, off int) { s.Diags = append(s.Diags, Diag{Code: c, Offset: off}) }

// decompress gunzips data when it starts with the gzip magic, reading at
// most max+1 bytes (one past the limit proves the input is too large);
// other data is returned as is. false means s already holds the failure.
func decompress(data []byte, max int, s *Sitemap) ([]byte, bool) {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return data, true
	}
	s.Compressed = true
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		s.diag(CodeGzipInvalid, 0)
		s.Grade = xmlparse.GradeFailed
		return nil, false
	}
	// Never inflate more than one byte past the limit.
	out, err := io.ReadAll(io.LimitReader(zr, int64(max)+1))
	if err != nil {
		s.diag(CodeGzipInvalid, len(out))
		s.Grade = xmlparse.GradeFailed
		return nil, false
	}
	return out, true
}

// state is the sitemap reader state over XML events (sitemaps.org 0.9
// schema: urlset > url > loc | lastmod | changefreq | priority, and
// sitemapindex > sitemap > loc | lastmod).
type state uint8

const (
	stateRoot  state = iota // expecting the root element
	stateTop                // inside urlset / sitemapindex
	stateEntry              // inside url / sitemap
	stateField              // inside loc / lastmod / changefreq / priority
	stateSkip               // inside an element we do not read (extensions)
)

// Parse reads an XML sitemap or sitemap index, gzip-compressed or not.
// Unknown and extension elements are skipped; it never fails as a whole
// (the result's Grade and Diags say what was read).
func Parse(data []byte, opts Options) *Sitemap {
	s := &Sitemap{}
	maxBytes, maxEntries := opts.maxBytes(), opts.maxEntries()
	src, ok := decompress(data, maxBytes, s)
	if !ok {
		return s
	}
	dec := xmlparse.NewDecoder(src, xmlparse.Options{MaxInputBytes: maxBytes})
	st := stateRoot
	var resume state // state to return to after stateSkip
	skipDepth := 0
	entryName := ""
	var cur Entry
	var seen [5]bool
	field := ""
	var text strings.Builder
	inNS := func(n xmlparse.Name) bool {
		return n.Space == Namespace || (!opts.RequireNamespace && n.Space == "" && s.missingNS())
	}
	for {
		ev, err := dec.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			se := err.(*xmlparse.SyntaxError)
			s.XMLErr = se
			switch se.Code {
			case xmlparse.ErrInputTooLarge:
				s.diag(CodeInputTooLarge, se.Offset)
			default:
				s.diag(CodeXMLError, se.Offset)
			}
			s.Grade = se.Code.Grade()
			return s
		}
		switch st {
		case stateRoot:
			if ev.Kind != xmlparse.EventStartElement {
				continue
			}
			switch {
			case ev.Name.Space == Namespace:
			case ev.Name.Space == "" && !opts.RequireNamespace:
				s.diag(CodeMissingNamespace, int(ev.Start))
			default:
				s.diag(CodeNotSitemap, int(ev.Start))
				s.Grade = xmlparse.GradeUnsupported
				return s
			}
			switch ev.Name.Local {
			case "urlset":
				s.Kind, entryName = KindURLSet, "url"
			case "sitemapindex":
				s.Kind, entryName = KindIndex, "sitemap"
			default:
				s.Diags = s.Diags[:0]
				s.diag(CodeNotSitemap, int(ev.Start))
				s.Grade = xmlparse.GradeUnsupported
				return s
			}
			st = stateTop
		case stateTop:
			switch ev.Kind {
			case xmlparse.EventStartElement:
				if inNS(ev.Name) && ev.Name.Local == entryName {
					if len(s.Entries) >= maxEntries {
						s.diag(CodeLimitExceeded, int(ev.Start))
						s.Grade = xmlparse.GradePartial
						return s
					}
					cur = Entry{Start: ev.Start}
					seen = [5]bool{}
					st = stateEntry
				} else {
					resume, skipDepth, st = stateTop, 1, stateSkip
				}
			case xmlparse.EventEndElement:
				st = stateRoot // root closed; the decoder checks the epilog
			}
		case stateEntry:
			switch ev.Kind {
			case xmlparse.EventStartElement:
				k := fieldIndex(ev.Name.Local)
				if !inNS(ev.Name) || k < 0 || (s.Kind == KindIndex && k > 2) {
					resume, skipDepth, st = stateEntry, 1, stateSkip
					continue
				}
				if seen[k] {
					s.diag(CodeDuplicateField, int(ev.Start))
					resume, skipDepth, st = stateEntry, 1, stateSkip
					continue
				}
				seen[k] = true
				field = ev.Name.Local
				text.Reset()
				st = stateField
			case xmlparse.EventEndElement:
				cur.End = ev.End
				switch {
				case !seen[1]:
					s.diag(CodeMissingLoc, int(cur.Start))
				case cur.Loc == "":
					// invalid loc already reported
				default:
					s.Entries = append(s.Entries, cur)
				}
				st = stateTop
			}
		case stateField:
			switch ev.Kind {
			case xmlparse.EventText, xmlparse.EventCData:
				text.WriteString(ev.Text)
			case xmlparse.EventStartElement:
				resume, skipDepth, st = stateField, 1, stateSkip
			case xmlparse.EventEndElement:
				applyField(s, &cur, field, trimSpace(text.String()), int(ev.Start))
				st = stateEntry
			}
		case stateSkip:
			switch ev.Kind {
			case xmlparse.EventStartElement:
				skipDepth++
			case xmlparse.EventEndElement:
				skipDepth--
				if skipDepth == 0 {
					st = resume
				}
			}
		}
	}
	if s.Kind == KindUnknown {
		s.diag(CodeNotSitemap, 0)
		s.Grade = xmlparse.GradeUnsupported
	}
	return s
}

func (s *Sitemap) missingNS() bool {
	for _, d := range s.Diags {
		if d.Code == CodeMissingNamespace {
			return true
		}
	}
	return false
}

// fieldIndex numbers the entry fields for the duplicate check (1 loc, 2
// lastmod, 3 changefreq, 4 priority; index entries take 1 and 2 only).
func fieldIndex(local string) int {
	switch local {
	case "lastmod":
		return 2
	case "loc":
		return 1
	case "changefreq":
		return 3
	case "priority":
		return 4
	}
	return -1
}

func trimSpace(s string) string { return strings.Trim(s, " \t\r\n") }

// applyField validates one field value v (at offset off) into e.
func applyField(s *Sitemap, e *Entry, field, v string, off int) {
	switch field {
	case "loc":
		if validLoc(v) {
			e.Loc = v
		} else {
			s.diag(CodeInvalidLoc, off)
		}
	case "lastmod":
		if t, ok := ParseW3CDatetime(v); ok {
			e.LastMod = t
		} else {
			s.diag(CodeInvalidLastMod, off)
		}
	case "changefreq":
		for i := ChangeFreqAlways; i <= ChangeFreqNever; i++ {
			if strings.EqualFold(v, changeFreqNames[i]) {
				e.ChangeFreq = i
				return
			}
		}
		s.diag(CodeInvalidChangeFreq, off)
	case "priority":
		p, ok := parsePriority(v)
		if !ok {
			s.diag(CodeInvalidPriority, off)
			return
		}
		e.Priority, e.HasPriority = p, true
	}
}

// validLoc reports whether v is an absolute http(s) URL with a host and
// fewer than 2,048 characters.
func validLoc(v string) bool {
	if v == "" || utf8.RuneCountInString(v) > MaxLocLength {
		return false
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Scheme, "http") || strings.EqualFold(u.Scheme, "https")
}

// parsePriority accepts digits with at most one ".", value in [0, 1].
func parsePriority(v string) (float64, bool) {
	if v == "" {
		return 0, false
	}
	dots := 0
	for i := 0; i < len(v); i++ {
		switch c := v[i]; {
		case c == '.':
			dots++
		case c < '0' || c > '9':
			return 0, false
		}
	}
	if dots > 1 || v == "." {
		return 0, false
	}
	p, err := strconv.ParseFloat(v, 64)
	if err != nil || p < 0 || p > 1 {
		return 0, false
	}
	return p, true
}

// ParseW3CDatetime parses a W3C Datetime value
// (https://www.w3.org/TR/NOTE-datetime: YYYY, YYYY-MM, YYYY-MM-DD, or a
// date with hh:mm, hh:mm:ss or hh:mm:ss.s and a TZD of Z, +hh:mm or
// -hh:mm) into UTC.
func ParseW3CDatetime(v string) (time.Time, bool) {
	var layouts []string
	switch {
	case len(v) == 4:
		layouts = []string{"2006"}
	case len(v) == 7:
		layouts = []string{"2006-01"}
	case len(v) == 10:
		layouts = []string{"2006-01-02"}
	case len(v) > 10 && v[10] == 'T':
		// PORT: time.Parse accepts a fractional second after the seconds
		// field (W3C hh:mm:ss.s); the Rust port must too.
		layouts = []string{"2006-01-02T15:04Z07:00", "2006-01-02T15:04:05Z07:00"}
	}
	for _, l := range layouts {
		if t, err := time.Parse(l, v); err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}

// ParseText reads a plain-text sitemap: UTF-8, one absolute URL per line
// (protocol §"Text file"). Blank lines are skipped; invalid lines are
// reported and skipped.
func ParseText(data []byte, opts Options) *Sitemap {
	s := &Sitemap{Kind: KindText}
	maxBytes, maxEntries := opts.maxBytes(), opts.maxEntries()
	src, ok := decompress(data, maxBytes, s)
	if !ok {
		return s
	}
	if len(src) > maxBytes {
		src = src[:maxBytes]
		// Drop the partial last line.
		if i := bytes.LastIndexAny(src, "\r\n"); i >= 0 {
			src = src[:i+1]
		} else {
			src = nil
		}
		s.diag(CodeInputTooLarge, maxBytes)
		s.Grade = xmlparse.GradePartial
	}
	if bytes.HasPrefix(src, []byte("\xef\xbb\xbf")) {
		src = src[3:]
	}
	off := 0
	for off < len(src) {
		end := bytes.IndexAny(src[off:], "\r\n")
		next := len(src)
		if end >= 0 {
			end += off
			next = end + 1
			if src[end] == '\r' && next < len(src) && src[next] == '\n' {
				next++
			}
		} else {
			end = len(src)
		}
		line := trimSpace(string(src[off:end]))
		switch {
		case line == "":
		case !utf8.ValidString(line):
			s.diag(CodeInvalidUTF8, off)
		case !validLoc(line):
			s.diag(CodeInvalidLoc, off)
		default:
			if len(s.Entries) >= maxEntries {
				s.diag(CodeLimitExceeded, off)
				s.Grade = xmlparse.GradePartial
				return s
			}
			s.Entries = append(s.Entries, Entry{Loc: line, Start: int32(off), End: int32(end)})
		}
		off = next
	}
	return s
}
