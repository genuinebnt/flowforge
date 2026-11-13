package codec

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/quotedprintable"
	"net/url"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
	"github.com/klauspost/compress/zstd"
)

// allTextCodecs is textCodecs plus the base64 variants under every
// decoder candidate.
func allTextCodecs() []textCodec {
	cs := textCodecs()
	for _, a := range Base64Algos() {
		for _, alpha := range []Base64Alphabet{Base64Std, Base64URL} {
			for _, np := range []bool{false, true} {
				cs = append(cs, base64Codec("base64/"+a.String(), alpha, np, a))
			}
		}
	}
	return cs
}

func compress(t testing.TB, k Kind, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	var w io.WriteCloser
	switch k {
	case KindGzip:
		w = gzip.NewWriter(&buf)
	case KindZlib:
		w = zlib.NewWriter(&buf)
	case KindDeflate:
		w, _ = flate.NewWriter(&buf, flate.BestCompression)
	case KindZstd:
		enc, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1))
		if err != nil {
			t.Fatal(err)
		}
		defer enc.Close()
		return enc.EncodeAll(b, nil)
	default:
		t.Fatalf("compress %s", k)
	}
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var compressionKinds = []Kind{KindGzip, KindZlib, KindDeflate, KindZstd}

// TestCodecInv01_RoundTrip: for every codec, variant and mode,
// decode(encode(x)) == x (JS over UTF-8/WTF-8; forms over UTF-8 pairs;
// every compression format through its reference encoder).
func TestCodecInv01_RoundTrip(t *testing.T) {
	r := testkit.Rng(t)
	for _, c := range allTextCodecs() {
		for i := 0; i < testkit.Cases(); i++ {
			x := randInput(r, c)
			if c.domain != nil && !c.domain(x) {
				continue
			}
			e := c.encode(x)
			for _, m := range []Mode{Strict, Lenient} {
				got, err := c.decode(e, m)
				if err != nil || !bytes.Equal(got, x) {
					t.Fatalf("%s %s: decode(encode(%q)) = %q, %v (encoded %q)", c.name, m, x, got, err, e)
				}
			}
		}
	}
	for i := 0; i < testkit.Cases(); i++ {
		pairs := make([]FormPair, core.IntN(r, 4))
		for k := range pairs {
			pairs[k] = FormPair{string(randInput(r, textCodec{domain: utf8.Valid})), string(randInput(r, textCodec{domain: utf8.Valid}))}
		}
		s := EncodeForm(pairs)
		for _, m := range []Mode{Strict, Lenient} {
			got, err := ParseForm(s, m)
			if err != nil || !slices.Equal(got, pairs) && !(len(got) == 0 && len(pairs) == 0) {
				t.Fatalf("form %s: %q -> %q -> %q, %v", m, pairs, s, got, err)
			}
		}
	}
	for i := 0; i < testkit.Cases()/10; i++ {
		x := randBytes(r, core.IntN(r, 5000))
		if i%2 == 0 {
			x = bytes.Repeat(x[:min(len(x), 7)], core.IntN(r, 3000))
		}
		for _, k := range compressionKinds {
			got, err := Inflate(compress(t, k, x), k, InflateOptions{MaxRatio: -1})
			if err != nil || !bytes.Equal(got, x) {
				t.Fatalf("%s: round trip of %d bytes: %d bytes, %v", k, len(x), len(got), err)
			}
		}
	}
}

// TestCodecInv02_StrictCanonical: for the codecs with a unique encoding
// (base64, base32, hex, percent per set, form), strict decoding accepts s
// iff s is the encoding of its decoding (base32 and hex up to case).
func TestCodecInv02_StrictCanonical(t *testing.T) {
	r := testkit.Rng(t)
	for _, c := range allTextCodecs() {
		if !c.unique {
			continue
		}
		accepted, rejected := 0, 0
		for i := 0; i < 2*testkit.Cases(); i++ {
			s := randEncoded(r, c)
			x, err := c.decode(s, Strict)
			if err == nil {
				accepted++
				if !bytes.Equal(c.canon(c.encode(x)), c.canon(s)) {
					t.Fatalf("%s: strict accepted non-canonical %q (canonical %q)", c.name, s, c.encode(x))
				}
				continue
			}
			rejected++
			if y, err := c.decode(s, Lenient); err == nil && bytes.Equal(c.canon(c.encode(y)), c.canon(s)) {
				t.Fatalf("%s: strict rejected canonical %q: %v", c.name, s, err)
			}
		}
		if accepted == 0 || rejected == 0 {
			t.Fatalf("%s: generator too narrow (%d accepted, %d rejected)", c.name, accepted, rejected)
		}
	}
	for i := 0; i < testkit.Cases(); i++ {
		s := mutate(r, EncodeForm([]FormPair{{"a b", "c=d"}, {string(randInput(r, textCodec{domain: utf8.Valid})), "%"}}), "&=+%2Bx ")
		p, err := ParseForm(s, Strict)
		if err == nil && !bytes.Equal(EncodeForm(p), s) {
			t.Fatalf("form: strict accepted non-canonical %q", s)
		}
		if err != nil {
			if q, err2 := ParseForm(s, Lenient); err2 == nil && bytes.Equal(EncodeForm(q), s) {
				t.Fatalf("form: strict rejected canonical %q: %v", s, err)
			}
		}
	}
}

// TestCodecInv03_StrictConformsSpec: quoted-printable and JavaScript
// strings have many valid encodings, so strict mode is the
// specification's MUSTs: every encoder output is accepted (lines within
// 76 characters for QP) and each planted violation is rejected with its
// code, while lenient decoding is total.
func TestCodecInv03_StrictConformsSpec(t *testing.T) {
	r := testkit.Rng(t)
	qpViolations := []struct {
		add  string
		code Code
	}{{"\n", CodeInvalidByte}, {"\rx", CodeInvalidByte}, {" ", CodeNonCanonical}, {"\t\r\n", CodeNonCanonical},
		{"=", CodeTruncated}, {"=3d", CodeNonCanonical}, {"=G0", CodeInvalidEscape}, {"= \r\n", CodeInvalidEscape},
		{"\x01", CodeInvalidByte}, {"\xC3\xA9", CodeInvalidByte}, {"\x7f", CodeInvalidByte}}
	jsViolations := []struct {
		add  string
		code Code
	}{{`\101`, CodeInvalidEscape}, {`\08`, CodeInvalidEscape}, {`\8`, CodeInvalidEscape}, {"\n", CodeInvalidByte},
		{"\r", CodeInvalidByte}, {`"`, CodeInvalidByte}, {`\x4G`, CodeInvalidEscape}, {`\u{110000}`, CodeInvalidEscape},
		{`\u{}`, CodeInvalidEscape}, {`\uD8`, CodeTruncated}, {"\xff", CodeInvalidByte}, {`\`, CodeTruncated}}
	for i := 0; i < testkit.Cases(); i++ {
		x := randInput(r, textCodec{})
		e := EncodeQP(x)
		for _, line := range bytes.Split(e, []byte("\r\n")) {
			if len(line) > QPMaxLine {
				t.Fatalf("EncodeQP line of %d characters: %q", len(line), line)
			}
		}
		if got, err := DecodeQP(e, Strict); err != nil || !bytes.Equal(got, x) {
			t.Fatalf("qp strict rejects its encoding %q: %v", e, err)
		}
		v := qpViolations[i%len(qpViolations)]
		// After a hard break, so the violation starts a fresh line.
		bad := append(append([]byte{}, e...), "\r\n"+v.add...)
		if _, err := DecodeQP(bad, Strict); CodeOf(err) != v.code {
			t.Fatalf("qp strict %q: %v, want %s", bad, err, v.code)
		}
		if _, err := DecodeQP(bad, Lenient); err != nil {
			t.Fatalf("qp lenient %q: %v", bad, err)
		}
		// Joining two lines at a soft break makes a line too long.
		if k := bytes.Index(e, []byte("=\r\n")); k >= 0 {
			joined := append(append([]byte{}, e[:k]...), e[k+3:]...)
			if _, err := DecodeQP(joined, Strict); CodeOf(err) != CodeLineTooLong {
				t.Fatalf("qp strict joined %q: %v, want line_too_long", joined, err)
			}
		}

		y := randInput(r, textCodec{domain: validWTF8})
		if !validWTF8(y) {
			continue
		}
		je := EncodeJSString(y, '"')
		if got, err := DecodeJSString(je, JSOptions{Quote: '"'}); err != nil || !bytes.Equal(got, y) {
			t.Fatalf("js strict rejects its encoding %q: %v", je, err)
		}
		jv := jsViolations[i%len(jsViolations)]
		jbad := append(append([]byte{}, je...), jv.add...)
		_, err := DecodeJSString(jbad, JSOptions{Quote: '"'})
		if CodeOf(err) != jv.code {
			t.Fatalf("js strict %q: %v, want %s", jbad, err, jv.code)
		}
		if want := len(je); jv.code != CodeTruncated && err.(*Error).Offset != want {
			t.Fatalf("js strict %q: offset %d, want %d", jbad, err.(*Error).Offset, want)
		}
		if _, err := DecodeJSString(jbad, JSOptions{Mode: Lenient, Quote: '"'}); err != nil {
			t.Fatalf("js lenient %q: %v", jbad, err)
		}
	}
}

// TestCodecInv04_LenientSuperset: whatever strict mode accepts, lenient
// accepts with the same output.
func TestCodecInv04_LenientSuperset(t *testing.T) {
	r := testkit.Rng(t)
	for _, c := range allTextCodecs() {
		for i := 0; i < testkit.Cases(); i++ {
			s := randEncoded(r, c)
			x, err := c.decode(s, Strict)
			if err != nil {
				continue
			}
			y, err := c.decode(s, Lenient)
			if err != nil || !bytes.Equal(x, y) {
				t.Fatalf("%s: strict %q -> %q, lenient %q, %v", c.name, s, x, y, err)
			}
		}
	}
	for i := 0; i < testkit.Cases(); i++ {
		s := mutate(r, []byte("a=b&c+d=%41%2B&e="), "&=+%41 ")
		p, err := ParseForm(s, Strict)
		if err != nil {
			continue
		}
		q, err := ParseForm(s, Lenient)
		if err != nil || !slices.Equal(p, q) {
			t.Fatalf("form %q: strict %q lenient %q %v", s, p, q, err)
		}
	}
}

// TestCodecInv05_StdlibAgreement compares with the standard library as a
// differential oracle on random inputs: encoding/base64, encoding/base32,
// encoding/hex, net/url, mime/quotedprintable, encoding/json (JS
// escapes) and compress/* plus klauspost zstd.
func TestCodecInv05_StdlibAgreement(t *testing.T) {
	r := testkit.Rng(t)
	type b64v struct {
		o   Base64Options
		std *base64.Encoding
	}
	b64s := []b64v{{Base64Options{}, base64.StdEncoding}, {Base64Options{Alphabet: Base64URL}, base64.URLEncoding},
		{Base64Options{NoPadding: true}, base64.RawStdEncoding}, {Base64Options{Alphabet: Base64URL, NoPadding: true}, base64.RawURLEncoding}}
	type b32v struct {
		o   Base32Options
		std *base32.Encoding
	}
	b32s := []b32v{{Base32Options{}, base32.StdEncoding}, {Base32Options{Alphabet: Base32Hex}, base32.HexEncoding},
		{Base32Options{NoPadding: true}, base32.StdEncoding.WithPadding(base32.NoPadding)},
		{Base32Options{Alphabet: Base32Hex, NoPadding: true}, base32.HexEncoding.WithPadding(base32.NoPadding)}}
	for i := 0; i < 2*testkit.Cases(); i++ {
		x := randBytes(r, core.IntN(r, 80))
		for _, v := range b64s {
			if got := EncodeBase64(x, v.o); string(got) != v.std.EncodeToString(x) {
				t.Fatalf("base64 encode %q: %q vs %q", x, got, v.std.EncodeToString(x))
			}
			s := randEncoded(r, base64Codec("", v.o.Alphabet, v.o.NoPadding, 0))
			if want, err := v.std.DecodeString(string(s)); err == nil {
				o := v.o
				o.Mode = Lenient
				if got, err := DecodeBase64(s, o); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("base64 %+v lenient %q: %q %v, stdlib %q", v.o, s, got, err, want)
				}
			}
			if !bytes.ContainsAny(s, "\r\n") {
				want, errStd := v.std.Strict().DecodeString(string(s))
				got, err := DecodeBase64(s, v.o)
				if (err == nil) != (errStd == nil) || err == nil && !bytes.Equal(got, want) {
					t.Fatalf("base64 %+v strict %q: %q %v, stdlib %q %v", v.o, s, got, err, want, errStd)
				}
			}
		}
		for _, v := range b32s {
			if got := EncodeBase32(x, v.o); string(got) != v.std.EncodeToString(x) {
				t.Fatalf("base32 encode %q: %q vs %q", x, got, v.std.EncodeToString(x))
			}
			s := randEncoded(r, base32Codec("", v.o.Alphabet, v.o.NoPadding))
			want, errStd := v.std.DecodeString(string(s))
			// PORT: unpadded encoding/base32 returns no bytes and no error
			// for data lengths no encoder emits (1, 3 or 6 mod 8), dropping
			// the whole final group; ours reports CodeTruncated
			// (OPEN_QUESTIONS.md), so those inputs are not compared.
			n := len(s) - bytes.Count(s, []byte("\r")) - bytes.Count(s, []byte("\n"))
			// It also ignores data after the padding (RFC 4648 §3.3 allows
			// either); ours rejects it.
			afterPad := false
			if p := bytes.IndexByte(s, '='); p >= 0 {
				afterPad = len(bytes.Trim(s[p:], "=\r\n")) > 0
			}
			if afterPad || v.o.NoPadding && (n%8 == 1 || n%8 == 3 || n%8 == 6) {
				errStd = errors.New("skipped")
			}
			if errStd == nil {
				o := v.o
				o.Mode = Lenient
				if got, err := DecodeBase32(s, o); err != nil || !bytes.Equal(got, want) {
					t.Fatalf("base32 %+v lenient %q: %q %v, stdlib %q", v.o, s, got, err, want)
				}
			}
			if got, err := DecodeBase32(s, v.o); err == nil && !bytes.ContainsAny(s, "\r\n") {
				// Strict output is canonical up to case; stdlib decodes the
				// upper-case form identically.
				want, errStd := v.std.DecodeString(strings.ToUpper(string(s)))
				if errStd != nil || !bytes.Equal(got, want) {
					t.Fatalf("base32 %+v strict %q: %q, stdlib %q %v", v.o, s, got, want, errStd)
				}
			}
		}
		if got := EncodeHex(x, HexOptions{}); string(got) != hex.EncodeToString(x) {
			t.Fatalf("hex encode %q", x)
		}
		hs := randEncoded(r, hexCodec("", false))
		want, errStd := hex.DecodeString(string(hs))
		got, err := DecodeHex(hs, HexOptions{})
		if (err == nil) != (errStd == nil) || err == nil && !bytes.Equal(got, want) {
			t.Fatalf("hex strict %q: %q %v, stdlib %q %v", hs, got, err, want, errStd)
		}

		// net/url: PathUnescape accepts exactly the RFC 3986 syntax
		// check; QueryUnescape adds '+'; both invert our encoders.
		ps := randEncoded(r, percentCodec(SetComponent))
		wantP, errP := url.PathUnescape(string(ps))
		gotP, err := DecodePercent(ps, PercentOptions{Mode: Lenient, RejectInvalidEscapes: true})
		if (err == nil) != (errP == nil) || err == nil && string(gotP) != wantP {
			t.Fatalf("percent %q: %q %v, net/url %q %v", ps, gotP, err, wantP, errP)
		}
		if wantQ, errQ := url.QueryUnescape(string(ps)); errQ == nil {
			if gotQ, err := AppendDecodeFormComponent(nil, ps, Lenient); err != nil || string(gotQ) != wantQ {
				t.Fatalf("form component %q: %q %v, net/url %q", ps, gotQ, err, wantQ)
			}
		}
		for s := PercentSet(0); s < numPercentSets; s++ {
			if back, err := url.PathUnescape(string(EncodePercent(x, s))); err != nil || back != string(x) {
				t.Fatalf("percent %s: net/url does not invert %q: %q %v", s, EncodePercent(x, s), back, err)
			}
		}
		if back, err := url.QueryUnescape(string(AppendEncodeFormComponent(nil, x))); err != nil || back != string(x) {
			t.Fatalf("form: net/url does not invert %q", x)
		}
		fs := mutate(r, []byte("a=b&c+d=%41%2B&e=&=x&k=v%2"), "&=+%;4 é")
		if vals, err := url.ParseQuery(string(fs)); err == nil {
			pairs, err := ParseForm(fs, Lenient)
			if err != nil {
				t.Fatalf("form %q: %v", fs, err)
			}
			got := url.Values{}
			valid := true
			for _, p := range pairs {
				got[p.Name] = append(got[p.Name], p.Value)
			}
			for k, vs := range vals {
				valid = valid && utf8.ValidString(k) && !slices.ContainsFunc(vs, func(s string) bool { return !utf8.ValidString(s) })
			}
			if valid && got.Encode() != vals.Encode() {
				t.Fatalf("form %q: %v, net/url %v", fs, got, vals)
			}
		}

		// mime/quotedprintable: lenient agrees wherever it accepts, and
		// it inverts EncodeQP; strict accepts its Binary writer output.
		qs := randEncoded(r, codecByName(t, "qp"))
		if want, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(qs))); err == nil {
			if got, err := DecodeQP(qs, Lenient); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("qp lenient %q: %q %v, mime %q", qs, got, err, want)
			}
		}
		if back, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(EncodeQP(x)))); err != nil || !bytes.Equal(back, x) {
			t.Fatalf("qp: mime does not invert EncodeQP(%q): %q %v", x, back, err)
		}
		var qb bytes.Buffer
		qw := quotedprintable.NewWriter(&qb)
		qw.Binary = true
		qw.Write(x)
		qw.Close()
		if got, err := DecodeQP(qb.Bytes(), Strict); err != nil || !bytes.Equal(got, x) {
			t.Fatalf("qp strict of mime writer output %q: %q %v", qb.Bytes(), got, err)
		}

		// encoding/json string escapes are a subset of JavaScript's.
		text := randInput(r, textCodec{domain: utf8.Valid})
		if utf8.Valid(text) {
			js, _ := json.Marshal(string(text))
			if got, err := DecodeJSString(js[1:len(js)-1], JSOptions{Quote: '"'}); err != nil || !bytes.Equal(got, text) {
				t.Fatalf("js of json %s: %q %v", js, got, err)
			}
		}
	}
	// Compressed streams, intact and corrupted: same success and output
	// as the reference readers.
	for i := 0; i < testkit.Cases()/5; i++ {
		x := randBytes(r, core.IntN(r, 3000))
		for _, k := range compressionKinds {
			c := compress(t, k, x)
			if core.IntN(r, 2) == 0 && len(c) > 0 {
				c = mutate(r, c, "\x00\xff\x01")
			}
			got, err := Inflate(c, k, InflateOptions{MaxRatio: -1})
			want, errRef := refInflate(k, c)
			if (err == nil) != (errRef == nil) || err == nil && !bytes.Equal(got, want) {
				t.Fatalf("%s %x: %d bytes %v, reference %d bytes %v", k, c, len(got), err, len(want), errRef)
			}
		}
	}
}

func refInflate(k Kind, c []byte) ([]byte, error) {
	switch k {
	case KindGzip:
		zr, err := gzip.NewReader(bytes.NewReader(c))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(zr)
	case KindZlib:
		zr, err := zlib.NewReader(bytes.NewReader(c))
		if err != nil {
			return nil, err
		}
		return io.ReadAll(zr)
	case KindDeflate:
		return io.ReadAll(flate.NewReader(bytes.NewReader(c)))
	}
	d, _ := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
	defer d.Close()
	return d.DecodeAll(c, nil)
}

// TestCodecInv06_Base64AlgosAgree: every Base64Algo candidate returns the
// same bytes, or the same error code and offset, for every option set and
// input, appended to any dst.
func TestCodecInv06_Base64AlgosAgree(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < 4*testkit.Cases(); i++ {
		o := Base64Options{Alphabet: Base64Alphabet(core.IntN(r, 2)), NoPadding: core.IntN(r, 2) == 0, Mode: Mode(core.IntN(r, 2))}
		s := randEncoded(r, base64Codec("", o.Alphabet, o.NoPadding, 0))
		if i%3 == 0 {
			// Long, MIME-wrapped input exercises the fast paths' hand-offs.
			s = EncodeBase64(randBytes(r, core.IntN(r, 600)), o)
			for k := 76; k < len(s); k += 78 {
				s = slices.Insert(s, k, '\r', '\n')
			}
			if core.IntN(r, 2) == 0 {
				s = mutate(r, s, b64Chars)
			}
		}
		prefix := randBytes(r, core.IntN(r, 5))
		o.Algo = Base64AlgoStateMachine
		want, wantErr := AppendDecodeBase64(append([]byte{}, prefix...), s, o)
		for _, a := range Base64Algos()[1:] {
			o.Algo = a
			got, err := AppendDecodeBase64(append([]byte{}, prefix...), s, o)
			if !bytes.Equal(got, want) || CodeOf(err) != CodeOf(wantErr) ||
				err != nil && err.(*Error).Offset != wantErr.(*Error).Offset {
				t.Fatalf("%s %+v %q: %q %v, statemachine %q %v", a, o, s, got, err, want, wantErr)
			}
		}
	}
}

// zstdRLEBomb is a zstd frame of n RLE blocks of 128 KiB zeros: about 4
// input bytes per 128 KiB of output (RFC 8878 §3.1.1.2, block type 1).
func zstdRLEBomb(n int) []byte {
	b := []byte{0x28, 0xB5, 0x2F, 0xFD, 0x00, 0x38} // magic, FHD, window 2^17
	for i := 0; i < n; i++ {
		h := uint32(1<<1 | (128<<10)<<3)
		if i == n-1 {
			h |= 1
		}
		b = append(b, byte(h), byte(h>>8), byte(h>>16), 0)
	}
	return b
}

// TestCodecInv07_OutputLimits: decompression never returns more than
// min(MaxOutput, max(RatioFloor, MaxRatio × input)); at the cap it
// returns exactly that prefix with ErrOutputLimit, so bombs stop early;
// below it the output is complete. Text decoders never output more bytes
// than they read.
func TestCodecInv07_OutputLimits(t *testing.T) {
	zeros := make([]byte, 12<<20)
	for _, k := range compressionKinds {
		bomb := compress(t, k, zeros)
		if k == KindZstd {
			bomb = zstdRLEBomb(8192) // 1 GiB
		}
		cap0, code := InflateOptions{}.limit(len(bomb))
		got, err := Inflate(bomb, k, InflateOptions{})
		if CodeOf(err) != code || code != CodeRatioLimit || len(got) != cap0 || bytes.IndexFunc(got, func(r rune) bool { return r != 0 }) >= 0 {
			t.Fatalf("%s bomb (%d bytes): %d bytes, %v; want %d bytes, %s", k, len(bomb), len(got), err, cap0, code)
		}
		if !errors.Is(err, ErrOutputLimit) || err.(*Error).Offset != cap0 {
			t.Fatalf("%s bomb: %v", k, err)
		}
		got, err = Inflate(bomb, k, InflateOptions{MaxOutput: 4 << 20, MaxRatio: -1})
		if CodeOf(err) != CodeOutputLimit || len(got) != 4<<20 {
			t.Fatalf("%s bomb without ratio guard: %d bytes, %v", k, len(got), err)
		}
	}
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases()/10; i++ {
		x := bytes.Repeat(randBytes(r, 1+core.IntN(r, 9)), core.IntN(r, 2000))
		k := compressionKinds[core.IntN(r, len(compressionKinds))]
		c := compress(t, k, x)
		o := InflateOptions{MaxOutput: 1 + core.IntN(r, 4000), MaxRatio: core.IntN(r, 4) - 1, RatioFloor: 1 + core.IntN(r, 500)}
		lim, code := o.limit(len(c))
		got, err := AppendInflate([]byte("pre"), c, k, o)
		if !bytes.HasPrefix(got, []byte("pre")) {
			t.Fatalf("dst prefix lost")
		}
		got = got[3:]
		switch {
		case len(x) > lim:
			if CodeOf(err) != code || !bytes.Equal(got, x[:lim]) {
				t.Fatalf("%s %+v: %d of %d bytes, %v; want prefix %d, %s", k, o, len(got), len(x), err, lim, code)
			}
		case err != nil || !bytes.Equal(got, x):
			t.Fatalf("%s %+v: %d of %d bytes, %v", k, o, len(got), len(x), err)
		}
	}
	for _, c := range allTextCodecs() {
		for i := 0; i < testkit.Cases()/2; i++ {
			s := randEncoded(r, c)
			for _, m := range []Mode{Strict, Lenient} {
				if got, err := c.decode(s, m); err == nil && len(got) > len(s) {
					t.Fatalf("%s: %d bytes from %d", c.name, len(got), len(s))
				}
			}
		}
	}
}

// layerWrap is one random encoding step for TestCodecInv08.
func layerWrap(t *testing.T, r core.Rng, data []byte, compressed bool, prev Kind) ([]byte, Kind, bool) {
	if !compressed && core.IntN(r, 2) == 0 {
		// Raw deflate has no magic, so Sniff never finds it.
		k := []Kind{KindGzip, KindZlib, KindZstd}[core.IntN(r, 3)]
		return compress(t, k, data), k, true
	}
	switch core.IntN(r, 3) {
	case 0:
		if prev != KindHex {
			// Hex of hex is all digits, which Sniff takes for a number.
			return EncodeHex(data, HexOptions{}), KindHex, false
		}
		fallthrough
	case 1:
		enc := EncodeBase64(data, Base64Options{Alphabet: Base64URL, NoPadding: true})
		k := KindBase64
		if bytes.ContainsAny(enc, "-_") {
			k = KindBase64URL
		}
		return enc, k, false
	}
	return EncodeBase64(data, Base64Options{}), KindBase64, false
}

// TestCodecInv08_LayersBounded: DecodeLayers terminates, never goes past
// MaxDepth, never keeps more than MaxTotal bytes or MaxNodes layers, and
// its arena is consistent (parents precede children, depth = parent + 1,
// spans inside the parent). A payload wrapped k ≤ MaxDepth times is found
// as text at depth k with the wrapping kinds as its path; deeper payloads
// are not reached. A gzip-in-gzip bomb stays within the total.
func TestCodecInv08_LayersBounded(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases()/5; i++ {
		payload := []byte("endpoint /api/v" + string(rune('0'+core.IntN(r, 10))) + "/internal/" +
			string(EncodeHex(randBytes(r, 6), HexOptions{})) + " token=none; " + strings.Repeat("Ab9", 4))
		data, compressed := payload, false
		var path []Kind
		depth := 1 + core.IntN(r, 5)
		for d := 0; d < depth; d++ {
			// After a compression step the next one is always a text
			// encoding: binary is only sniffed at the start of a buffer.
			var k, prev Kind
			if len(path) > 0 {
				prev = path[0]
			}
			data, k, compressed = layerWrap(t, r, data, compressed, prev)
			path = append([]Kind{k}, path...)
		}
		in := data
		if !compressed {
			in = []byte("var x = \"" + string(data) + "\";")
		}
		o := LayersOptions{MaxDepth: 1 + core.IntN(r, 5)}
		if core.IntN(r, 4) == 0 {
			o.MaxTotal = 1 + core.IntN(r, 300)
		}
		res := DecodeLayers(in, o)
		checkLayers(t, in, res, o)
		if o.MaxTotal == 0 && len(path) <= o.MaxDepth {
			found := false
			for idx, n := range res.Nodes {
				if n.Depth == len(path) && n.Text && bytes.Equal(n.Data, payload) {
					// Walk to the root and compare the path of kinds.
					var kinds []Kind
					for j := idx; j >= 0; j = res.Nodes[j].Parent {
						kinds = append([]Kind{res.Nodes[j].Kind}, kinds...)
					}
					found = slices.Equal(kinds, path)
				}
			}
			if !found {
				t.Fatalf("payload behind %v not found (max depth %d): %+v", path, o.MaxDepth, res.Nodes)
			}
		}
	}
	bomb := EncodeBase64(compress(t, KindGzip, compress(t, KindGzip, make([]byte, 12<<20))), Base64Options{})
	for _, total := range []int{0, 100_000} {
		o := LayersOptions{MaxTotal: total}
		in := append([]byte("x="), bomb...)
		res := DecodeLayers(in, o)
		checkLayers(t, in, res, o)
		if total > 0 && !res.Truncated {
			t.Fatalf("bomb within %d bytes not truncated: %d", total, res.Total)
		}
	}
}

func checkLayers(t *testing.T, in []byte, res Layers, o LayersOptions) {
	t.Helper()
	maxDepth, maxTotal, maxNodes := o.MaxDepth, o.MaxTotal, o.MaxNodes
	if maxDepth <= 0 {
		maxDepth = DefaultMaxDepth
	}
	if maxTotal <= 0 {
		maxTotal = DefaultMaxTotal
	}
	if maxNodes <= 0 {
		maxNodes = DefaultMaxNodes
	}
	total := 0
	for i, n := range res.Nodes {
		parentData, parentDepth := in, 0
		if n.Parent >= 0 {
			if n.Parent >= i {
				t.Fatalf("layer %d: parent %d does not precede it", i, n.Parent)
			}
			parentData, parentDepth = res.Nodes[n.Parent].Data, res.Nodes[n.Parent].Depth
		}
		if n.Depth != parentDepth+1 || n.Depth > maxDepth || n.Start < 0 || n.End > len(parentData) || n.Start >= n.End {
			t.Fatalf("layer %d inconsistent: %+v (parent depth %d, parent %d bytes)", i, n, parentDepth, len(parentData))
		}
		if n.Text != looksLikeTextPrefix(n.Data, n.Truncated) {
			t.Fatalf("layer %d: Text flag wrong", i)
		}
		total += len(n.Data)
	}
	if total != res.Total || total > maxTotal || len(res.Nodes) > maxNodes {
		t.Fatalf("total %d (reported %d) of %d, %d nodes", total, res.Total, maxTotal, len(res.Nodes))
	}
}

// TestCodecInv09_SniffSound: Sniff finds exactly the planted blobs (base64
// of either alphabet, hex) in filler text of words and camel-case
// identifiers, with exact spans; on any input its candidates are sorted,
// disjoint, inside the input, and decode with their kind.
func TestCodecInv09_SniffSound(t *testing.T) {
	r := testkit.Rng(t)
	words := []string{"the", "fetch", "addEventListener", "getElementById", "window", "return", "function",
		"const", "innerHTML", "/api/users", "x", "=>", "{", "}", "querySelectorAll", "README", "typeof"}
	delims := []string{" ", "\"", "'", "(", ")", ";", ",", "\n", "\t"}
	for i := 0; i < testkit.Cases(); i++ {
		var b []byte
		var want []Candidate
		for k := 0; k < 1+core.IntN(r, 12); k++ {
			b = append(b, delims[core.IntN(r, len(delims))]...)
			if core.IntN(r, 3) > 0 {
				b = append(b, words[core.IntN(r, len(words))]...)
				continue
			}
			raw := randBytes(r, 48+core.IntN(r, 80))
			var enc []byte
			kind := KindHex
			switch core.IntN(r, 3) {
			case 0:
				enc = EncodeHex(raw[:16+core.IntN(r, 32)], HexOptions{Upper: core.IntN(r, 2) == 0})
			case 1:
				enc, kind = EncodeBase64(raw, Base64Options{}), KindBase64
			default:
				enc, kind = EncodeBase64(raw, Base64Options{Alphabet: Base64URL, NoPadding: core.IntN(r, 2) == 0}), KindBase64URL
				if !bytes.ContainsAny(enc, "-_") {
					kind = KindBase64
				}
			}
			want = append(want, Candidate{Kind: kind, Start: len(b), End: len(b) + len(enc)})
			b = append(b, enc...)
			b = append(b, delims[core.IntN(r, len(delims))]...)
		}
		got := Sniff(b, SniffOptions{})
		if !slices.Equal(got, want) {
			t.Fatalf("Sniff(%q) = %+v, want %+v", b, got, want)
		}
		checkSniff(t, b, got)
		m := mutate(r, b, "=+/-_\r\n AZaz09")
		checkSniff(t, m, Sniff(m, SniffOptions{}))
	}
}

func checkSniff(t *testing.T, b []byte, cands []Candidate) {
	t.Helper()
	end := 0
	for _, c := range cands {
		if c.Start < end || c.End <= c.Start || c.End > len(b) {
			t.Fatalf("candidate %+v out of order or bounds (previous end %d, len %d)", c, end, len(b))
		}
		if !candidateDecodes(b, c) {
			t.Fatalf("candidate %+v does not decode: %q", c, b[c.Start:c.End])
		}
		end = c.End
	}
}

// TestCodecInv10_Determinism: every encoder and decoder gives the same
// bytes on repeated calls, Append variants append exactly what the plain
// function returns whatever dst holds, and Sniff and DecodeLayers repeat
// exactly.
func TestCodecInv10_Determinism(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		x := randInput(r, textCodec{})
		pre := randBytes(r, core.IntN(r, 6))
		app := func(f func([]byte) []byte) []byte { return f(append([]byte{}, pre...))[len(pre):] }
		pairs := []struct {
			name string
			a, b []byte
		}{
			{"base64", EncodeBase64(x, Base64Options{}), app(func(d []byte) []byte { return AppendEncodeBase64(d, x, Base64Options{}) })},
			{"base32", EncodeBase32(x, Base32Options{}), app(func(d []byte) []byte { return AppendEncodeBase32(d, x, Base32Options{}) })},
			{"hex", EncodeHex(x, HexOptions{}), app(func(d []byte) []byte { return AppendEncodeHex(d, x, HexOptions{}) })},
			{"percent", EncodePercent(x, SetPath), app(func(d []byte) []byte { return AppendEncodePercent(d, x, SetPath) })},
			{"qp", EncodeQP(x), app(func(d []byte) []byte { return AppendEncodeQP(d, x) })},
			{"js", EncodeJSString(x, 0), app(func(d []byte) []byte { return AppendEncodeJSString(d, x, 0) })},
		}
		for _, p := range pairs {
			if !bytes.Equal(p.a, p.b) {
				t.Fatalf("%s: Append differs: %q vs %q", p.name, p.a, p.b)
			}
		}
		for _, c := range allTextCodecs() {
			s := randEncoded(r, c)
			a, errA := c.decode(s, Lenient)
			b, errB := c.decode(s, Lenient)
			if !bytes.Equal(a, b) || CodeOf(errA) != CodeOf(errB) {
				t.Fatalf("%s not deterministic on %q", c.name, s)
			}
		}
		doc := append(append([]byte("k="), EncodeBase64(compress(t, KindGzip, x), Base64Options{})...), mutate(r, x, "=+/")...)
		if !slices.Equal(Sniff(doc, SniffOptions{}), Sniff(doc, SniffOptions{})) {
			t.Fatal("Sniff not deterministic")
		}
		l1, l2 := DecodeLayers(doc, LayersOptions{}), DecodeLayers(doc, LayersOptions{})
		if len(l1.Nodes) != len(l2.Nodes) || l1.Total != l2.Total || l1.Truncated != l2.Truncated {
			t.Fatal("DecodeLayers not deterministic")
		}
		for k := range l1.Nodes {
			a, b := l1.Nodes[k], l2.Nodes[k]
			if a.Kind != b.Kind || a.Start != b.Start || a.End != b.End || a.Parent != b.Parent || !bytes.Equal(a.Data, b.Data) {
				t.Fatal("DecodeLayers nodes differ")
			}
		}
	}
}

// TestCodecInv11_StableErrors: every decoder error is an *Error with a
// code from the closed list that unwraps to its sentinel, with an offset
// inside the input (text codecs) or the returned output (streams), and an
// Append decoder that fails leaves dst unchanged.
func TestCodecInv11_StableErrors(t *testing.T) {
	r := testkit.Rng(t)
	seen := map[Code]bool{}
	for _, c := range allTextCodecs() {
		for i := 0; i < testkit.Cases()/2; i++ {
			s := randEncoded(r, c)
			for _, m := range []Mode{Strict, Lenient} {
				_, err := c.decode(s, m)
				checkErr(t, c.name, err, len(s))
				seen[CodeOf(err)] = true
			}
		}
	}
	for i := 0; i < testkit.Cases()/2; i++ {
		s := randEncoded(r, codecByName(t, "base64"))
		pre := []byte("keep")
		for _, f := range []func([]byte) ([]byte, error){
			func(d []byte) ([]byte, error) { return AppendDecodeBase64(d, s, Base64Options{}) },
			func(d []byte) ([]byte, error) { return AppendDecodeBase32(d, s, Base32Options{}) },
			func(d []byte) ([]byte, error) { return AppendDecodeHex(d, s, HexOptions{}) },
			func(d []byte) ([]byte, error) { return AppendDecodePercent(d, s, PercentOptions{}) },
			func(d []byte) ([]byte, error) { return AppendDecodeQP(d, s, Strict) },
			func(d []byte) ([]byte, error) { return AppendDecodeJSString(d, s, JSOptions{}) },
		} {
			out, err := f(append([]byte{}, pre...))
			if err != nil && !bytes.Equal(out, pre) {
				t.Fatalf("failed Append changed dst: %q", out)
			}
		}
		k := compressionKinds[i%len(compressionKinds)]
		c := mutate(r, compress(t, k, randBytes(r, 50)), "\x00\xff")
		out, err := Inflate(c, k, InflateOptions{MaxOutput: 1 + core.IntN(r, 60)})
		checkErr(t, k.String(), err, -1)
		if err != nil && err.(*Error).Offset != len(out) {
			t.Fatalf("%s: offset %d, %d bytes returned", k, err.(*Error).Offset, len(out))
		}
		seen[CodeOf(err)] = true
		_, err = ParseForm(s, Strict)
		checkErr(t, "form", err, len(s))
	}
	if _, err := Inflate([]byte("x"), KindHex, InflateOptions{}); CodeOf(err) != CodeUnsupported || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Inflate of hex: %v", err)
	}
	for _, c := range Codes() {
		if strings.HasPrefix(c.String(), "code(") || !strings.HasPrefix(c.Sentinel().Error(), "codec: ") {
			t.Fatalf("code %d has no name or sentinel", c)
		}
	}
	for _, c := range []Code{CodeInvalidByte, CodeBadPadding, CodeTrailingBits, CodeTruncated, CodeInvalidEscape,
		CodeNonCanonical, CodeLineTooLong, CodeOutputLimit} {
		if !seen[c] {
			t.Fatalf("random inputs never produced %s", c)
		}
	}
	if (&Error{Code: CodeOutputLimit, Offset: 3}).Error() != "codec: output limit (output_limit at byte 3)" {
		t.Fatal("error text changed")
	}
}
