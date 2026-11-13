package content

import (
	"bytes"
	"compress/gzip"
	"reflect"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/codec"
	"github.com/genuinebnt/flowforge/testkit"
)

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	w.Write(b)
	w.Close()
	return buf.Bytes()
}

// CONTENT-INV-17: DecodeBlobs finds the text inside base64, base64>gzip
// and hex blobs of a bundle, with the outer blob's span and the path of
// encodings; every planted secret of every rule is redacted from the
// decoded text, and blobs inside secrets (JWT segments) are not decoded; the decoded bytes stay within MaxResponseBytes; binary
// payloads and plain text give nothing; the result is deterministic.
func TestContentInv17_DecodedBlobsRedacted(t *testing.T) {
	r := testkit.Rng(t)
	rules := SecretRuleIDs()
	for i := range max(60, testkit.Cases()/10) {
		rule := rules[i%len(rules)][0]
		plant, secret := genSecret(r, rule)
		inner := `{"endpoint":"/internal/admin/export","config":` + strings.Repeat(" ", i%3) + `"` + plant + `"}`
		var blob []byte
		var path string
		switch i % 3 {
		case 0:
			blob, path = codec.EncodeBase64(gzipBytes(t, []byte(inner)), codec.Base64Options{}), "base64>gzip"
		case 1:
			blob, path = codec.EncodeBase64([]byte(inner), codec.Base64Options{Alphabet: codec.Base64URL}), "base64"
			if bytes.ContainsAny(blob, "-_") {
				path = "base64url"
			}
		default:
			blob, path = codec.EncodeHex([]byte(inner), codec.HexOptions{}), "hex"
		}
		prefix := `var cfg = atob("`
		doc := []byte(prefix + string(blob) + `"); fetch("/api/x");`)
		got := DecodeBlobs(doc, Limits{})
		if len(got) != 1 {
			t.Fatalf("%s %s: %d blobs: %+v", rule, path, len(got), got)
		}
		b := got[0]
		if b.Path != path || b.Start != len(prefix) || b.End != len(prefix)+len(blob) || b.Truncated {
			t.Fatalf("%s: blob %+v, want path %s span [%d,%d)", rule, b, path, len(prefix), len(prefix)+len(blob))
		}
		if !strings.Contains(b.Text, "/internal/admin/export") || strings.Contains(b.Text, secret) ||
			!strings.Contains(b.Text, "[REDACTED:") {
			t.Fatalf("%s %s: decoded text not redacted or missing: %q", rule, path, b.Text)
		}
		if again := DecodeBlobs(doc, Limits{}); !reflect.DeepEqual(again, got) {
			t.Fatal("DecodeBlobs not deterministic")
		}
		// A 40-byte bound cuts every layer: whatever text is returned is
		// marked truncated, and no secret survives the cut either.
		for _, s := range DecodeBlobs(doc, Limits{MaxResponseBytes: 40}) {
			if !s.Truncated || strings.Contains(s.Text, secret) {
				t.Fatalf("%s: blob within 40 bytes: %+v", rule, s)
			}
		}
	}
	jwt, _ := genSecret(r, "jwt")
	if got := DecodeBlobs([]byte(`headers.Authorization = "Bearer `+jwt+`";`), Limits{}); len(got) != 0 {
		t.Fatalf("JWT segments decoded: %+v", got)
	}
	sri := codec.EncodeBase64(bytes.Repeat([]byte{0xC3, 0x28, 0x00, 0xFF}, 12), codec.Base64Options{})
	for _, doc := range []string{
		`<script integrity="sha384-` + string(sri) + `" src="/a.js"></script>`,
		"function addEventListenerForTheComponent() { return getElementsByTagNameFromDocument }",
		"",
	} {
		if got := DecodeBlobs([]byte(doc), Limits{}); len(got) != 0 {
			t.Fatalf("%q: %+v", doc, got)
		}
	}
	// A wrapper layer (base64 of base64 of text) reports only the text.
	dbl := codec.EncodeBase64(codec.EncodeBase64([]byte("fetch('/api/v2/private/export?format=csv')"), codec.Base64Options{}), codec.Base64Options{})
	got := DecodeBlobs(append([]byte("x="), dbl...), Limits{})
	if len(got) != 1 || got[0].Path != "base64>base64" || got[0].Text != "fetch('/api/v2/private/export?format=csv')" {
		t.Fatalf("double base64: %+v", got)
	}
}
