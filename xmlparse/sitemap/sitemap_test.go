package sitemap

import (
	"bytes"
	"compress/gzip"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
	"github.com/genuinebnt/flowforge/xmlparse"
)

type smCase struct {
	Name       string `json:"name"`
	Input      string `json:"input"`
	InputHex   string `json:"input_hex"`
	RepeatURLs int    `json:"repeat_urls"`
	Pad        int    `json:"pad"`
	Gzip       bool   `json:"gzip"`
	Text       bool   `json:"text"`
	Options    struct {
		MaxEntries       int  `json:"max_entries"`
		MaxBytes         int  `json:"max_bytes"`
		RequireNamespace bool `json:"require_namespace"`
	} `json:"options"`
	Kind       string   `json:"kind"`
	Grade      string   `json:"grade"`
	Diags      []string `json:"diags"`
	Compressed bool     `json:"compressed"`
	EntryCount *int     `json:"entry_count"`
	Entries    []struct {
		Loc        string   `json:"loc"`
		LastMod    string   `json:"lastmod"`
		ChangeFreq string   `json:"changefreq"`
		Priority   *float64 `json:"priority"`
	} `json:"entries"`
}

type smFile struct {
	Description string   `json:"description"`
	Cases       []smCase `json:"cases"`
}

func gz(t testing.TB, b []byte) []byte {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func urlset(n, pad int, text bool) []byte {
	var b bytes.Buffer
	if !text {
		b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">`)
	}
	for i := 0; i < n; i++ {
		if text {
			fmt.Fprintf(&b, "https://e.example/%d\n", i)
		} else {
			fmt.Fprintf(&b, "<url><loc>https://e.example/%d</loc></url>", i)
		}
	}
	if !text {
		b.WriteString(strings.Repeat(" ", pad))
		b.WriteString("</urlset>")
	}
	return b.Bytes()
}

func (c *smCase) input(t *testing.T) []byte {
	var src []byte
	switch {
	case c.InputHex != "":
		b, err := hex.DecodeString(c.InputHex)
		if err != nil {
			t.Fatal(err)
		}
		src = b
	case c.RepeatURLs > 0:
		src = urlset(c.RepeatURLs, c.Pad, c.Text)
	default:
		src = []byte(c.Input)
	}
	if c.Gzip {
		src = gz(t, src)
	}
	return src
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// TestSitemapConformance runs conformance/xmlparse/sitemap.
func TestSitemapConformance(t *testing.T) {
	n := 0
	for _, f := range testkit.LoadCases[smFile](t, "xmlparse/sitemap") {
		for _, c := range f.Data.Cases {
			n++
			t.Run(f.Name+"/"+c.Name, func(t *testing.T) {
				opts := Options{MaxEntries: c.Options.MaxEntries, MaxBytes: c.Options.MaxBytes, RequireNamespace: c.Options.RequireNamespace}
				var s *Sitemap
				if c.Text {
					s = ParseText(c.input(t), opts)
				} else {
					s = Parse(c.input(t), opts)
				}
				if s.Kind.String() != c.Kind {
					t.Errorf("kind %s, want %s", s.Kind, c.Kind)
				}
				if c.Grade != "" && s.Grade.String() != c.Grade {
					t.Errorf("grade %s, want %s (diags %v, xml %v)", s.Grade, c.Grade, s.Diags, s.XMLErr)
				}
				if s.Compressed != c.Compressed {
					t.Errorf("compressed %v, want %v", s.Compressed, c.Compressed)
				}
				if c.Diags != nil {
					var got []string
					for _, d := range s.Diags {
						got = append(got, d.Code.String())
					}
					if strings.Join(got, ",") != strings.Join(c.Diags, ",") {
						t.Errorf("diags %v, want %v", got, c.Diags)
					}
				}
				if c.EntryCount != nil && len(s.Entries) != *c.EntryCount {
					t.Errorf("%d entries, want %d", len(s.Entries), *c.EntryCount)
				}
				if c.Entries != nil {
					if len(s.Entries) != len(c.Entries) {
						t.Fatalf("%d entries %+v, want %d", len(s.Entries), s.Entries, len(c.Entries))
					}
					for i, w := range c.Entries {
						g := s.Entries[i]
						if g.Loc != w.Loc || fmtTime(g.LastMod) != w.LastMod || g.ChangeFreq.String() != w.ChangeFreq {
							t.Errorf("entry %d: %q %q %q, want %q %q %q", i, g.Loc, fmtTime(g.LastMod), g.ChangeFreq, w.Loc, w.LastMod, w.ChangeFreq)
						}
						if (w.Priority != nil) != g.HasPriority || (w.Priority != nil && *w.Priority != g.Priority) {
							t.Errorf("entry %d: priority %v/%v, want %v", i, g.Priority, g.HasPriority, w.Priority)
						}
					}
				}
			})
		}
	}
	if n < 20 {
		t.Fatalf("only %d cases", n)
	}
}

// genSitemap builds a random sitemap-ish document with valid and invalid
// entries; it returns the document and the locs that must be read.
func genSitemap(r core.Rng) ([]byte, []string) {
	var b bytes.Buffer
	var want []string
	root, entry := "urlset", "url"
	if core.IntN(r, 3) == 0 {
		root, entry = "sitemapindex", "sitemap"
	}
	b.WriteString(`<?xml version="1.0"?><` + root + ` xmlns="` + Namespace + `" xmlns:x="urn:x">`)
	for i := core.IntN(r, 12); i > 0; i-- {
		switch core.IntN(r, 5) {
		case 0:
			b.WriteString("<x:junk><" + entry + "><loc>https://no.example/</loc></" + entry + "></x:junk>")
		case 1:
			fmt.Fprintf(&b, "<%s><loc>not-a-url-%d</loc></%s>", entry, i, entry)
		default:
			loc := fmt.Sprintf("https://e.example/%d?q=%d", core.IntN(r, 1000), i)
			want = append(want, loc)
			fmt.Fprintf(&b, "<%s>\n <x:ext a='1'><loc>https://ext.example/</loc></x:ext><loc>%s</loc><lastmod>2020-0%d-01</lastmod></%s>",
				entry, strings.ReplaceAll(loc, "&", "&amp;"), core.IntN(r, 9)+1, entry)
		}
	}
	b.WriteString("</" + root + ">")
	return b.Bytes(), want
}

// TestSitemapInv01_ExactEntries: generated sitemaps (valid entries mixed
// with invalid ones and extension elements holding look-alike <loc>s)
// yield exactly the valid entries in order, compressed or not.
func TestSitemapInv01_ExactEntries(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		src, want := genSitemap(r)
		if i%2 == 1 {
			src = gz(t, src)
		}
		s := Parse(src, Options{})
		if s.Grade != xmlparse.GradeComplete {
			t.Fatalf("grade %s: %v %v", s.Grade, s.Diags, s.XMLErr)
		}
		if len(s.Entries) != len(want) {
			t.Fatalf("%d entries, want %d", len(s.Entries), len(want))
		}
		for j := range want {
			if s.Entries[j].Loc != want[j] {
				t.Fatalf("entry %d %q, want %q", j, s.Entries[j].Loc, want[j])
			}
		}
	}
}

// TestSitemapInv02_LimitsGradePartial: an entry or byte limit hit grades
// the read partial, never complete; the entry count never exceeds the
// limit, and decompression never inflates more than MaxBytes+1 bytes.
func TestSitemapInv02_LimitsGradePartial(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases()/4; i++ {
		n := core.IntN(r, 40)
		maxE := core.IntN(r, 40) + 1
		src := urlset(n, core.IntN(r, 4000), false)
		maxB := core.IntN(r, len(src)+200) + 1
		in := src
		if i%2 == 0 {
			in = gz(t, src)
		}
		s := Parse(in, Options{MaxEntries: maxE, MaxBytes: maxB})
		limit := false
		for _, d := range s.Diags {
			limit = limit || d.Code == CodeLimitExceeded || d.Code == CodeInputTooLarge
		}
		if limit != (s.Grade == xmlparse.GradePartial) {
			t.Fatalf("n=%d maxE=%d maxB=%d: limit %v grade %s %v", n, maxE, maxB, limit, s.Grade, s.Diags)
		}
		if len(s.Entries) > maxE {
			t.Fatalf("%d entries over limit %d", len(s.Entries), maxE)
		}
		if (n > maxE || len(src) > maxB) && s.Grade == xmlparse.GradeComplete {
			t.Fatalf("over a limit but complete")
		}
		if len(src) <= maxB && n <= maxE && s.Grade != xmlparse.GradeComplete {
			t.Fatalf("within limits but %s: %v %v", s.Grade, s.Diags, s.XMLErr)
		}
	}
	// A 1 GiB-of-zeros-style bomb inflates only to the limit.
	bomb := gz(t, append(urlset(1, 0, false)[:61], bytes.Repeat([]byte(" "), 32<<20)...))
	got, ok := decompress(bomb, 1<<16, &Sitemap{})
	if !ok || len(got) != 1<<16+1 {
		t.Fatalf("decompressed %d bytes, want %d", len(got), 1<<16+1)
	}
}

// FuzzSitemap asserts totality, the limit/grade rule and that every
// returned entry has a valid absolute http(s) loc.
func FuzzSitemap(f *testing.F) {
	f.Add(urlset(3, 0, false))
	f.Add([]byte(`<sitemapindex xmlns="` + Namespace + `"><sitemap><loc>https://a/</loc><lastmod>2020</lastmod></sitemap></sitemapindex>`))
	f.Add([]byte("https://a.example/\nhttps://b.example/"))
	f.Fuzz(func(t *testing.T, src []byte) {
		for _, s := range []*Sitemap{Parse(src, Options{MaxEntries: 5, MaxBytes: 4096}), ParseText(src, Options{MaxEntries: 5, MaxBytes: 4096})} {
			if len(s.Entries) > 5 {
				t.Fatalf("over the entry limit")
			}
			limit := false
			for _, d := range s.Diags {
				limit = limit || d.Code == CodeLimitExceeded || d.Code == CodeInputTooLarge
			}
			if limit && s.Grade != xmlparse.GradePartial {
				t.Fatalf("limit hit but grade %s", s.Grade)
			}
			for _, e := range s.Entries {
				if !validLoc(e.Loc) {
					t.Fatalf("invalid loc %q returned", e.Loc)
				}
			}
		}
	})
}

// BenchmarkSitemap01_Parse measures urlset reading at n and 4n entries.
func BenchmarkSitemap01_Parse(b *testing.B) {
	for _, n := range []int{5000, 20000} {
		src := urlset(n, 0, false)
		b.Run(fmt.Sprintf("%dentries", n), func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				Parse(src, Options{})
			}
		})
	}
}
