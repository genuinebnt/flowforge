package htmlparse

import (
	"fmt"
	"strconv"
	"unicode/utf8"
)

// decodeJSONStringWTF8 decodes a raw JSON string literal (including the
// quotes) into bytes. Unlike encoding/json it keeps lone surrogate
// escapes (\uD800) as their generalized-UTF-8 (WTF-8) encoding, so the
// html5lib tests that feed surrogates into the input stream can be run:
// the tokenizer reports surrogate-in-input-stream for them.
func decodeJSONStringWTF8(raw []byte) ([]byte, error) {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return nil, fmt.Errorf("not a JSON string: %q", raw)
	}
	return unescapeU(raw[1:len(raw)-1], true)
}

// unescapeU decodes backslash escapes. With full it accepts every JSON
// escape; without it only \uXXXX is decoded (the html5lib
// "doubleEscaped" second pass). Lone surrogates are written as WTF-8.
func unescapeU(s []byte, full bool) ([]byte, error) {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			out = append(out, c)
			continue
		}
		e := s[i+1]
		if e != 'u' {
			if !full {
				out = append(out, c)
				continue
			}
			i++
			switch e {
			case '"', '\\', '/':
				out = append(out, e)
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			default:
				return nil, fmt.Errorf("bad escape \\%c", e)
			}
			continue
		}
		if i+6 > len(s) {
			return nil, fmt.Errorf("short \\u escape")
		}
		v, err := strconv.ParseUint(string(s[i+2:i+6]), 16, 32)
		if err != nil {
			return nil, err
		}
		i += 5
		r := rune(v)
		if r >= 0xD800 && r <= 0xDBFF && i+7 <= len(s) && s[i+1] == '\\' && s[i+2] == 'u' {
			if lo, err := strconv.ParseUint(string(s[i+3:i+7]), 16, 32); err == nil && lo >= 0xDC00 && lo <= 0xDFFF {
				r = 0x10000 + (r-0xD800)<<10 + (rune(lo) - 0xDC00)
				i += 6
				out = utf8.AppendRune(out, r)
				continue
			}
		}
		if r >= 0xD800 && r <= 0xDFFF {
			out = append(out, 0xED, 0xA0|byte((r>>6)&0x1F), 0x80|byte(r&0x3F))
			continue
		}
		out = utf8.AppendRune(out, r)
	}
	return out, nil
}

// replaceSurrogates maps WTF-8 surrogate sequences to U+FFFD, the
// tokenizer's output policy for lone surrogates.
func replaceSurrogates(b []byte) string {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if i+2 < len(b) && b[i] == 0xED && b[i+1] >= 0xA0 && b[i+1] <= 0xBF {
			out = append(out, "�"...)
			i += 3
			continue
		}
		out = append(out, b[i])
		i++
	}
	return string(out)
}
