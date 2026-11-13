package diff

import (
	"unicode"
	"unicode/utf8"
)

// NoToken is the id no interned string receives. CleanupOptions.Score
// receives it for the boundary at either end of a sequence.
const NoToken uint32 = 1<<32 - 1

// Interner maps strings (lines, words, labels) to dense uint32 ids in
// first-seen order, so every algorithm in this package works on []uint32
// and compares tokens with a single integer comparison. Two strings get the
// same id exactly when they are byte-equal. Ids are deterministic: they
// depend only on the order of Intern calls. It is not safe for concurrent
// use.
//
// PORT: keys are raw byte strings (no Unicode normalization); the map is
// only looked up, ids come from the append order of strs.
type Interner struct {
	ids  map[string]uint32
	strs []string
	full bool
}

// NewInterner returns an empty interner.
func NewInterner() *Interner { return &Interner{ids: make(map[string]uint32)} }

// Intern returns s's id, assigning the next id if s is new. Once 2^32-1
// distinct strings are interned the table is full: further new strings get
// NoToken and Overflowed reports true (unreachable for in-memory inputs).
func (in *Interner) Intern(s string) uint32 {
	if id, ok := in.ids[s]; ok {
		return id
	}
	if uint64(len(in.strs)) >= uint64(NoToken) {
		in.full = true
		return NoToken
	}
	id := uint32(len(in.strs))
	in.ids[s] = id
	in.strs = append(in.strs, s)
	return id
}

// InternAll interns every string of ss in order.
func (in *Interner) InternAll(ss []string) []uint32 {
	out := make([]uint32, len(ss))
	for i, s := range ss {
		out[i] = in.Intern(s)
	}
	return out
}

// Value returns the string with id, or "" and false for an unknown id.
func (in *Interner) Value(id uint32) (string, bool) {
	if uint64(id) >= uint64(len(in.strs)) {
		return "", false
	}
	return in.strs[id], true
}

// Values maps ids back to strings; unknown ids map to "".
func (in *Interner) Values(ids []uint32) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i], _ = in.Value(id)
	}
	return out
}

// Len returns the number of distinct strings interned.
func (in *Interner) Len() int { return len(in.strs) }

// Overflowed reports whether a string was refused because the table was
// full.
func (in *Interner) Overflowed() bool { return in.full }

// SplitLines splits text into lines that keep their "\n" terminator, so a
// last line without a newline is a different token from the same line with
// one (as in git). "\r\n" stays part of the line. Empty text has no lines.
func SplitLines(text []byte) []string {
	var out []string
	start := 0
	for i, c := range text {
		if c == '\n' {
			out = append(out, string(text[start:i+1]))
			start = i + 1
		}
	}
	if start < len(text) {
		out = append(out, string(text[start:]))
	}
	return out
}

// SplitWords splits s into word tokens that tile it exactly: maximal runs
// of letters, digits and '_', maximal runs of white space, and every other
// rune on its own. Invalid UTF-8 bytes are single tokens.
//
// PORT: letters, digits and white space are Go's unicode.IsLetter,
// unicode.IsDigit and unicode.IsSpace (Unicode 15.0 tables); Rust's
// char::is_alphabetic/is_numeric/is_whitespace differ in places, so port
// the same categories (L*, Nd, and Go's White_Space set).
func SplitWords(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		cls := wordClass(r, size)
		j := i + size
		if cls != classOther {
			for j < len(s) {
				r2, s2 := utf8.DecodeRuneInString(s[j:])
				if wordClass(r2, s2) != cls {
					break
				}
				j += s2
			}
		}
		out = append(out, s[i:j])
		i = j
	}
	return out
}

// SplitWords token classes.
const (
	classWord = iota
	classSpace
	classOther
)

// wordClass classifies one decoded rune; an invalid byte is classOther, so
// it is a token of its own.
func wordClass(r rune, size int) int {
	switch {
	case r == utf8.RuneError && size == 1:
		return classOther
	case r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r):
		return classWord
	case unicode.IsSpace(r):
		return classSpace
	default:
		return classOther
	}
}
