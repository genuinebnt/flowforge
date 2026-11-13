package content

import (
	"strings"

	"github.com/genuinebnt/flowforge/codec"
)

// DecodedBlob is an encoded blob found in a document's text and decoded
// for the search index (V2 §12.14: gzip and base64 blobs found in content
// are decoded, bounded, before indexing).
type DecodedBlob struct {
	// Path names the encodings from the outside in, joined by '>', for
	// example "base64>gzip".
	Path string `json:"path"`
	// Start and End are the span of the outermost encoded blob in the
	// scanned text.
	Start int `json:"start"`
	End   int `json:"end"`
	// Text is the decoded text, redacted (V2 §12.11): what may be indexed.
	Text string `json:"text"`
	// Truncated: a bound cut the decoding short.
	Truncated bool `json:"truncated,omitempty"`
}

// DecodeBlobs finds base64 (standard or URL alphabet), hex, gzip, zlib and
// zstd blobs in text and returns the decoded text inside them
// (codec.DecodeLayers at its default depth of three levels), redacted. Spec
// 33.12 names only base64, base64-then-gzip and hex; the other codec kinds
// are decoded too. It is bounded by the response limits: at most
// MaxResponseBytes decoded bytes and MaxInnerDocuments layers. A text
// layer that is nothing but the encoding of a deeper layer (base64 of
// base64) is skipped in favour of that layer. A blob that overlaps a
// secret (a JWT's segments, a PEM body) is not returned, nor anything
// inside it. Binary payloads are never returned. The order is the
// document order, outer blobs first.
//
// Callers pass normalized or raw text; secrets inside blobs are only
// redacted here, not reported as findings (OPEN_QUESTIONS.md 35).
func DecodeBlobs(text []byte, lim Limits) []DecodedBlob {
	lim = lim.withDefaults()
	res := codec.DecodeLayers(text, codec.LayersOptions{MaxTotal: lim.MaxResponseBytes, MaxNodes: lim.MaxInnerDocuments})
	wrapper := make([]bool, len(res.Nodes))
	// A blob inside a secret (the parts of a JWT, a PEM body, a key that
	// happens to be base64) is never decoded: its plaintext would carry
	// the secret's content past redaction. Nodes are in pre-order, so a
	// parent's verdict is known before its children.
	hidden := make([]bool, len(res.Nodes))
	spans := map[int][][2]int{} // parent index (-1: the input) -> secret spans
	secretSpans := func(parent int) [][2]int {
		if sp, ok := spans[parent]; ok {
			return sp
		}
		data := text
		if parent >= 0 {
			data = res.Nodes[parent].Data
		}
		var sp [][2]int
		for _, f := range ScanSecrets(data, "") {
			sp = append(sp, [2]int{f.Start, f.End})
		}
		spans[parent] = sp
		return sp
	}
	for i, n := range res.Nodes {
		if n.Parent >= 0 && hidden[n.Parent] {
			hidden[i] = true
			continue
		}
		for _, sp := range secretSpans(n.Parent) {
			if n.Start < sp[1] && sp[0] < n.End {
				hidden[i] = true
				break
			}
		}
		// A child spanning all of its parent's data makes the parent a mere
		// wrapper (base64 of base64): only the inner text is returned. This
		// runs for hidden children too, so a wrapper around a secret is
		// dropped with it.
		if n.Parent >= 0 && n.Start == 0 && n.End == len(res.Nodes[n.Parent].Data) {
			wrapper[n.Parent] = true
		}
	}
	var out []DecodedBlob
	for i, n := range res.Nodes {
		if !n.Text || wrapper[i] || hidden[i] {
			continue
		}
		var kinds []string
		root, truncated := i, false
		for j := i; j >= 0; j = res.Nodes[j].Parent {
			kinds = append(kinds, res.Nodes[j].Kind.String())
			root, truncated = j, truncated || res.Nodes[j].Truncated
		}
		for a, b := 0, len(kinds)-1; a < b; a, b = a+1, b-1 {
			kinds[a], kinds[b] = kinds[b], kinds[a]
		}
		red, _ := Redact(n.Data)
		out = append(out, DecodedBlob{Path: strings.Join(kinds, ">"), Start: res.Nodes[root].Start,
			End: res.Nodes[root].End, Text: string(red), Truncated: truncated})
	}
	return out
}
