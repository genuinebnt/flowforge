package htmlparse

import (
	"bytes"
	"testing"
	"unicode/utf8"
)

// Fuzz targets. The invariants are the assertions; seeds are every .dat
// input (and the tokenizer suite's inputs for FuzzHTMLTokenizer).

func addDatSeeds(f *testing.F, add func(data []byte, sel byte)) {
	for i, c := range loadDatSuite(f) {
		add([]byte(c.Data), byte(i))
	}
}

var fuzzContexts = []ContextElement{
	{Name: "body"}, {Name: "div"}, {Name: "td"}, {Name: "tr"}, {Name: "table"}, {Name: "template"},
	{Name: "select"}, {Name: "textarea"}, {Name: "title"}, {Name: "script"}, {Name: "plaintext"},
	{Name: "html"}, {Name: "frameset"}, {Namespace: SVGNamespace, Name: "svg"},
	{Namespace: SVGNamespace, Name: "foreignObject"}, {Namespace: MathMLNamespace, Name: "mi"},
	{Namespace: MathMLNamespace, Name: "annotation-xml", Attrs: []Attr{{Name: "encoding", Value: "text/html"}}},
}

// FuzzHTMLTokenizer: tokenization in any initial state is total, spans
// tile the input (HTML-INV-01) and errors are ordered and located
// (HTML-INV-08); character data is valid UTF-8.
func FuzzHTMLTokenizer(f *testing.F) {
	addDatSeeds(f, func(d []byte, s byte) { f.Add(d, s) })
	for _, file := range loadTokenizerSuite(f) {
		for i, tc := range file.Tests {
			if in, err := decodeJSONStringWTF8(tc.Input); err == nil {
				f.Add(in, byte(i))
			}
		}
	}
	f.Fuzz(func(t *testing.T, in []byte, sel byte) {
		st := TokenizerState(sel % 6)
		toks, errs := Tokenize(in, TokenizeOptions{InitialState: st, AllowCDATA: sel&0x80 != 0})
		if err := checkSpansTile(toks, len(in)); err != nil {
			t.Fatal(err)
		}
		if err := checkErrors(in, errs); err != nil {
			t.Fatal(err)
		}
		for _, tk := range toks {
			if !utf8.ValidString(tk.Data) || !utf8.ValidString(tk.Name) {
				t.Fatalf("token %s has invalid UTF-8", tk.Kind)
			}
		}
	})
}

// FuzzHTMLParse: parsing documents and fragments is total, the arena is
// consistent (HTML-INV-05), text is coalesced (HTML-INV-10), attributes
// are unique (HTML-INV-09), errors are ordered (HTML-INV-08), and
// round-trip-safe trees are serialize/reparse fixpoints (HTML-INV-03).
func FuzzHTMLParse(f *testing.F) {
	addDatSeeds(f, func(d []byte, s byte) { f.Add(d, s) })
	f.Fuzz(func(t *testing.T, in []byte, sel byte) {
		opts := Options{Scripting: sel&1 != 0, MaxDepth: 64 + int(sel>>1)}
		doc, errs := Parse(in, opts)
		for name, err := range map[string]error{
			"integrity": checkArenaIntegrity(doc),
			"coalesced": checkCoalesced(doc),
			"attrs":     checkAttrs(doc, DefaultMaxAttributes),
			"errors":    checkErrors(in, errs),
		} {
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if roundTripSafe(doc, errs) && !opts.Scripting {
			out := Render(doc, doc.Root)
			doc2, _ := Parse(out, opts)
			if Dump(doc) != Dump(doc2) {
				t.Fatalf("serialize/reparse changed the tree\nrendered %q\n#first\n%s\n#second\n%s", out, Dump(doc), Dump(doc2))
			}
		}
		ctx := fuzzContexts[int(sel)%len(fuzzContexts)]
		frag, ferrs := ParseFragment(in, ctx, opts)
		if err := checkArenaIntegrity(frag); err != nil {
			t.Fatalf("fragment (%s): %v", ctx.Name, err)
		}
		if err := checkErrors(in, ferrs); err != nil {
			t.Fatalf("fragment (%s): %v", ctx.Name, err)
		}
		_ = Render(frag, frag.Root)
	})
}

// FuzzHTMLDecode: decoding is total and yields valid UTF-8; sniffing is
// deterministic; DecodeAs with the sniffed encoding equals Decode; valid
// UTF-8 labelled utf-8 decodes to itself (HTML-INV-07).
func FuzzHTMLDecode(f *testing.F) {
	labels := []string{"", "utf-8", "text/html; charset=windows-1252", "iso-8859-2", "shift_jis", "utf-16le", "gbk", "bogus", "x-user-defined", "replacement"}
	addDatSeeds(f, func(d []byte, s byte) { f.Add(d, labels[int(s)%len(labels)]) })
	f.Add([]byte("<meta charset=koi8-r>\xc1\xc2"), "")
	f.Add([]byte("<?xml version='1.0' encoding='euc-jp'?>"), "")
	f.Add([]byte("\xff\xfe<\x00p\x00>\x00"), "")
	f.Add([]byte("<meta http-equiv=Content-Type content='text/html; charset=\"big5\"'>"), "")
	f.Fuzz(func(t *testing.T, raw []byte, label string) {
		out, enc, conf := Decode(raw, label)
		if !utf8.Valid(out) {
			t.Fatalf("invalid UTF-8 output for encoding %s", enc)
		}
		enc2, conf2 := Sniff(raw, label)
		if enc2 != enc || conf2 != conf {
			t.Fatalf("Sniff is not deterministic: %s/%s vs %s/%s", enc, conf, enc2, conf2)
		}
		if again := DecodeAs(raw, enc); !bytes.Equal(again, out) {
			t.Fatalf("DecodeAs(%s) differs from Decode", enc)
		}
		if utf8.Valid(raw) && !bytes.HasPrefix(raw, []byte{0xEF, 0xBB, 0xBF}) && !bytes.HasPrefix(raw, []byte{0xFE, 0xFF}) && !bytes.HasPrefix(raw, []byte{0xFF, 0xFE}) {
			if same, _, _ := Decode(raw, "utf-8"); !bytes.Equal(same, raw) {
				t.Fatal("valid UTF-8 labelled utf-8 changed")
			}
		}
		if _, enc, _ := ParseRaw(raw, label, Options{}); enc == "" {
			t.Fatal("ParseRaw returned no encoding")
		}
	})
}
