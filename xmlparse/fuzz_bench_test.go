package xmlparse

import (
	"bytes"
	"fmt"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/core"
)

// FuzzXMLParse asserts XML-INV-01 (totality, offsets, grades), XML-INV-02
// (first offending byte), XML-INV-04 (canonical fixed point) and
// XML-INV-05 (no expansion growth) on arbitrary input.
func FuzzXMLParse(f *testing.F) {
	for _, s := range []string{
		"<a/>", `<?xml version="1.0" encoding="UTF-8"?><r a='1'>t&amp;<![CDATA[x]]><!--c--><?p d?></r>`,
		`<!DOCTYPE r [<!ENTITY a "b">]><r/>`, `<p:a xmlns:p="urn:x" p:b="&#x41;"/>`, "<a>]]></a>",
		"\xef\xbb\xbf<a/>", "\xff\xfe<\x00a\x00/\x00>\x00", "<a b='1' b='2'/>", "<a></b>",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		doc, err := Parse(src, Options{})
		checkDoc(t, src, doc, err)
		if total, n, _ := eventBytes(src); total > n {
			t.Fatalf("%d decoded bytes from %d", total, n)
		}
		if err == nil {
			c := doc.Canonical(true)
			doc2, err2 := Parse(c, Options{})
			if err2 != nil {
				t.Fatalf("canonical %q does not parse: %v", c, err2)
			}
			if c2 := doc2.Canonical(true); !bytes.Equal(c, c2) {
				t.Fatalf("canonical not a fixed point: %q vs %q", c, c2)
			}
			return
		}
		e := err.(*SyntaxError)
		if e.Code.EOFClass() || doc.UTF16 {
			return
		}
		k := e.Offset
		if p := parseErr(src[:k]); p != nil && !p.Code.EOFClass() {
			t.Fatalf("error %v at %d but prefix fails with %v", e.Code, k, p)
		}
		w := 1
		if r, size := utf8.DecodeRune(src[k:]); r != utf8.RuneError || size > 1 {
			w = size
		}
		if k+w <= len(src) {
			if p := parseErr(src[:k+w]); p == nil || p.Code != e.Code || p.Offset != k {
				t.Fatalf("error %v at %d, prefix gives %v", e.Code, k, p)
			}
		}
	})
}

// genXML builds a sitemap-like document of about n bytes.
func genXML(n int) []byte {
	r := core.NewSplitMix64(0x584d4c) // "XML"
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n" + `<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9" xmlns:image="http://www.google.com/schemas/sitemap-image/1.1">` + "\n")
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "  <url>\n    <loc>https://example.com/p/%d?a=1&amp;b=%d</loc>\n    <lastmod>2024-0%d-1%dT10:00:00+00:00</lastmod>\n    <changefreq>weekly</changefreq>\n    <priority>0.%d</priority>\n    <image:image><image:loc>https://cdn.example.com/%d.jpg</image:loc></image:image>\n  </url>\n",
			i, core.IntN(r, 1000), core.IntN(r, 9)+1, core.IntN(r, 9), core.IntN(r, 10), i)
	}
	b.WriteString("</urlset>\n")
	return b.Bytes()
}

// BenchmarkXML01_Decode measures the streaming decoder at n and 4n.
func BenchmarkXML01_Decode(b *testing.B) {
	for _, n := range []int{256 << 10, 1 << 20} {
		src := genXML(n)
		b.Run(fmt.Sprintf("%dKiB", n>>10), func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				d := NewDecoder(src, Options{})
				for {
					if _, err := d.Next(); err != nil {
						break
					}
				}
			}
		})
	}
}

// BenchmarkXML02_Parse measures tree construction at n and 4n.
func BenchmarkXML02_Parse(b *testing.B) {
	for _, n := range []int{256 << 10, 1 << 20} {
		src := genXML(n)
		b.Run(fmt.Sprintf("%dKiB", n>>10), func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				if _, err := Parse(src, Options{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func perByte(src []byte, fn func([]byte)) float64 {
	best := time.Duration(1 << 62)
	for i := 0; i < 5; i++ {
		start := time.Now()
		fn(src)
		if d := time.Since(start); d < best {
			best = d
		}
	}
	return float64(best) / float64(len(src))
}

// TestXMLInv06_LinearTime is the timing half of XML-INV-06: per-byte cost
// at 4n stays within 1.5x of n on generated documents (2.5x on
// adversarial shapes).
func TestXMLInv06_LinearTime(t *testing.T) {
	if testing.Short() || raceEnabled {
		t.Skip("timing check skipped in -short and -race runs")
	}
	fn := func(src []byte) { Parse(src, Options{MaxDepth: 1 << 20}) }
	small, large := genXML(256<<10), genXML(1<<20)
	fn(small)
	if a, b := perByte(small, fn), perByte(large, fn); b > 1.5*a {
		t.Fatalf("per-byte cost grew from %.2f to %.2f ns", a, b)
	}
	for name, src := range adversarialXML(1 << 15) {
		big := adversarialXML(1 << 17)[name]
		if a, b := perByte(src, fn), perByte(big, fn); b > 2.5*a+1 {
			t.Errorf("%s: per-byte cost grew from %.2f to %.2f ns", name, a, b)
		}
	}
}
