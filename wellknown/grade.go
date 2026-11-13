package wellknown

import "strconv"

// Grade says how completely a result reflects its input, so callers can
// tell "read fine, found nothing" from "could not read". The same four
// grades, with the same names, exist in cssparse, xmlparse and sourcemap.
type Grade uint8

const (
	// GradeComplete: the whole file was read.
	GradeComplete Grade = iota
	// GradePartial: a limit (input size, item count) cut the read; what
	// was read before it is returned.
	GradePartial
	// GradeUnsupported: reserved for parity with the other parsers;
	// wellknown never produces it.
	GradeUnsupported
	// GradeFailed: reserved for parity; both formats here are read
	// line by line and never fail as a whole.
	GradeFailed
)

var gradeNames = [...]string{"complete", "partial", "unsupported", "failed"}

// String returns the stable name of g.
func (g Grade) String() string {
	if int(g) < len(gradeNames) {
		return gradeNames[g]
	}
	return "grade(" + strconv.Itoa(int(g)) + ")"
}

// Default limits, identical in every port (V2_DESIGN parser assurance
// bounds): 20,000 facts per document, 8 MiB per document. robots.txt has
// its own, smaller byte limit (RFC 9309 §2.5).
const (
	DefaultMaxItems = 20000
	DefaultMaxBytes = 8 << 20
)

// splitLines calls fn for each line of src (CR, LF or CR LF terminated)
// with its 1-based number and byte offset.
func splitLines(src []byte, fn func(line []byte, n, off int) bool) {
	n, off := 1, 0
	for off < len(src) {
		end := off
		for end < len(src) && src[end] != '\n' && src[end] != '\r' {
			end++
		}
		if !fn(src[off:end], n, off) {
			return
		}
		next := end + 1
		if end < len(src) && src[end] == '\r' && next < len(src) && src[next] == '\n' {
			next++
		}
		off, n = next, n+1
	}
}

func isWS(c byte) bool { return c == ' ' || c == '\t' }

func trimWS(b []byte) []byte {
	for len(b) > 0 && isWS(b[0]) {
		b = b[1:]
	}
	for len(b) > 0 && isWS(b[len(b)-1]) {
		b = b[:len(b)-1]
	}
	return b
}

func lowerASCII(b []byte) string {
	out := make([]byte, len(b))
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}
