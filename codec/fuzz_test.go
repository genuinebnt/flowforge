package codec

import (
	"bytes"
	"io"
	"mime/quotedprintable"
	"testing"
)

// fuzzText asserts, for one input of one codec, the decoder invariants
// that hold on arbitrary bytes: stable errors (11), lenient ⊇ strict (04),
// strict canonical (02, unique codecs), output no longer than the input
// (07) and determinism (10).
func fuzzText(t *testing.T, c textCodec, s []byte) {
	x, errS := c.decode(s, Strict)
	y, errL := c.decode(s, Lenient)
	checkErr(t, c.name+" strict", errS, len(s))
	checkErr(t, c.name+" lenient", errL, len(s))
	if errS == nil {
		if errL != nil || !bytes.Equal(x, y) {
			t.Fatalf("%s: strict %q, lenient %q %v", c.name, x, y, errL)
		}
		if c.unique && !bytes.Equal(c.canon(c.encode(x)), c.canon(s)) {
			t.Fatalf("%s: strict accepted non-canonical %q", c.name, s)
		}
	}
	if errL == nil && len(y) > len(s) {
		t.Fatalf("%s: %d bytes out of %d", c.name, len(y), len(s))
	}
	if errL == nil && c.unique && errS != nil && bytes.Equal(c.canon(c.encode(y)), c.canon(s)) {
		t.Fatalf("%s: strict rejected canonical %q: %v", c.name, s, errS)
	}
	if z, err := c.decode(s, Lenient); !bytes.Equal(z, y) || CodeOf(err) != CodeOf(errL) {
		t.Fatalf("%s: not deterministic", c.name)
	}
	if c.domain == nil || c.domain(s) {
		if back, err := c.decode(c.encode(s), Strict); err != nil || !bytes.Equal(back, s) {
			t.Fatalf("%s: round trip of %q: %q %v", c.name, s, back, err)
		}
	}
}

func fuzzCodecs(f *testing.F, prefix string, seeds ...string) {
	var cs []textCodec
	for _, c := range allTextCodecs() {
		if len(c.name) >= len(prefix) && c.name[:len(prefix)] == prefix {
			cs = append(cs, c)
		}
	}
	for _, s := range seeds {
		f.Add([]byte(s), uint8(0))
	}
	f.Fuzz(func(t *testing.T, s []byte, which uint8) {
		fuzzText(t, cs[int(which)%len(cs)], s)
	})
}

// FuzzCodecBase64 also checks that every Base64Algo candidate agrees
// (CODEC-INV-06).
func FuzzCodecBase64(f *testing.F) {
	for _, s := range []string{"Zm9vYmFy", "Zg==", "Zh==", "Zm9v\r\nYmFy", "-_8=", " Z g =\t\n= ", "Zg==Zg==", "QUJD="} {
		f.Add([]byte(s), uint8(0))
	}
	f.Fuzz(func(t *testing.T, s []byte, opts uint8) {
		o := Base64Options{Alphabet: Base64Alphabet(opts & 1), NoPadding: opts&2 != 0, Mode: Mode(opts >> 2 & 1)}
		fuzzText(t, base64Codec("base64", o.Alphabet, o.NoPadding, 0), s)
		o.Algo = Base64AlgoStateMachine
		want, wantErr := DecodeBase64(s, o)
		for _, a := range Base64Algos()[1:] {
			o.Algo = a
			got, err := DecodeBase64(s, o)
			if !bytes.Equal(got, want) || CodeOf(err) != CodeOf(wantErr) || err != nil && err.(*Error).Offset != wantErr.(*Error).Offset {
				t.Fatalf("%s %+v: %q %v, statemachine %q %v", a, o, got, err, want, wantErr)
			}
		}
	})
}

// FuzzCodecBase32 covers both alphabets, padded and unpadded.
func FuzzCodecBase32(f *testing.F) {
	fuzzCodecs(f, "base32", "MZXW6YTBOI======", "mzxw6", "MY===", "CPNMUOJ1", "MZXW6YQ===")
}

// FuzzCodecHex covers both encoder cases.
func FuzzCodecHex(f *testing.F) { fuzzCodecs(f, "hex", "DeadBEEF", "abc", " de ad\n", "0g") }

// FuzzCodecPercent covers every percent-encode set and form components,
// and runs ParseForm on the same input.
func FuzzCodecPercent(f *testing.F) {
	for _, s := range []string{"a%20b", "%2f", "%zz%4", "%%41", "a=b&c=%FF&&d", "q=a+b%2B", "x=%E2%82"} {
		f.Add([]byte(s), uint8(0))
	}
	var cs []textCodec
	for _, c := range textCodecs() {
		if c.name == "form-component" || len(c.name) > 8 && c.name[:8] == "percent-" {
			cs = append(cs, c)
		}
	}
	f.Fuzz(func(t *testing.T, s []byte, which uint8) {
		fuzzText(t, cs[int(which)%len(cs)], s)
		p, err := ParseForm(s, Strict)
		checkErr(t, "form", err, len(s))
		if err == nil && !bytes.Equal(EncodeForm(p), s) {
			t.Fatalf("form strict accepted non-canonical %q", s)
		}
		q, err := ParseForm(s, Lenient)
		if err != nil {
			t.Fatalf("form lenient failed: %v", err)
		}
		for _, pair := range q {
			if !isValidUTF8(pair.Name) || !isValidUTF8(pair.Value) {
				t.Fatalf("form lenient returned invalid UTF-8: %q", q)
			}
		}
	})
}

func isValidUTF8(s string) bool { return bytes.Equal([]byte(s), appendUTF8Replacing(nil, []byte(s))) }

// FuzzCodecQP: lenient decoding is total and agrees with
// mime/quotedprintable wherever that accepts (CODEC-INV-05).
func FuzzCodecQP(f *testing.F) {
	f.Add([]byte("Now's the time =\r\nfor all folk to come=\r\n to the aid"), uint8(0))
	f.Add([]byte("a= \t\r\nb=3d=XY \r\n=4"), uint8(0))
	f.Fuzz(func(t *testing.T, s []byte, _ uint8) {
		c := codecByName(t, "qp")
		fuzzText(t, c, s)
		got, err := DecodeQP(s, Lenient)
		if err != nil {
			t.Fatalf("lenient failed: %v", err)
		}
		if want, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(s))); err == nil && !bytes.Equal(got, want) {
			t.Fatalf("lenient %q, mime %q", got, want)
		}
	})
}

// FuzzCodecJSString: lenient decoding is total; strict ⊆ lenient; UTF-8
// and WTF-8 inputs round-trip through EncodeJSString.
func FuzzCodecJSString(f *testing.F) {
	for _, s := range []string{`\x41B\u{43}`, `😀`, `\101\08\8`, "a\\\r\nb", `\u{110000}`, `\x4G`, `\`} {
		f.Add([]byte(s), uint8(0))
	}
	f.Fuzz(func(t *testing.T, s []byte, _ uint8) {
		fuzzText(t, codecByName(t, "js"), s)
		if _, err := DecodeJSString(s, JSOptions{Mode: Lenient, Surrogates: SurrogateReplace}); err != nil {
			t.Fatalf("lenient failed: %v", err)
		}
	})
}

// FuzzCodecInflate: every format, any bytes, any cap: the output never
// exceeds the cap, errors are stable, and output matches the reference
// reader when neither fails.
func FuzzCodecInflate(f *testing.F) {
	for _, k := range compressionKinds {
		f.Add(compress(f, k, []byte("hello, hello, hello world")), uint8(k), uint16(10))
		f.Add(compress(f, k, make([]byte, 5000)), uint8(k), uint16(100))
	}
	f.Add(zstdRLEBomb(64), uint8(KindZstd), uint16(1000))
	f.Fuzz(func(t *testing.T, src []byte, kind uint8, capOut uint16) {
		k := compressionKinds[int(kind)%len(compressionKinds)]
		o := InflateOptions{MaxOutput: int(capOut) + 1, RatioFloor: 64}
		lim, _ := o.limit(len(src))
		out, err := Inflate(src, k, o)
		checkErr(t, k.String(), err, -1)
		if len(out) > lim || err != nil && err.(*Error).Offset != len(out) {
			t.Fatalf("%s: %d bytes over cap %d (%v)", k, len(out), lim, err)
		}
		if err == nil {
			if want, errRef := refInflate(k, src); errRef == nil && !bytes.Equal(out, want) {
				t.Fatalf("%s: %q, reference %q", k, out, want)
			}
		}
	})
}

// FuzzCodecSniff: candidates are sorted, disjoint, in bounds, decodable
// and deterministic (CODEC-INV-09).
func FuzzCodecSniff(f *testing.F) {
	f.Add([]byte("token=ZmV0Y2goJy9hcGkvdjIvcHJpdmF0ZS9leHBvcnQ/Zm9ybWF0PWNzdicp; sha=ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"))
	f.Add(compress(f, KindGzip, []byte("x")))
	f.Fuzz(func(t *testing.T, b []byte) {
		c := Sniff(b, SniffOptions{})
		checkSniff(t, b, c)
		if d := Sniff(b, SniffOptions{}); len(d) != len(c) {
			t.Fatal("not deterministic")
		}
	})
}

// FuzzCodecDecodeLayers: bounded and consistent on any input
// (CODEC-INV-08).
func FuzzCodecDecodeLayers(f *testing.F) {
	f.Add([]byte("v='H4sIAAAAAAAA/6pWSizIVLJS0s/MK0ktykvM0U9Myc3M0y8tTi0qVtJRKsnPTs1TslLKy89LVaoFBAAA//+u2pSkLgAAAA=='"), uint16(0))
	f.Add(EncodeBase64(EncodeBase64([]byte("x=\"ZmV0Y2goJy9hcGkvdjIvcHJpdmF0ZS9leHBvcnQ/Zm9ybWF0PWNzdicp\""), Base64Options{}), Base64Options{}), uint16(50))
	f.Fuzz(func(t *testing.T, b []byte, total uint16) {
		o := LayersOptions{MaxTotal: int(total), MaxDepth: int(total % 5)}
		checkLayers(t, b, DecodeLayers(b, o), o)
	})
}
