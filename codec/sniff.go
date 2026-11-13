package codec

import "unicode/utf8"

// Sniff defaults.
const (
	// DefaultMinBase64 is the shortest base64 run reported, in data
	// characters (18 bytes decoded): shorter runs are mostly identifiers
	// and words.
	DefaultMinBase64 = 24
	// DefaultMinHex is the shortest hex run reported (16 bytes decoded).
	DefaultMinHex = 32
	// DefaultMaxCandidates bounds the candidates of one Sniff call.
	DefaultMaxCandidates = 1024
	// mimeMinLine is the shortest line of a wrapped base64 block that a
	// line break may continue: PEM uses 64 characters, MIME 76.
	mimeMinLine = 60
	// minDistinctBase64 is how many distinct characters a base64 run
	// needs: random data of 24 characters shows about 20 of 64, English
	// words and identifiers far fewer.
	minDistinctBase64 = 16
)

// SniffOptions tunes Sniff. Zero fields take the defaults.
type SniffOptions struct {
	MinBase64     int
	MinHex        int
	MaxCandidates int
}

func (o SniffOptions) withDefaults() SniffOptions {
	if o.MinBase64 <= 0 {
		o.MinBase64 = DefaultMinBase64
	}
	if o.MinHex <= 0 {
		o.MinHex = DefaultMinHex
	}
	if o.MaxCandidates <= 0 {
		o.MaxCandidates = DefaultMaxCandidates
	}
	return o
}

// Candidate is one likely-encoded span b[Start:End] of a sniffed buffer.
type Candidate struct {
	Kind       Kind
	Start, End int
}

// Sniff finds likely-encoded blobs in b, in order of Start, without
// overlaps:
//
//   - b as a whole when it starts with gzip, zlib or zstd magic
//     (DetectCompression) and its first bytes inflate; nothing else is
//     reported then;
//   - maximal runs of hex digits of at least MinHex characters, even,
//     with both digits and letters (a long number is not hex);
//   - maximal runs of base64 characters of at least MinBase64 data
//     characters, optional '=' padding, one alphabet only, with upper-
//     and lower-case letters, a digit or one of + / - _ (camel-case
//     identifiers have none), and at least 16 distinct characters, that
//     a lenient decode accepts; a run continues across a line break when
//     the line so far is at least 60 characters and a multiple of 4
//     (MIME and PEM wrapping).
//
// Sniff is total and deterministic (CODEC-INV-09).
func Sniff(b []byte, o SniffOptions) []Candidate {
	cands := sniffRaw(b, o.withDefaults())
	out := cands[:0]
	for _, c := range cands {
		if candidateDecodes(b, c) {
			out = append(out, c)
		}
	}
	return out
}

// candidateDecodes checks a candidate with the decoder DecodeLayers uses.
func candidateDecodes(b []byte, c Candidate) bool {
	span := b[c.Start:c.End]
	switch c.Kind {
	case KindHex:
		_, err := AppendDecodeHex(nil, span, HexOptions{Mode: Lenient})
		return err == nil
	case KindBase64, KindBase64URL:
		_, err := AppendDecodeBase64(nil, span, Base64Options{Mode: Lenient})
		return err == nil
	}
	// A compressed stream: a valid header and first bytes (a 1-byte cap
	// stops at once on any healthy stream).
	_, err := Inflate(span, c.Kind, InflateOptions{MaxOutput: 1, MaxRatio: -1})
	return err == nil || CodeOf(err) == CodeOutputLimit
}

// runClass accumulates what a run of base64-alphabet characters contains.
type runClass struct {
	data          int // alphabet characters (no '=' or line breaks)
	pad           int
	upper, lower  bool
	digit, letter bool // digit, and a letter a-f / A-F
	nonHex        bool // a character outside [0-9A-Fa-f]
	std, url      bool // '+' or '/', '-' or '_'
	wrapped       bool // contains line breaks
	seen          [2]uint64
	distinct      int
}

func (r *runClass) add(c byte) {
	r.data++
	switch {
	case c >= 'A' && c <= 'Z':
		r.upper = true
		r.letter = r.letter || c <= 'F'
		r.nonHex = r.nonHex || c > 'F'
	case c >= 'a' && c <= 'z':
		r.lower = true
		r.letter = r.letter || c <= 'f'
		r.nonHex = r.nonHex || c > 'f'
	case c >= '0' && c <= '9':
		r.digit = true
	case c == '+' || c == '/':
		r.std, r.nonHex = true, true
	default:
		r.url, r.nonHex = true, true
	}
	if r.seen[c>>6]&(1<<(c&63)) == 0 {
		r.seen[c>>6] |= 1 << (c & 63)
		r.distinct++
	}
}

func isB64Char(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '-' || c == '_'
}

// sniffState is the run scanner's state.
type sniffState uint8

const (
	// sniffOutside: between runs.
	sniffOutside sniffState = iota
	// sniffRun: inside a run of alphabet characters.
	sniffRun
	// sniffPad: '=' padding after a run.
	sniffPad
)

func sniffRaw(b []byte, o SniffOptions) []Candidate {
	if k, ok := DetectCompression(b); ok {
		return []Candidate{{Kind: k, Start: 0, End: len(b)}}
	}
	var out []Candidate
	state := sniffOutside
	var r runClass
	start, lineStart := 0, 0
	end := func(i int) {
		if c, ok := classify(&r, start, i, o); ok && len(out) < o.MaxCandidates {
			out = append(out, c)
		}
		state = sniffOutside
	}
	for i := 0; i < len(b); i++ {
		c := b[i]
		switch state {
		case sniffOutside:
			if isB64Char(c) {
				r = runClass{}
				r.add(c)
				state, start, lineStart = sniffRun, i, i
			}
		case sniffRun:
			switch {
			case isB64Char(c):
				r.add(c)
			case c == '=':
				r.pad++
				state = sniffPad
			case (c == '\n' || c == '\r' && i+1 < len(b) && b[i+1] == '\n') && i-lineStart >= mimeMinLine &&
				(i-lineStart)%4 == 0 && continuesRun(b, i):
				// A wrapped line: skip the break, the run goes on.
				if c == '\r' {
					i++
				}
				r.wrapped = true
				lineStart = i + 1
			default:
				end(i)
			}
		case sniffPad:
			if c == '=' && r.pad < 2 {
				r.pad++
				continue
			}
			end(i)
			i-- // the byte after the padding may start a run
		}
	}
	if state != sniffOutside {
		end(len(b))
	}
	return out
}

// continuesRun reports that the line after the break at i starts like
// base64 data. '-' and '_' do not count: a PEM footer ("-----END") must
// not join the block.
func continuesRun(b []byte, i int) bool {
	if b[i] == '\r' {
		i++
	}
	return i+1 < len(b) && isB64Char(b[i+1]) && b[i+1] != '-' && b[i+1] != '_'
}

// classify decides what a finished run b[start:end] is.
func classify(r *runClass, start, end int, o SniffOptions) (Candidate, bool) {
	if !r.nonHex && r.pad == 0 && !r.wrapped {
		if r.data >= o.MinHex && r.data%2 == 0 && r.digit && r.letter {
			return Candidate{Kind: KindHex, Start: start, End: end}, true
		}
		return Candidate{}, false
	}
	if r.data < o.MinBase64 || r.std && r.url || !r.upper || !r.lower || !(r.digit || r.std || r.url) ||
		r.distinct < minDistinctBase64 ||
		r.data%4 == 1 || r.pad > 0 && (r.data+r.pad)%4 != 0 {
		return Candidate{}, false
	}
	k := KindBase64
	if r.url {
		k = KindBase64URL
	}
	return Candidate{Kind: k, Start: start, End: end}, true
}

// LooksLikeText reports whether b is non-empty UTF-8 text: valid UTF-8
// with no control characters other than HT, LF, FF and CR. DecodeLayers
// keeps a decoded layer only when it is text or compressed.
func LooksLikeText(b []byte) bool {
	if len(b) == 0 || !utf8.Valid(b) {
		return false
	}
	for _, c := range b {
		if c < 0x20 && c != '\t' && c != '\n' && c != '\f' && c != '\r' || c == 0x7F {
			return false
		}
	}
	return true
}
