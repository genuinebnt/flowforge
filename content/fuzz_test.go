package content

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
)

var fuzzSeeds = []string{
	"<!DOCTYPE html><p>a<b>c</p>", "<?php echo 1; ?>", `{"a":[1,{"b":null}]}`, "function f(){return /x/g}",
	"a{color:red}", "<?xml version=\"1.0\"?><urlset/>", "User-agent: *\nDisallow: /", "\x89PNG\r\n\x1a\n",
	"<svg><a><dd><input></svg>", "<table><a><tr>x", "DB_PASSWORD=x\nKEY=y\n", ")]}'\n{\"version\":3}",
	"<plaintext><b>", "<!DOCTYPE html bogus><table><p>", "var a = ;\n\\ \nb", "'use strict';x",
}

// FuzzContentSniff: Sniff is total, deterministic, and Mismatch agrees
// with the hint (CONTENT-INV-13).
func FuzzContentSniff(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add([]byte(s), "text/html")
	}
	f.Fuzz(func(t *testing.T, body []byte, ct string) {
		a := Sniff(body, ct, "https://f.example/x.js")
		b := Sniff(body, ct, "https://f.example/x.js")
		if a != b || a.Kind == "" || a.Reason == "" {
			t.Fatalf("not total/deterministic: %+v %+v", a, b)
		}
		if a.Mismatch != (a.Hint != "" && !compatibleKinds(a.Hint, a.Kind)) {
			t.Fatalf("mismatch inconsistent: %+v", a)
		}
	})
}

// FuzzContentNormalize: for every kind, Normalize never panics and is
// idempotent (CONTENT-INV-01); its output is redaction-stable.
func FuzzContentNormalize(f *testing.F) {
	for i, s := range fuzzSeeds {
		f.Add([]byte(s), uint8(i))
	}
	kinds := Kinds()
	f.Fuzz(func(t *testing.T, body []byte, k uint8) {
		kind := kinds[int(k)%len(kinds)]
		n1 := Normalize(kind, body, NormalizeOptions{})
		n2 := Normalize(kind, n1.Text, NormalizeOptions{})
		if !bytes.Equal(n1.Text, n2.Text) {
			t.Fatalf("%s not idempotent:\n%q\n%q\n%q", kind, body, n1.Text, n2.Text)
		}
		r1, _ := Redact(n1.Text)
		r2, _ := Redact(r1)
		if !bytes.Equal(r1, r2) {
			t.Fatalf("redaction not idempotent: %q", n1.Text)
		}
	})
}

// FuzzContentExtract: Extract never panics, reports a grade for every
// extractor of the kind, returns sorted features with known families,
// and never puts a secret's value in a feature.
func FuzzContentExtract(f *testing.F) {
	for i, s := range fuzzSeeds {
		f.Add([]byte(s), uint8(i))
	}
	f.Add([]byte(`<script>var k="AKIAIOSFODNN7EXAMPLE";fetch("/api/"+id)</script>`), uint8(0))
	kinds := Kinds()
	f.Fuzz(func(t *testing.T, body []byte, k uint8) {
		kind := kinds[int(k)%len(kinds)]
		fs := Extract(ExtractInput{URL: "https://f.example/a/b", Kind: kind, Body: body, Hasher: testHasher})
		want := map[string]bool{}
		for _, e := range ExtractorsFor(kind) {
			want[e.ID()] = true
		}
		got := map[string]bool{}
		for _, g := range fs.Grades {
			got[g.Extractor] = true
			if g.Grade == "" {
				t.Fatalf("empty grade %+v", g)
			}
		}
		for id := range want {
			if !got[id] {
				t.Fatalf("%s: extractor %s reported no grade", kind, id)
			}
		}
		if !slices.IsSortedFunc(fs.Features, func(a, b Feature) int {
			if a.Kind != b.Kind {
				return compareStr(string(a.Kind), string(b.Kind))
			}
			return compareStr(a.Key, b.Key)
		}) {
			t.Fatalf("features not sorted")
		}
		js, _ := json.Marshal(fs)
		for _, fd := range fs.Findings {
			if len(fd.Secret) >= 8 && bytes.Contains(js, fd.Secret) {
				t.Fatalf("secret value %q in extracted features", fd.Secret)
			}
		}
		for _, ft := range fs.Features {
			if ft.Family != FamilyOf(ft.Kind) {
				t.Fatalf("feature %s has family %s", ft.ID(), ft.Family)
			}
		}
	})
}

func compareStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
