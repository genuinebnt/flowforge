package contentstore

import (
	"bytes"
	"testing"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/diff"
)

// FuzzCStoreSealRoundTrip: for any text, restoring the redaction seal
// gives the text back exactly (the mechanism behind CSTORE-INV-07's raw
// reads). Whether redaction finds every secret is content's
// CONTENT-INV-10; see OPEN_QUESTIONS.md for a content.Redact idempotence
// case found here (testdata/fuzz).
func FuzzCStoreSealRoundTrip(f *testing.F) {
	f.Add([]byte(`var k = "` + "AK" + "IA0123456789ABCDEF" + `"; postgres://u:pw_long_enough9@db/x`))
	f.Add([]byte("[REDACTED:jwt:Aa9_:3] plain text"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, text []byte) {
		red, spans := redact("body", text)
		back, err := restore(red, spans)
		if err != nil || !bytes.Equal(back, text) {
			t.Fatalf("restore: %v", err)
		}
	})
}

// FuzzCStoreHunkRoundTrip: for any two texts, every presented hunk of
// their diff is stored by reference (or verbatim) and expands back to the
// same lines.
func FuzzCStoreHunkRoundTrip(f *testing.F) {
	f.Add([]byte("a\nb\nc\n"), []byte("a\nx\nc\nd\n"))
	f.Add([]byte(""), []byte("only\nnew\n"))
	f.Add([]byte("same\n"), []byte("same"))
	f.Fuzz(func(t *testing.T, a, b []byte) {
		if len(a) > 4096 || len(b) > 4096 {
			return
		}
		old := &content.Artifact{DocID: "d", Kind: content.KindText, BodySHA256: sha256Hex(a), NormalizedSHA256: sha256Hex(a),
			Normalized: content.Normalized{Kind: content.KindText, Text: a}}
		cur := &content.Artifact{DocID: "d", Kind: content.KindText, BodySHA256: sha256Hex(b), NormalizedSHA256: sha256Hex(b),
			Normalized: content.Normalized{Kind: content.KindText, Text: b}}
		d, err := content.DiffArtifacts(old, cur, content.DiffOptions{Bounds: diff.Bounds{}})
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range d.Hunks {
			c := compactOf(h, textLines(a), textLines(b))
			got, ok := expandHunk(c, textLines(a), textLines(b))
			if !ok || len(got.Lines) != len(h.Lines) {
				t.Fatalf("hunk %q does not round-trip (%v)", h.Header, ok)
			}
			for i := range got.Lines {
				if got.Lines[i] != h.Lines[i] {
					t.Fatalf("line %d: %q vs %q", i, got.Lines[i], h.Lines[i])
				}
			}
		}
	})
}

// FuzzCStoreRecordDecode: stored-record decoders are total on arbitrary
// bytes (an error, never a panic).
func FuzzCStoreRecordDecode(f *testing.F) {
	f.Add([]byte(`{"v":1,"artifacts":[{"doc_id":"d","kind":"text","compact":[{"h":"@@ -1 +1 @@","o":"d1 i1"}]}]}`), []byte(`{"v":1,"change_set":{}}`))
	f.Add([]byte(`{"v":1,"processed":{"artifacts":[{}]}}`), []byte("FFZB"))
	f.Fuzz(func(t *testing.T, a, b []byte) {
		_, _, _ = expandRecords(a, b, nil, &content.Processed{Artifacts: []content.Artifact{{DocID: "d"}}}, content.DiffOptions{})
		_, _, _ = decodeFeatures(a, b)
		_, _ = decodeBlob(a, b)
		_, _ = decodeDiffBlob(a)
		_, _, _ = hunkStarts(string(a))
	})
}
