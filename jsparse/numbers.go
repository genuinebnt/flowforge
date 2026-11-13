package jsparse

import (
	"math"
	"math/big"
	"strconv"
	"strings"
)

// numState is the state of the NumericLiteral machine (12.9.3).
type numState uint8

const (
	numStart           numState = iota // first character
	numLeadingZero                     // after an initial 0
	numDecimalInteger                  // DecimalIntegerLiteral digits
	numLegacyOctalLike                 // LegacyOctalIntegerLiteral / NonOctalDecimalIntegerLiteral (B.1.1)
	numFraction                        // after the decimal point
	numExponent                        // after e / E: optional sign
	numExponentDigits                  // DecimalDigits of ExponentPart
	numNonDecimal                      // HexIntegerLiteral / OctalIntegerLiteral / BinaryIntegerLiteral digits
	numBigIntSuffix                    // BigIntLiteralSuffix n
	numEnd                             // check the following code point
)

// scanNumericLiteral recognises NumericLiteral at start. The value is
// stored in Num (BigInt literals keep their digits in Value instead).
func (l *Lexer) scanNumericLiteral(start int) Token {
	src := l.src
	t := Token{Kind: TNumeric, Start: int32(start)}
	setErr := func(c Code, at int) {
		if t.Err == CodeNone {
			t.Err, t.ErrAt = c, int32(at)
		}
	}
	base := 10
	legacy := false     // legacy octal (base 8 without prefix)
	nonOctal := false   // NonOctalDecimalIntegerLiteral
	isFloat := false    // has fraction or exponent
	digitStart := start // first digit after any prefix
	st := numStart
	for st != numEnd {
		switch st {
		case numStart:
			c := src[l.pos]
			switch {
			case c == '0':
				l.pos++
				st = numLeadingZero
			case c == '.':
				l.pos++
				isFloat = true
				st = numFraction
			default:
				st = numDecimalInteger
			}

		case numLeadingZero:
			c := l.peekByte(0)
			switch {
			case c == 'x' || c == 'X':
				base = 16
				l.pos++
				st = numNonDecimal
			case c == 'o' || c == 'O':
				base = 8
				l.pos++
				st = numNonDecimal
			case c == 'b' || c == 'B':
				base = 2
				l.pos++
				st = numNonDecimal
			case isDecimalDigit(c):
				st = numLegacyOctalLike
			case c == '_':
				setErr(CodeNumericSeparator, l.pos)
				l.pos++
				st = numDecimalInteger
			case c == '.':
				l.pos++
				isFloat = true
				st = numFraction
			case c == 'e' || c == 'E':
				l.pos++
				isFloat = true
				st = numExponent
			case c == 'n':
				st = numBigIntSuffix
			default:
				st = numEnd
			}

		case numLegacyOctalLike:
			allOctal := true
			for l.pos < len(src) && (isDecimalDigit(src[l.pos]) || src[l.pos] == '_') {
				if src[l.pos] == '_' {
					setErr(CodeNumericSeparator, l.pos)
				} else if src[l.pos] >= '8' {
					allOctal = false
				}
				l.pos++
			}
			t.Flags |= FlagLegacyOctal
			if allOctal {
				legacy = true
				st = numEnd
				break
			}
			nonOctal = true
			switch l.peekByte(0) {
			case '.':
				l.pos++
				isFloat = true
				st = numFraction
			case 'e', 'E':
				l.pos++
				isFloat = true
				st = numExponent
			default:
				st = numEnd
			}

		case numDecimalInteger:
			l.scanDigits(10, &t)
			switch l.peekByte(0) {
			case '.':
				l.pos++
				isFloat = true
				st = numFraction
			case 'e', 'E':
				l.pos++
				isFloat = true
				st = numExponent
			case 'n':
				st = numBigIntSuffix
			default:
				st = numEnd
			}

		case numFraction:
			if c := l.peekByte(0); c == '_' {
				setErr(CodeNumericSeparator, l.pos)
			}
			l.scanDigits(10, &t)
			if c := l.peekByte(0); c == 'e' || c == 'E' {
				l.pos++
				st = numExponent
			} else {
				st = numEnd
			}

		case numExponent:
			if c := l.peekByte(0); c == '+' || c == '-' {
				l.pos++
			}
			st = numExponentDigits

		case numExponentDigits:
			if !isDecimalDigit(l.peekByte(0)) {
				setErr(CodeInvalidNumber, l.pos)
			}
			l.scanDigits(10, &t)
			st = numEnd

		case numNonDecimal:
			digitStart = l.pos
			if l.peekByte(0) == '_' {
				setErr(CodeNumericSeparator, l.pos)
			}
			n := l.scanDigits(base, &t)
			if n == 0 {
				setErr(CodeInvalidNumber, l.pos)
			}
			if l.peekByte(0) == 'n' {
				st = numBigIntSuffix
			} else {
				st = numEnd
			}

		case numBigIntSuffix:
			l.pos++
			t.Kind = TBigInt
			st = numEnd

		case numEnd:
		}
	}
	// The SourceCharacter immediately following a NumericLiteral must not
	// be an IdentifierStart or DecimalDigit.
	if l.pos < len(src) {
		c := src[l.pos]
		r, _ := decodeRune(src, l.pos)
		if isDecimalDigit(c) || c == '\\' || isIDStart(r) {
			setErr(CodeIdentifierAfterNumber, l.pos)
			// Consume the offending identifier part so the error token covers it.
			for l.pos < len(src) {
				r, n := decodeRune(src, l.pos)
				if !isIDContinue(r) && src[l.pos] != '\\' {
					break
				}
				l.pos += n
			}
		}
	}
	t.End = int32(l.pos)
	raw := src[start:l.pos]
	t.Value = raw
	if t.Err != CodeNone {
		return t
	}
	digits := src[digitStart:l.pos]
	if t.Kind == TBigInt {
		digits = digits[:len(digits)-1]
		if isFloat || legacy || nonOctal {
			setErr(CodeInvalidNumber, int(t.End)-1)
		}
		v := raw[:len(raw)-1]
		if t.Flags&FlagSeparator != 0 {
			v = strings.ReplaceAll(v, "_", "")
		}
		t.Value = v
		return t
	}
	if t.Flags&FlagSeparator != 0 {
		digits = strings.ReplaceAll(digits, "_", "")
	}
	switch {
	case legacy:
		t.Num = parseIntBase(digits, 8)
	case base != 10:
		t.Num = parseIntBase(digits, base)
	default:
		t.Num = parseDecimal(digits)
	}
	return t
}

// scanDigits consumes digits of base with NumericLiteralSeparator rules
// (a separator must sit between two digits). It returns the number of
// digits consumed.
func (l *Lexer) scanDigits(base int, t *Token) int {
	src := l.src
	n := 0
	prevSep := false
	for l.pos < len(src) {
		c := src[l.pos]
		if c == '_' {
			t.Flags |= FlagSeparator
			if n == 0 || prevSep {
				if t.Err == CodeNone {
					t.Err, t.ErrAt = CodeNumericSeparator, int32(l.pos)
				}
			}
			prevSep = true
			l.pos++
			continue
		}
		if !isDigitOfBase(c, base) {
			break
		}
		prevSep = false
		n++
		l.pos++
	}
	if prevSep && t.Err == CodeNone {
		t.Err, t.ErrAt = CodeNumericSeparator, int32(l.pos-1)
	}
	return n
}

func isDigitOfBase(c byte, base int) bool {
	switch base {
	case 2:
		return c == '0' || c == '1'
	case 8:
		return c >= '0' && c <= '7'
	case 16:
		return isHexDigit(c)
	}
	return isDecimalDigit(c)
}

// parseDecimal converts StrDecimalLiteral text to the nearest double
// (round half to even), as NumericValue requires. PORT: Rust's
// str::parse::<f64> is also correctly rounded.
func parseDecimal(s string) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		// Range errors still return the correctly rounded ±Inf or 0.
		if ne, ok := err.(*strconv.NumError); ok && ne.Err == strconv.ErrRange {
			return v
		}
		return math.NaN()
	}
	return v
}

// parseIntBase converts digits in base 2, 8 or 16 to the nearest double.
func parseIntBase(s string, base int) float64 {
	var u uint64
	bits := uint(4)
	switch base {
	case 2:
		bits = 1
	case 8:
		bits = 3
	}
	if uint(len(s))*bits <= 64 {
		for i := 0; i < len(s); i++ {
			u = u<<bits | uint64(hexVal(s[i]))
		}
		return float64(u)
	}
	var z big.Int
	if _, ok := z.SetString(s, base); !ok {
		return math.NaN()
	}
	f, _ := new(big.Float).SetInt(&z).Float64()
	return f
}
