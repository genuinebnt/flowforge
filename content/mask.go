package content

import (
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"slices"
	"strings"
)

// MaskClass names one kind of volatile value a normalizer masks. Masks
// change only the normalized text, never the extracted features
// (CONTENT-INV-03).
type MaskClass string

// Mask classes of the built-in rule tables.
const (
	MaskNonce     MaskClass = "nonce"      // nonce attributes, CSP 'nonce-…' sources
	MaskCSRF      MaskClass = "csrf"       // CSRF token inputs and meta tags
	MaskTimestamp MaskClass = "timestamp"  // epoch seconds/milliseconds, ISO-8601 date-times
	MaskCacheBust MaskClass = "cache_bust" // ?v=…, ?t=… query values
	MaskBuildHash MaskClass = "build_hash" // bundler hashes in asset names, SRI digests, build ids
	MaskRandomID  MaskClass = "random_id"  // UUIDs and request ids in volatile positions
	MaskHidden    MaskClass = "hidden_value"
	MaskHandler   MaskClass = "handler" // event-handler bodies moved to the #handlers document
	MaskLearned   MaskClass = "learned" // a frozen learned volatility rule
)

// maskPlaceholder is the text that replaces a masked value. Placeholders
// are upper-case ASCII with underscores: no rule matches them again
// (idempotence), and every tokenizer keeps them as one identifier or
// string body.
func maskPlaceholder(c MaskClass) string {
	return "__FF_MASK_" + strings.ToUpper(string(c)) + "__"
}

// isPlaceholder reports text that is already a mask placeholder.
func isPlaceholder(s string) bool {
	return strings.HasPrefix(s, "__FF_MASK_") && strings.HasSuffix(s, "__") || s == hashPlaceholder
}

// MaskCount is how many values of one class a normalization masked.
type MaskCount struct {
	Class MaskClass `json:"class"`
	Count int       `json:"count"`
}

// masker counts masked values and digests the originals, so two
// normalizations whose texts are equal can still tell "only masked
// values changed" (masked_only) from "nothing changed".
type masker struct {
	counts map[MaskClass]int
	digest hash.Hash
}

func newMasker() *masker { return &masker{counts: map[MaskClass]int{}, digest: sha256.New()} }

// hit records one masked value. The digest input is class, NUL, value,
// NUL, in text order.
func (m *masker) hit(c MaskClass, original string) {
	m.counts[c]++
	m.digest.Write([]byte(c))
	m.digest.Write([]byte{0})
	m.digest.Write([]byte(original))
	m.digest.Write([]byte{0})
}

// result returns the counts sorted by class and the hex digest.
func (m *masker) result() ([]MaskCount, string) {
	var out []MaskCount
	for c, n := range m.counts {
		out = append(out, MaskCount{Class: c, Count: n})
	}
	// PORT: map iteration order is random; sort so the output is stable.
	slices.SortFunc(out, func(a, b MaskCount) int { return strings.Compare(string(a.Class), string(b.Class)) })
	return out, hex.EncodeToString(m.digest.Sum(nil))
}

// Epoch bounds for timestamp detection: 2000-01-01 to 2100-01-01 UTC.
const (
	minEpochSeconds = 946_684_800
	maxEpochSeconds = 4_102_444_800
)

// isTimestampDigits reports 10 digits of epoch seconds or 13 digits of
// epoch milliseconds between 2000-01-01 and 2100-01-01 UTC, both ends
// included. A 13-digit value is tested on its first 10 digits (whole
// seconds). The digit counts exclude the 9-digit seconds and 12-digit
// milliseconds of 2000-01-01 to 2001-09-09.
func isTimestampDigits(s string) bool {
	if (len(s) != 10 && len(s) != 13) || !isAllDigits(s) {
		return false
	}
	var v uint64
	for i := 0; i < 10; i++ {
		v = v*10 + uint64(s[i]-'0')
	}
	return v >= minEpochSeconds && v <= maxEpochSeconds
}

// isISODateTime reports an ISO-8601 / RFC 3339 date-time:
// YYYY-MM-DD, 'T' or ' ', HH:MM, optional :SS, optional fraction,
// optional 'Z' or ±HH:MM. A bare date is not volatile (it is content).
func isISODateTime(s string) bool {
	if len(s) < 16 || len(s) > 40 {
		return false
	}
	d := func(i int) bool { return i < len(s) && s[i] >= '0' && s[i] <= '9' }
	for _, i := range []int{0, 1, 2, 3, 5, 6, 8, 9, 11, 12, 14, 15} {
		if !d(i) {
			return false
		}
	}
	if s[4] != '-' || s[7] != '-' || (s[10] != 'T' && s[10] != ' ') || s[13] != ':' {
		return false
	}
	i := 16
	if i < len(s) && s[i] == ':' {
		if !d(i+1) || !d(i+2) {
			return false
		}
		i += 3
		if i < len(s) && (s[i] == '.' || s[i] == ',') {
			i++
			j := i
			for d(i) {
				i++
			}
			if i == j {
				return false
			}
		}
	}
	switch {
	case i == len(s):
		return true
	case s[i] == 'Z' && i+1 == len(s):
		return true
	case (s[i] == '+' || s[i] == '-') && i+6 == len(s):
		return d(i+1) && d(i+2) && s[i+3] == ':' && d(i+4) && d(i+5)
	}
	return false
}

// findISODateTimes returns the [start, end) spans of ISO date-times in
// running text (each starting at a digit not preceded by a digit).
func findISODateTimes(s string) [][2]int {
	var out [][2]int
	for i := 0; i+16 <= len(s); i++ {
		if s[i] < '0' || s[i] > '9' || i > 0 && (s[i-1] >= '0' && s[i-1] <= '9') || s[i+4] != '-' {
			continue
		}
		// Longest prefix that is a date-time: try ends from long to short.
		for end := min(len(s), i+40); end >= i+16; end-- {
			if isISODateTime(s[i:end]) && (end == len(s) || !isDateTimeByte(s[end])) {
				out = append(out, [2]int{i, end})
				i = end - 1
				break
			}
		}
	}
	return out
}

func isDateTimeByte(c byte) bool {
	return c >= '0' && c <= '9' || c == ':' || c == '.' || c == 'Z' || c == '+' || c == '-'
}

// isUUID reports the 8-4-4-4-12 hex form.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// isBuildID reports a long digest-like string: 16+ hex digits with at
// least one digit and one letter (git SHAs, content hashes, build ids).
func isBuildID(s string) bool {
	if len(s) < 16 || len(s) > 128 || !isHexString(s) {
		return false
	}
	return strings.ContainsAny(s, "0123456789") && strings.ContainsAny(s, "abcdefABCDEF")
}

// maskText masks ISO date-times in running text.
func maskText(s string, m *masker) string {
	spans := findISODateTimes(s)
	if len(spans) == 0 {
		return s
	}
	var b strings.Builder
	prev := 0
	for _, sp := range spans {
		b.WriteString(s[prev:sp[0]])
		m.hit(MaskTimestamp, s[sp[0]:sp[1]])
		b.WriteString(maskPlaceholder(MaskTimestamp))
		prev = sp[1]
	}
	b.WriteString(s[prev:])
	return b.String()
}

// volatileScalar classifies a standalone string value (an attribute
// value, a JSON string, a JS literal) by shape: timestamps, UUIDs and
// build ids. Path-like values are never volatile: they are level-3
// feature candidates (endpoints, links).
func volatileScalar(s string) (MaskClass, bool) {
	if strings.ContainsRune(s, '/') || isPlaceholder(s) {
		return "", false
	}
	switch {
	case isTimestampDigits(s) || isISODateTime(s):
		return MaskTimestamp, true
	case isUUID(s):
		return MaskRandomID, true
	case isBuildID(s):
		return MaskBuildHash, true
	}
	return "", false
}
