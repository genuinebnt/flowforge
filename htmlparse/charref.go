package htmlparse

// Character reference states: HTML §13.2.5 "Character reference state"
// through "Numeric character reference end state", and the named
// character reference lookup (§13.5).

//go:generate sh -c "cd .. && go run ./htmlparse/internal/gen -src \"$(go list -m -f '{{.Dir}}' golang.org/x/net)/html/entity.go\" -go htmlparse/entities_table.go -json conformance/htmlparse/entities.json"

// CharRefStrategy selects the named character reference lookup
// algorithm. Both find the longest name in the WHATWG table that prefixes
// the input (HTML §13.2.5 "named character reference state") and give
// identical results (HTML-INV-11); they differ only in speed and table
// size. See README.md "Performance comparison" for why the default won.
type CharRefStrategy uint8

const (
	// CharRefTrie (the default) walks a generated compact prefix trie
	// (9854 nodes, 9853 edges, about 100 KB of static tables) with a
	// binary search over each node's sorted edges.
	CharRefTrie CharRefStrategy = iota
	// CharRefSortedSearch narrows a range of the sorted name table with
	// two binary searches per input byte; it needs no table beyond the
	// names themselves but is about 1.6x slower on entity-dense text.
	CharRefSortedSearch
)

// entityTrieNode is a node of the generated prefix trie: its outgoing
// edges are entityTrieEdges[first : first+count]; entity is the index in
// entityNames of the name ending at this node, or -1.
type entityTrieNode struct {
	first  uint16
	count  uint8
	entity int16
}

// entityTrieEdge is one labelled trie edge.
type entityTrieEdge struct {
	c     byte
	child uint16
}

// lookupEntityTrie is lookupEntity over the prefix trie.
func lookupEntityTrie(b []byte) (int, int) {
	node := 0
	bestLen, best := 0, -1
	for k := 0; k < len(b) && k < entityMaxLen; k++ {
		n := entityTrieNodes[node]
		lo, hi := int(n.first), int(n.first)+int(n.count)
		c := b[k]
		for lo < hi {
			m := int(uint(lo+hi) >> 1)
			if entityTrieEdges[m].c < c {
				lo = m + 1
			} else {
				hi = m
			}
		}
		if lo >= int(n.first)+int(n.count) || entityTrieEdges[lo].c != c {
			break
		}
		node = int(entityTrieEdges[lo].child)
		if e := entityTrieNodes[node].entity; e >= 0 {
			bestLen, best = k+1, int(e)
		}
	}
	return bestLen, best
}

// lookupEntity finds the longest named character reference that is a
// prefix of b and returns its length and index in entityNames, or (0, -1).
//
// The search narrows a range [lo, hi) of entityNames that share the
// prefix b[:k] one byte at a time with two binary searches per byte.
// Because the names are sorted bytewise, a name equal to the prefix
// itself sorts first in its range, so each step checks one candidate.
// This is O(entityMaxLen · log entityCount) and needs no trie.
func lookupEntity(b []byte) (int, int) {
	lo, hi := 0, entityCount
	bestLen, best := 0, -1
	for k := 0; k < len(b) && k < entityMaxLen; k++ {
		c := b[k]
		if !isASCIIAlnumB(c) && c != ';' {
			break
		}
		// Every name in [lo, hi) has prefix b[:k]. Find the sub-range whose
		// byte k equals c; names of length k (which have no byte k) sort
		// first and are excluded.
		l, h := lo, hi
		for l < h {
			m := int(uint(l+h) >> 1)
			if keyAt(m, k) < int(c) {
				l = m + 1
			} else {
				h = m
			}
		}
		newLo := l
		l, h = newLo, hi
		for l < h {
			m := int(uint(l+h) >> 1)
			if keyAt(m, k) <= int(c) {
				l = m + 1
			} else {
				h = m
			}
		}
		lo, hi = newLo, l
		if lo >= hi {
			break
		}
		if len(entityNames[lo]) == k+1 {
			bestLen, best = k+1, lo
		}
		if c == ';' {
			break // no name continues past ';'
		}
	}
	return bestLen, best
}

// keyAt is byte k of entityNames[i], or -1 if the name is shorter.
func keyAt(i, k int) int {
	n := entityNames[i]
	if k < len(n) {
		return int(n[k])
	}
	return -1
}

// characterReferenceState is the character reference state.
func (t *Tokenizer) characterReferenceState() {
	t.temp = append(t.temp[:0], '&')
	c := t.next()
	switch {
	case isASCIIAlnum(c):
		t.reconsume()
		t.state = stateNamedCharacterReference
	case c == '#':
		t.temp = append(t.temp, '#')
		t.state = stateNumericCharacterReference
	default:
		t.reconsume()
		t.flushCharRef()
		t.state = t.returnState
	}
}

// flushCharRef is "flush code points consumed as a character reference".
func (t *Tokenizer) flushCharRef() {
	if t.inAttributeValue() {
		t.attrValue = append(t.attrValue, t.temp...)
		return
	}
	t.emitBytes(t.temp)
}

// namedCharacterReferenceState is the named character reference state.
func (t *Tokenizer) namedCharacterReferenceState() {
	var n, idx int
	if t.charRef == CharRefSortedSearch {
		n, idx = lookupEntity(t.in[t.pos:])
	} else {
		n, idx = lookupEntityTrie(t.in[t.pos:])
	}
	if n == 0 {
		t.flushCharRef()
		t.state = stateAmbiguousAmpersand
		return
	}
	matched := t.in[t.pos : t.pos+n]
	t.temp = append(t.temp, matched...)
	t.skipASCII(n)
	last := matched[n-1]
	if t.inAttributeValue() && last != ';' && t.pos < len(t.in) && (t.in[t.pos] == '=' || isASCIIAlnumB(t.in[t.pos])) {
		t.flushCharRef()
		t.state = t.returnState
		return
	}
	if last != ';' {
		t.errAt(ErrMissingSemicolonAfterCharacterReference, t.pos)
	}
	v := entityValues[idx]
	t.temp = appendRune(t.temp[:0], v[0])
	if v[1] != 0 {
		t.temp = appendRune(t.temp, v[1])
	}
	t.flushCharRef()
	t.state = t.returnState
}

// ambiguousAmpersandState is the ambiguous ampersand state.
func (t *Tokenizer) ambiguousAmpersandState() {
	for {
		c := t.next()
		switch {
		case isASCIIAlnum(c):
			if t.inAttributeValue() {
				t.attrValue = append(t.attrValue, byte(c))
			} else {
				t.emitRune(c)
			}
			continue
		case c == ';':
			t.err(ErrUnknownNamedCharacterReference)
		}
		t.reconsume()
		t.state = t.returnState
		return
	}
}

// numericCharacterReferenceState is the numeric character reference
// state.
func (t *Tokenizer) numericCharacterReferenceState() {
	t.refCode = 0
	c := t.next()
	switch {
	case c == 'x' || c == 'X':
		t.temp = append(t.temp, byte(c))
		t.state = stateHexadecimalCharacterReferenceStart
	case isASCIIDigit(c):
		t.reconsume()
		t.state = stateDecimalCharacterReference
	default:
		t.err(ErrAbsenceOfDigitsInNumericCharacterReference)
		t.reconsume()
		t.flushCharRef()
		t.state = t.returnState
	}
}

// hexadecimalCharacterReferenceStartState is the hexadecimal character
// reference start state.
func (t *Tokenizer) hexadecimalCharacterReferenceStartState() {
	c := t.next()
	t.reconsume()
	if isASCIIHex(c) {
		t.state = stateHexadecimalCharacterReference
		return
	}
	t.err(ErrAbsenceOfDigitsInNumericCharacterReference)
	t.flushCharRef()
	t.state = t.returnState
}

// addDigit accumulates a digit into the character reference code,
// saturating just above the Unicode range so overflow cannot wrap.
func (t *Tokenizer) addDigit(base, d int32) {
	if t.refCode <= 0x10FFFF {
		t.refCode = t.refCode*base + d
	}
}

// hexadecimalCharacterReferenceState is the hexadecimal character
// reference state.
func (t *Tokenizer) hexadecimalCharacterReferenceState() {
	for {
		c := t.next()
		switch {
		case isASCIIDigit(c):
			t.addDigit(16, c-'0')
		case c >= 'A' && c <= 'F':
			t.addDigit(16, c-0x37)
		case c >= 'a' && c <= 'f':
			t.addDigit(16, c-0x57)
		case c == ';':
			t.state = stateNumericCharacterReferenceEnd
			return
		default:
			t.err(ErrMissingSemicolonAfterCharacterReference)
			t.reconsume()
			t.state = stateNumericCharacterReferenceEnd
			return
		}
	}
}

// decimalCharacterReferenceState is the decimal character reference
// state.
func (t *Tokenizer) decimalCharacterReferenceState() {
	for {
		c := t.next()
		switch {
		case isASCIIDigit(c):
			t.addDigit(10, c-'0')
		case c == ';':
			t.state = stateNumericCharacterReferenceEnd
			return
		default:
			t.err(ErrMissingSemicolonAfterCharacterReference)
			t.reconsume()
			t.state = stateNumericCharacterReferenceEnd
			return
		}
	}
}

// c1Replacements is the numeric character reference end state's table
// for 0x80..0x9F (0 means "no replacement").
var c1Replacements = [32]rune{
	0x20AC, 0, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021,
	0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0, 0x017D, 0,
	0, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014,
	0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0, 0x017E, 0x0178,
}

// numericCharacterReferenceEndState is the numeric character reference
// end state. It consumes no input. Errors are reported at the offset
// just after the reference.
func (t *Tokenizer) numericCharacterReferenceEndState() {
	code := t.refCode
	at := t.pos
	switch {
	case code == 0:
		t.errAt(ErrNullCharacterReference, at)
		code = 0xFFFD
	case code > 0x10FFFF:
		t.errAt(ErrCharacterReferenceOutsideUnicodeRange, at)
		code = 0xFFFD
	case isSurrogate(code):
		t.errAt(ErrSurrogateCharacterReference, at)
		code = 0xFFFD
	case isNoncharacter(code):
		t.errAt(ErrNoncharacterCharacterReference, at)
	case code == 0x0D || (isControlError(code) && code != 0):
		t.errAt(ErrControlCharacterReference, at)
		if code >= 0x80 && code <= 0x9F {
			if r := c1Replacements[code-0x80]; r != 0 {
				code = r
			}
		}
	}
	t.temp = appendRune(t.temp[:0], code)
	t.flushCharRef()
	t.state = t.returnState
}
