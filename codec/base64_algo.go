package codec

import (
	"encoding/base64"
	"encoding/binary"
	"slices"
)

// Base64Algo is the base64 decoder candidate (docs/ALGORITHMS.md,
// "Base64 decoding"). All candidates implement exactly the same
// semantics, including the error code and offset (CODEC-INV-06): the fast
// ones only take whole quanta of four alphabet characters and hand every
// other quantum to the state machine.
type Base64Algo uint8

const (
	// Base64AlgoDefault is the zero value and means the chosen default
	// (Base64AlgoSWAR).
	Base64AlgoDefault Base64Algo = iota
	// Base64AlgoStateMachine decodes byte by byte with the RFC 4648 §4
	// state machine (b64Dec). It is the reference the others must match.
	Base64AlgoStateMachine
	// Base64AlgoSWAR is table-driven and word-at-a-time: four lookup
	// tables give each character's sextet already shifted into place
	// with an "invalid" bit above bit 23, so eight characters decode with
	// eight lookups, seven ORs, one test and one 8-byte store.
	Base64AlgoSWAR
	// Base64AlgoStdlib calls encoding/base64 on inputs it can take as they
	// are (no white space; one alphabet; full or no padding) and the
	// state machine otherwise and on every error.
	Base64AlgoStdlib
)

var base64AlgoNames = [...]string{
	Base64AlgoStateMachine: "statemachine",
	Base64AlgoSWAR:         "swar",
	Base64AlgoStdlib:       "stdlib",
}

// Base64Algos returns the selectable candidates in a stable order.
func Base64Algos() []Base64Algo {
	return []Base64Algo{Base64AlgoStateMachine, Base64AlgoSWAR, Base64AlgoStdlib}
}

// String returns the configuration name ("statemachine", "swar",
// "stdlib"); the default prints as the candidate it resolves to.
func (a Base64Algo) String() string {
	r := a.resolve()
	if int(r) < len(base64AlgoNames) && base64AlgoNames[r] != "" {
		return base64AlgoNames[r]
	}
	return "unknown"
}

// ParseBase64Algo maps a configuration name to its candidate. The empty
// string is the default.
func ParseBase64Algo(name string) (Base64Algo, error) {
	if name == "" {
		return Base64AlgoDefault, nil
	}
	for i, n := range base64AlgoNames {
		if n != "" && n == name {
			return Base64Algo(i), nil
		}
	}
	return 0, ErrUnknownAlgorithm
}

func (a Base64Algo) resolve() Base64Algo {
	switch a {
	case Base64AlgoStateMachine, Base64AlgoSWAR, Base64AlgoStdlib:
		return a
	}
	return Base64AlgoSWAR
}

// b64Bad is the "not a plain alphabet character" bit of the SWAR tables.
const b64Bad = 1 << 24

// b64Fast[t][k][c] is the sextet of character c at position k of a
// quantum, shifted to bit 18-6k, or b64Bad for anything else (white
// space and '=' too, so the state machine handles them).
var b64Fast = func() (f [3][4][256]uint32) {
	for t := range f {
		for k := 0; k < 4; k++ {
			for c := 0; c < 256; c++ {
				v := b64Tables[t][c]
				if v < 64 {
					f[t][k][c] = uint32(v) << (18 - 6*k)
				} else {
					f[t][k][c] = b64Bad
				}
			}
		}
	}
	return f
}()

func decodeBase64SWAR(dst, src []byte, o Base64Options) ([]byte, error) {
	d := newB64Dec(dst, src, o)
	// Eight bytes of headroom let the 8-byte store write 6 bytes at a
	// time without a bounds branch per byte; the state machine's appends
	// then never reallocate.
	d.out = slices.Grow(d.out, DecodedLenMaxBase64(len(src))+8)
	t := &b64Fast[o.table()]
	t0, t1, t2, t3 := &t[0], &t[1], &t[2], &t[3]
	out := d.out[:cap(d.out)]
	n, i := len(d.out), 0
	for i < len(src) {
		for len(src)-i >= 8 && len(out)-n >= 8 {
			x := t0[src[i]] | t1[src[i+1]] | t2[src[i+2]] | t3[src[i+3]]
			y := t0[src[i+4]] | t1[src[i+5]] | t2[src[i+6]] | t3[src[i+7]]
			if (x|y)&b64Bad != 0 {
				break
			}
			binary.BigEndian.PutUint64(out[n:], uint64(x)<<40|uint64(y)<<16)
			n += 6
			i += 8
		}
		if len(src)-i >= 4 {
			if x := t0[src[i]] | t1[src[i+1]] | t2[src[i+2]] | t3[src[i+3]]; x&b64Bad == 0 {
				out[n], out[n+1], out[n+2] = byte(x>>16), byte(x>>8), byte(x)
				n += 3
				i += 4
				continue
			}
		}
		if i >= len(src) {
			break
		}
		// Not a whole quantum of alphabet characters: the state machine
		// takes one quantum (white space, padding, the end of the data,
		// or an error). It returns early only at a quantum boundary, so
		// any other state means it reached the end or failed.
		d.out = out[:n]
		i = d.run(src, i, true)
		n = len(d.out)
		if d.err != nil || d.state != b64Quantum0 {
			break
		}
	}
	d.out = out[:n]
	return d.finish(dst, len(src))
}

func stdEncoding(o Base64Options, padded bool) *base64.Encoding {
	switch {
	case o.Alphabet == Base64URL && padded:
		return base64.URLEncoding
	case o.Alphabet == Base64URL:
		return base64.RawURLEncoding
	case padded:
		return base64.StdEncoding
	}
	return base64.RawStdEncoding
}

func decodeBase64Stdlib(dst, src []byte, o Base64Options) ([]byte, error) {
	var enc *base64.Encoding
	if o.Mode == Strict {
		// PORT: encoding/base64 skips CR and LF even in Strict mode; ours
		// does not, so those inputs go to the state machine.
		for _, c := range src {
			if c == '\r' || c == '\n' {
				return slowBase64(dst, src, o)
			}
		}
		enc = stdEncoding(o, !o.NoPadding).Strict()
	} else {
		padded := false
		for _, c := range src {
			if isSpace(c) {
				return slowBase64(dst, src, o)
			}
			padded = padded || c == '='
		}
		enc = stdEncoding(o, padded)
	}
	start := len(dst)
	out := append(dst, make([]byte, enc.DecodedLen(len(src)))...)
	n, err := enc.Decode(out[start:], src)
	if err != nil {
		return slowBase64(dst[:start], src, o)
	}
	return out[:start+n], nil
}

func slowBase64(dst, src []byte, o Base64Options) ([]byte, error) {
	d := newB64Dec(dst, src, o)
	d.run(src, 0, false)
	return d.finish(dst, len(src))
}
