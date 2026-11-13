package codec

import "slices"

// QPMaxLine is the longest encoded quoted-printable line, CRLF excluded
// (RFC 2045 §6.7 rule 5).
const QPMaxLine = 76

// EncodeQP returns the canonical quoted-printable encoding of src (RFC
// 2045 §6.7). It is the output strict decoding is checked against in the
// round-trip invariant:
//
//   - CR LF in src is a hard line break, written as CR LF (rule 4); a CR
//     or LF on its own is binary data and written =0D / =0A, so decoding
//     restores src exactly;
//   - bytes 33-60 and 62-126 are literal (rule 2); SP and HT are literal
//     unless they end a line or the data, then =20 / =09 (rule 3);
//     everything else is =XX with upper-case hex (rule 1);
//   - lines are filled greedily and broken with a soft line break "=" CR
//     LF so that no line exceeds 76 characters (rule 5); an escape is
//     never split.
func EncodeQP(src []byte) []byte { return AppendEncodeQP(nil, src) }

// AppendEncodeQP appends EncodeQP(src) to dst.
func AppendEncodeQP(dst, src []byte) []byte {
	dst = slices.Grow(dst, len(src)+len(src)/8)
	col := 0
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '\r' && i+1 < len(src) && src[i+1] == '\n' {
			dst = append(dst, '\r', '\n')
			col = 0
			i++
			continue
		}
		atLineEnd := i+1 == len(src) || src[i+1] == '\r' && i+2 < len(src) && src[i+2] == '\n'
		literal := c >= 33 && c <= 126 && c != '=' || (c == ' ' || c == '\t') && !atLineEnd
		w := 3
		if literal {
			w = 1
		}
		// A token may end a line at column 76 only when the line ends
		// there anyway (hard break or end of data); otherwise the soft
		// break's '=' needs the 76th column.
		if col+w > QPMaxLine || col+w == QPMaxLine && !atLineEnd {
			dst = append(dst, '=', '\r', '\n')
			col = 0
		}
		if literal {
			dst = append(dst, c)
		} else {
			dst = append(dst, '=', hexUpper[c>>4], hexUpper[c&15])
		}
		col += w
	}
	return dst
}

// qpState is the quoted-printable decoder state (RFC 2045 §6.7).
type qpState uint8

const (
	// qpText: literal octets (rule 2), white space (rule 3) and line
	// breaks (rule 4).
	qpText qpState = iota
	// qpEquals: '=' read; an escape (rule 1) or a soft line break (rule
	// 5) follows.
	qpEquals
	// qpEqualsHex: '=' and one hex digit read.
	qpEqualsHex
	// qpSoftSpace: '=' then SP/HT read (lenient: transport-added white
	// space before a soft line break, rule 3).
	qpSoftSpace
	// qpSoftCR: '=' (and white space) then CR read; LF ends the soft
	// line break.
	qpSoftCR
	// qpCR: (strict) a CR in text; only LF may follow (rule 4: CR and LF
	// occur only as a CRLF pair).
	qpCR
)

// DecodeQP decodes quoted-printable src.
//
// Strict enforces the MUSTs of RFC 2045 §6.7: line breaks are CR LF;
// literal octets are 33-60, 62-126, SP and HT; SP and HT never end a line
// or the data; '=' starts an upper-case =XX escape or a soft line break
// "=" CR LF; lines are at most 76 characters. Literal-able octets may
// still be escaped, so strict output re-encodes to EncodeQP only up to
// that freedom (CODEC-INV-03).
//
// Lenient is the robust decoder of rule 3 and of mime/quotedprintable:
// white space (and CR) at the end of a line or of the data is deleted; a
// bare LF is a line break (written as LF; CR LF stays CR LF); "=" followed
// by white space and a line break, or at the end of the data, is a soft
// line break; lower-case hex is accepted; '=' that starts neither an
// escape nor a soft break is a literal '='; every other octet passes
// through. It accepts everything mime/quotedprintable accepts, with the
// same output (CODEC-INV-05).
func DecodeQP(src []byte, mode Mode) ([]byte, error) {
	out, err := AppendDecodeQP(nil, src, mode)
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []byte{}
	}
	return out, nil
}

// AppendDecodeQP appends the decoding of src to dst; on error dst is
// returned unchanged.
func AppendDecodeQP(dst, src []byte, mode Mode) ([]byte, error) {
	if mode == Strict {
		return decodeQPStrict(dst, src)
	}
	out := slices.Grow(dst, len(src))
	state := qpText
	// ws is the start of the pending run of SP, HT and CR in text: it is
	// written only if a non-white octet follows on the same line.
	ws, eq := -1, 0
	var hi byte
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case qpText:
			switch c {
			case ' ', '\t', '\r':
				if ws < 0 {
					ws = i
				}
			case '\n':
				// Hard line break; the white space before it is deleted.
				if i > 0 && src[i-1] == '\r' {
					out = append(out, '\r', '\n')
				} else {
					out = append(out, '\n')
				}
				ws = -1
			default:
				if ws >= 0 {
					out = append(out, src[ws:i]...)
					ws = -1
				}
				if c == '=' {
					state, eq = qpEquals, i
				} else {
					out = append(out, c)
				}
			}
		case qpEquals:
			switch {
			case hexVal[c] != 0xFF:
				hi, state = hexVal[c], qpEqualsHex
			case c == '\n':
				state = qpText
			case c == '\r':
				state = qpSoftCR
			case c == ' ' || c == '\t':
				state = qpSoftSpace
			default:
				out = append(out, '=')
				state = qpText
				i--
			}
		case qpEqualsHex:
			if hexVal[c] != 0xFF {
				out = append(out, hi<<4|hexVal[c])
				state = qpText
				continue
			}
			out = append(out, '=', src[i-1])
			state = qpText
			i--
		case qpSoftSpace:
			switch c {
			case ' ', '\t':
			case '\n':
				state = qpText
			case '\r':
				state = qpSoftCR
			default:
				// Not a soft line break: the '=' is literal and the bytes
				// after it are read again as text.
				out = append(out, '=')
				state, i = qpText, eq
			}
		case qpSoftCR:
			if c == '\n' {
				state = qpText
				continue
			}
			out = append(out, '=')
			state, i = qpText, eq
		}
	}
	if state == qpEqualsHex {
		out = append(out, '=', src[len(src)-1])
	}
	// A pending white-space run, or a '=' (with white space) at the end
	// of the data, is deleted: the data ends a line.
	return out, nil
}

func decodeQPStrict(dst, src []byte) ([]byte, error) {
	out := slices.Grow(dst, len(src))
	state := qpText
	lineStart, eq := 0, 0
	var hi byte
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch state {
		case qpText:
			switch {
			case c == '\r':
				state = qpCR
				continue
			case c == '=':
				state, eq = qpEquals, i
			case c >= 33 && c <= 126 || c == ' ' || c == '\t':
				if (c == ' ' || c == '\t') && (i+1 == len(src) || src[i+1] == '\r') {
					// Rule 3: white space must not end an encoded line.
					return dst, errAt(CodeNonCanonical, i)
				}
				out = append(out, c)
			default:
				return dst, errAt(CodeInvalidByte, i)
			}
		case qpCR:
			if c != '\n' {
				return dst, errAt(CodeInvalidByte, i-1)
			}
			out = append(out, '\r', '\n')
			state, lineStart = qpText, i+1
			continue
		case qpEquals:
			switch {
			case c == '\r':
				state = qpSoftCR
				continue
			case isUpperHexDigit(c):
				hi, state = hexVal[c], qpEqualsHex
			case hexVal[c] != 0xFF:
				return dst, errAt(CodeNonCanonical, eq)
			default:
				return dst, errAt(CodeInvalidEscape, eq)
			}
		case qpEqualsHex:
			switch {
			case isUpperHexDigit(c):
				out = append(out, hi<<4|hexVal[c])
				state = qpText
			case hexVal[c] != 0xFF:
				return dst, errAt(CodeNonCanonical, eq)
			default:
				return dst, errAt(CodeInvalidEscape, eq)
			}
		case qpSoftCR:
			if c != '\n' {
				return dst, errAt(CodeInvalidEscape, eq)
			}
			state, lineStart = qpText, i+1
			continue
		}
		if i-lineStart+1 > QPMaxLine {
			return dst, errAt(CodeLineTooLong, i)
		}
	}
	switch state {
	case qpCR:
		return dst, errAt(CodeInvalidByte, len(src)-1)
	case qpEquals, qpEqualsHex, qpSoftCR:
		return dst, errAt(CodeTruncated, len(src))
	}
	return out, nil
}
