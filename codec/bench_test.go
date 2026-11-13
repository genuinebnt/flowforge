package codec

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

var benchSizes = []int{64, 4 << 10, 1 << 20}

func sizeName(n int) string {
	switch {
	case n >= 1<<20:
		return strconv.Itoa(n>>20) + "MiB"
	case n >= 1<<10:
		return strconv.Itoa(n>>10) + "KiB"
	}
	return strconv.Itoa(n) + "B"
}

func benchData(n int) []byte { return randBytes(core.NewSplitMix64(uint64(n)), n) }

// mimeWrap inserts CRLF every 76 characters (RFC 2045 §6.8).
func mimeWrap(b []byte) []byte {
	var out []byte
	for len(b) > 76 {
		out = append(append(out, b[:76]...), '\r', '\n')
		b = b[76:]
	}
	return append(out, b...)
}

// BenchmarkCodecAlgo compares the base64 decoder candidates
// (docs/ALGORITHMS.md "Base64 decoding"): strict decoding of canonical
// input of each decoded size, and lenient decoding of MIME-wrapped input
// ("-mime"), where every 76 characters the fast paths hand a line break
// to the state machine.
func BenchmarkCodecAlgo(b *testing.B) {
	for _, a := range Base64Algos() {
		for _, n := range benchSizes {
			for _, mime := range []bool{false, true} {
				src := EncodeBase64(benchData(n), Base64Options{})
				o := Base64Options{Algo: a}
				name := a.String() + "/" + sizeName(n)
				if mime {
					src, o.Mode, name = mimeWrap(src), Lenient, name+"-mime"
				}
				b.Run(name, func(b *testing.B) {
					dst := make([]byte, 0, n+16)
					b.SetBytes(int64(n))
					b.ReportAllocs()
					for b.Loop() {
						if _, err := AppendDecodeBase64(dst, src, o); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}

// BenchmarkCodecEncode measures every encoder on 1 MiB of random bytes
// (MB/s of input).
func BenchmarkCodecEncode(b *testing.B) {
	x := benchData(1 << 20)
	text := []byte(strings.Repeat("The quick brown fox = jumps \"over\" the lazy dog;\r\n", 1<<20/50))
	for _, c := range []struct {
		name string
		in   []byte
		f    func(dst, src []byte) []byte
	}{
		{"base64", x, func(d, s []byte) []byte { return AppendEncodeBase64(d, s, Base64Options{}) }},
		{"base32", x, func(d, s []byte) []byte { return AppendEncodeBase32(d, s, Base32Options{}) }},
		{"hex", x, func(d, s []byte) []byte { return AppendEncodeHex(d, s, HexOptions{}) }},
		{"percent", text, func(d, s []byte) []byte { return AppendEncodePercent(d, s, SetComponent) }},
		{"qp", text, AppendEncodeQP},
		{"js", text, func(d, s []byte) []byte { return AppendEncodeJSString(d, s, '"') }},
	} {
		b.Run(c.name, func(b *testing.B) {
			dst := make([]byte, 0, 4*len(c.in))
			b.SetBytes(int64(len(c.in)))
			b.ReportAllocs()
			for b.Loop() {
				c.f(dst, c.in)
			}
		})
	}
}

// BenchmarkCodecDecode measures the other decoders on about 1 MiB of
// their own encoding (MB/s of encoded input), strict and lenient.
func BenchmarkCodecDecode(b *testing.B) {
	x := benchData(1 << 20)
	text := []byte(strings.Repeat("The quick brown fox = jumps \"over\" the lazy dog;\r\n", 1<<20/50))
	cases := []struct {
		name string
		in   []byte
		f    func(dst, src []byte, m Mode) ([]byte, error)
	}{
		{"base32", EncodeBase32(x, Base32Options{}), func(d, s []byte, m Mode) ([]byte, error) {
			return AppendDecodeBase32(d, s, Base32Options{Mode: m})
		}},
		{"hex", EncodeHex(x, HexOptions{}), func(d, s []byte, m Mode) ([]byte, error) { return AppendDecodeHex(d, s, HexOptions{Mode: m}) }},
		{"percent", EncodePercent(text, SetComponent), func(d, s []byte, m Mode) ([]byte, error) {
			return AppendDecodePercent(d, s, PercentOptions{Mode: m})
		}},
		{"qp", EncodeQP(text), AppendDecodeQP},
		{"js", EncodeJSString(text, '"'), func(d, s []byte, m Mode) ([]byte, error) {
			return AppendDecodeJSString(d, s, JSOptions{Mode: m, Quote: '"'})
		}},
	}
	for _, c := range cases {
		for _, m := range []Mode{Strict, Lenient} {
			b.Run(c.name+"/"+m.String(), func(b *testing.B) {
				dst := make([]byte, 0, len(c.in))
				b.SetBytes(int64(len(c.in)))
				b.ReportAllocs()
				for b.Loop() {
					if _, err := c.f(dst, c.in, m); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkCodecInflate measures decompression of 1 MiB of text-like data
// (MB/s of output), and how fast a 1 GiB zstd bomb is stopped.
func BenchmarkCodecInflate(b *testing.B) {
	var t bytes.Buffer
	r := core.NewSplitMix64(7)
	words := []string{"function", "return", "const", "fetch", "/api/v1/", "user", "admin", "{", "}", ";", "\n"}
	for t.Len() < 1<<20 {
		t.WriteString(words[core.IntN(r, len(words))])
		t.WriteByte(' ')
	}
	text := t.Bytes()
	for _, k := range compressionKinds {
		c := compress(b, k, text)
		b.Run(k.String(), func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Inflate(c, k, InflateOptions{MaxRatio: -1}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
	bomb := zstdRLEBomb(8192)
	b.Run("zstd-bomb", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := Inflate(bomb, KindZstd, InflateOptions{}); CodeOf(err) != CodeRatioLimit {
				b.Fatal(err)
			}
		}
	})
}

// benchDoc is a 1 MiB JS-like document with a base64-of-gzip blob, hex
// digests and base64 blobs every few KiB.
func benchDoc(b *testing.B) []byte {
	var d bytes.Buffer
	r := core.NewSplitMix64(3)
	for i := 0; d.Len() < 1<<20; i++ {
		d.WriteString("function handler" + strconv.Itoa(i) + "(event) { return fetch('/api/v1/items/' + event.id).then(r => r.json()); }\n")
		if i%40 == 0 {
			d.WriteString("const blob = \"" + string(EncodeBase64(compress(b, KindGzip, []byte(strings.Repeat("{\"path\":\"/internal/x\"}", 20))), Base64Options{})) + "\";\n")
			d.WriteString("const sri = \"sha256-" + string(EncodeBase64(randBytes(r, 32), Base64Options{})) + "\";\n")
			d.WriteString("const id = \"" + string(EncodeHex(randBytes(r, 20), HexOptions{})) + "\";\n")
		}
	}
	return d.Bytes()
}

// BenchmarkCodecSniff and BenchmarkCodecDecodeLayers scan benchDoc (MB/s
// of document).
func BenchmarkCodecSniff(b *testing.B) {
	doc := benchDoc(b)
	b.SetBytes(int64(len(doc)))
	b.ReportAllocs()
	for b.Loop() {
		Sniff(doc, SniffOptions{})
	}
}

func BenchmarkCodecDecodeLayers(b *testing.B) {
	doc := benchDoc(b)
	b.SetBytes(int64(len(doc)))
	b.ReportAllocs()
	for b.Loop() {
		DecodeLayers(doc, LayersOptions{})
	}
}
