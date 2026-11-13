package codec

import (
	"errors"
	"unicode/utf8"
)

// DecodeLayers defaults.
const (
	// DefaultMaxDepth is the default nesting limit: base64 → gzip → text
	// is depth 2, so 3 leaves one more layer (base64 of base64 of gzip).
	DefaultMaxDepth = 3
	// DefaultMaxTotal caps the bytes of all decoded layers together: 8
	// MiB, the content response bound.
	DefaultMaxTotal = 8 << 20
	// DefaultMaxNodes caps the layers kept.
	DefaultMaxNodes = 256
)

// LayersOptions bounds DecodeLayers. Zero fields take the defaults.
type LayersOptions struct {
	MaxDepth int
	MaxTotal int
	MaxNodes int
	// Sniff tunes candidate detection at every level.
	Sniff SniffOptions
	// Inflate bounds each decompression; its MaxOutput is further
	// clipped to what is left of MaxTotal.
	Inflate InflateOptions
}

// Layer is one decoded blob. Layers form an arena tree: Parent is the
// index of the enclosing layer in Layers.Nodes, or -1 when the blob was
// found in the input itself.
type Layer struct {
	Kind   Kind
	Parent int
	// Start and End are the encoded span inside the parent's Data (inside
	// the input when Parent is -1).
	Start, End int
	// Depth is 1 for blobs found in the input, parent's depth + 1 below.
	Depth int
	Data  []byte
	// Text reports LooksLikeText(Data) (for a truncated layer, ignoring
	// an incomplete final character).
	Text bool
	// Truncated: Data is a prefix, cut by a limit or by a corrupt stream.
	Truncated bool
}

// Layers is the result of DecodeLayers.
type Layers struct {
	// Nodes in depth-first pre-order (a layer precedes its children;
	// siblings in input order).
	Nodes []Layer
	// Total is the sum of len(Data) over Nodes; never above MaxTotal.
	Total int
	// Truncated reports that a limit (depth excluded) stopped decoding.
	Truncated bool
}

// TextNodes returns the indexes of the layers whose Data is text, in
// order: what a search index adds next to the original text.
func (l Layers) TextNodes() []int {
	var out []int
	for i, n := range l.Nodes {
		if n.Text {
			out = append(out, i)
		}
	}
	return out
}

// DecodeLayers decodes the blobs Sniff finds in b, then the blobs inside
// those, to MaxDepth levels (V2_DESIGN §12.14: gzip and base64 blobs
// found in content are decoded, bounded, before indexing). A decoded blob
// is kept only if it is text or compressed; binary garbage (a base64 run
// that was an image or an identifier) is dropped and not counted. Every
// decode is capped by what is left of MaxTotal, so Total never exceeds
// it, and the work is bounded by MaxDepth × len(b) plus the decompressed
// bytes (CODEC-INV-08).
func DecodeLayers(b []byte, o LayersOptions) Layers {
	if o.MaxDepth <= 0 {
		o.MaxDepth = DefaultMaxDepth
	}
	if o.MaxTotal <= 0 {
		o.MaxTotal = DefaultMaxTotal
	}
	if o.MaxNodes <= 0 {
		o.MaxNodes = DefaultMaxNodes
	}
	o.Sniff = o.Sniff.withDefaults()
	st := layersState{o: o}
	st.expand(-1, b, 0)
	return st.res
}

type layersState struct {
	o       LayersOptions
	res     Layers
	stopped bool
}

// expand decodes the candidates of data (the input or a layer's Data).
// Recursion depth is bounded by MaxDepth.
func (st *layersState) expand(parent int, data []byte, depth int) {
	if depth >= st.o.MaxDepth {
		return
	}
	for _, c := range sniffRaw(data, st.o.Sniff) {
		if st.stopped {
			return
		}
		left := st.o.MaxTotal - st.res.Total
		if left <= 0 || len(st.res.Nodes) >= st.o.MaxNodes {
			st.res.Truncated, st.stopped = true, true
			return
		}
		out, truncated, ok := st.decode(data[c.Start:c.End], c.Kind, left)
		if truncated && ok {
			// Reported even when the prefix is dropped below: a limit cut
			// the decoding short.
			st.res.Truncated = true
		}
		if !ok {
			continue
		}
		text := looksLikeTextPrefix(out, truncated)
		if !text {
			if _, compressed := DetectCompression(out); !compressed {
				continue
			}
		}
		st.res.Nodes = append(st.res.Nodes, Layer{Kind: c.Kind, Parent: parent, Start: c.Start, End: c.End,
			Depth: depth + 1, Data: out, Text: text, Truncated: truncated})
		st.res.Total += len(out)
		if st.res.Total >= st.o.MaxTotal {
			st.stopped = true
		}
		st.expand(len(st.res.Nodes)-1, out, depth+1)
	}
}

// decode decodes one candidate with at most left output bytes. ok is
// false when nothing usable came out.
func (st *layersState) decode(span []byte, k Kind, left int) (out []byte, truncated, ok bool) {
	var err error
	switch k {
	case KindHex:
		out, err = AppendDecodeHex(nil, span, HexOptions{Mode: Lenient})
	case KindBase64, KindBase64URL:
		out, err = AppendDecodeBase64(nil, span, Base64Options{Mode: Lenient})
	default:
		inf := st.o.Inflate
		if inf.MaxOutput <= 0 || inf.MaxOutput > left {
			if inf.MaxOutput <= 0 && left > DefaultMaxOutput {
				left = DefaultMaxOutput
			}
			inf.MaxOutput = left
		}
		out, err = Inflate(span, k, inf)
		if err != nil {
			// A stream cut by a limit or broken after some output still
			// yields its prefix.
			return out, true, len(out) > 0 && (errors.Is(err, ErrOutputLimit) || CodeOf(err) == CodeCorrupt)
		}
		return out, false, true
	}
	if err != nil {
		return nil, false, false
	}
	if len(out) > left {
		return out[:left], true, true
	}
	return out, false, true
}

// looksLikeTextPrefix is LooksLikeText, ignoring an incomplete UTF-8
// sequence at the end of a truncated prefix.
func looksLikeTextPrefix(b []byte, truncated bool) bool {
	if truncated {
		for k := 1; k <= 3 && k <= len(b); k++ {
			if utf8.RuneStart(b[len(b)-k]) {
				if !utf8.FullRune(b[len(b)-k:]) {
					b = b[:len(b)-k]
				}
				break
			}
		}
	}
	return LooksLikeText(b)
}
