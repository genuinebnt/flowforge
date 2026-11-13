package codec

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

// confFile is the schema of every conformance/codec/*.json file.
type confFile struct {
	Description string     `json:"description"`
	Source      string     `json:"source"`
	Cases       []confCase `json:"cases"`
}

type confCase struct {
	Name  string `json:"name"`
	Codec string `json:"codec"`
	// Op is "decode" (default), "encode" or "both" (encode decoded to
	// exactly encoded, and strictly decode encoded back).
	Op string `json:"op"`
	// Mode is "strict", "lenient" or "both" (default).
	Mode       string      `json:"mode"`
	Encoded    *string     `json:"encoded"`
	EncodedHex *string     `json:"encoded_hex"`
	Decoded    *string     `json:"decoded"`
	DecodedHex *string     `json:"decoded_hex"`
	Error      string      `json:"error"`
	Offset     *int        `json:"offset"`
	Pairs      [][2]string `json:"pairs"`
	Quote      string      `json:"quote"`
	// RejectInvalid sets PercentOptions.RejectInvalidEscapes.
	RejectInvalid bool `json:"reject_invalid"`
	MaxOutput     int  `json:"max_output"`
	MaxRatio      int  `json:"max_ratio"`
	RatioFloor    int  `json:"ratio_floor"`
	Candidates    []struct {
		Kind       string `json:"kind"`
		Start, End int
	} `json:"candidates"`
	MaxDepth  int         `json:"max_depth"`
	MaxTotal  int         `json:"max_total"`
	Truncated bool        `json:"truncated"`
	Layers    []confLayer `json:"layers"`
}

type confLayer struct {
	Kind      string  `json:"kind"`
	Parent    int     `json:"parent"`
	Start     int     `json:"start"`
	End       int     `json:"end"`
	Depth     int     `json:"depth"`
	Text      bool    `json:"text"`
	Truncated bool    `json:"truncated"`
	Data      *string `json:"data"`
	DataHex   *string `json:"data_hex"`
}

func textOrHex(t *testing.T, s, h *string) ([]byte, bool) {
	t.Helper()
	switch {
	case s != nil:
		return []byte(*s), true
	case h != nil:
		b, err := hex.DecodeString(*h)
		if err != nil {
			t.Fatalf("bad hex %q: %v", *h, err)
		}
		return b, true
	}
	return nil, false
}

func (c confCase) modes() []Mode {
	switch c.Mode {
	case "strict":
		return []Mode{Strict}
	case "lenient":
		return []Mode{Lenient}
	}
	return []Mode{Strict, Lenient}
}

// checkResult compares a decode result with the case's expectation.
func checkResult(t *testing.T, c confCase, what string, got []byte, err error, want []byte, hasWant bool) {
	t.Helper()
	if c.Error != "" {
		if err == nil {
			t.Fatalf("%s: got %q, want error %s", what, got, c.Error)
		}
		if CodeOf(err).String() != c.Error {
			t.Fatalf("%s: error %v, want %s", what, err, c.Error)
		}
		if c.Offset != nil && err.(*Error).Offset != *c.Offset {
			t.Fatalf("%s: error %v, want offset %d", what, err, *c.Offset)
		}
		if hasWant && !bytes.Equal(got, want) {
			t.Fatalf("%s: prefix %q, want %q", what, got, want)
		}
		return
	}
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s: got %q, want %q", what, got, want)
	}
}

// TestCodecConformance runs every case of conformance/codec against the
// codecs (base64 cases against every Base64Algo candidate).
func TestCodecConformance(t *testing.T) {
	n := 0
	for _, f := range testkit.LoadCases[confFile](t, "codec") {
		if len(f.Data.Cases) == 0 || f.Data.Source == "" {
			t.Fatalf("%s: no cases or no source", f.Name)
		}
		for _, c := range f.Data.Cases {
			n++
			t.Run(f.Name+"/"+c.Name, func(t *testing.T) { runConfCase(t, c) })
		}
	}
	if n < 150 {
		t.Fatalf("only %d conformance cases", n)
	}
}

func runConfCase(t *testing.T, c confCase) {
	enc, hasEnc := textOrHex(t, c.Encoded, c.EncodedHex)
	dec, hasDec := textOrHex(t, c.Decoded, c.DecodedHex)
	switch {
	case c.Codec == "form":
		runFormCase(t, c, enc)
		return
	case c.Codec == "sniff":
		runSniffCase(t, c, enc)
		return
	case c.Codec == "layers":
		runLayersCase(t, c, enc)
		return
	}
	if k, ok := ParseKind(c.Codec); ok && k.IsCompression() {
		got, err := Inflate(enc, k, InflateOptions{MaxOutput: c.MaxOutput, MaxRatio: c.MaxRatio, RatioFloor: c.RatioFloor})
		checkResult(t, c, "inflate", got, err, dec, hasDec)
		return
	}
	var codecs []textCodec
	switch {
	case strings.HasPrefix(c.Codec, "base64"):
		codecByName(t, c.Codec) // the variant must be registered
		alpha, noPad := Base64Std, strings.HasSuffix(c.Codec, "-nopad")
		if strings.HasPrefix(c.Codec, "base64url") {
			alpha = Base64URL
		}
		for _, a := range Base64Algos() {
			cc := base64Codec(c.Codec+"/"+a.String(), alpha, noPad, a)
			codecs = append(codecs, cc)
		}
	case c.Codec == "js":
		var q byte
		if c.Quote != "" {
			q = c.Quote[0]
		}
		codecs = []textCodec{{name: "js", encode: func(b []byte) []byte { return EncodeJSString(b, q) },
			decode: func(b []byte, m Mode) ([]byte, error) { return DecodeJSString(b, JSOptions{Mode: m, Quote: q}) }}}
	case strings.HasPrefix(c.Codec, "percent-") && c.RejectInvalid:
		set, err := ParsePercentSet(strings.TrimPrefix(c.Codec, "percent-"))
		if err != nil {
			t.Fatal(err)
		}
		codecs = []textCodec{{name: c.Codec, decode: func(b []byte, m Mode) ([]byte, error) {
			return DecodePercent(b, PercentOptions{Set: set, Mode: m, RejectInvalidEscapes: true})
		}}}
	default:
		codecs = []textCodec{codecByName(t, c.Codec)}
	}
	for _, cc := range codecs {
		if c.Op == "encode" || c.Op == "both" {
			if !hasEnc || !hasDec {
				t.Fatal("encode case needs encoded and decoded")
			}
			if got := cc.encode(dec); !bytes.Equal(got, enc) {
				t.Fatalf("%s encode(%q) = %q, want %q", cc.name, dec, got, enc)
			}
			if c.Op == "encode" {
				continue
			}
			got, err := cc.decode(enc, Strict)
			checkResult(t, c, cc.name+" strict", got, err, dec, true)
			continue
		}
		for _, m := range c.modes() {
			got, err := cc.decode(enc, m)
			checkResult(t, c, cc.name+" "+m.String(), got, err, dec, hasDec)
		}
	}
}

func runFormCase(t *testing.T, c confCase, enc []byte) {
	want := make([]FormPair, len(c.Pairs))
	for i, p := range c.Pairs {
		want[i] = FormPair{p[0], p[1]}
	}
	if c.Op == "both" || c.Op == "encode" {
		if got := EncodeForm(want); !bytes.Equal(got, enc) {
			t.Fatalf("EncodeForm = %q, want %q", got, enc)
		}
		if c.Op == "encode" {
			return
		}
		c.Mode = "both"
	}
	for _, m := range c.modes() {
		got, err := ParseForm(enc, m)
		if c.Error != "" {
			if CodeOf(err).String() != c.Error || c.Offset != nil && err.(*Error).Offset != *c.Offset {
				t.Fatalf("%s: got %v %v, want %s at %v", m, got, err, c.Error, *c.Offset)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", m, err)
		}
		if len(got) != len(want) {
			t.Fatalf("%s: got %q, want %q", m, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: got %q, want %q", m, got, want)
			}
		}
	}
}

func runSniffCase(t *testing.T, c confCase, in []byte) {
	got := Sniff(in, SniffOptions{})
	if len(got) != len(c.Candidates) {
		t.Fatalf("Sniff = %+v, want %+v", got, c.Candidates)
	}
	for i, w := range c.Candidates {
		if got[i].Kind.String() != w.Kind || got[i].Start != w.Start || got[i].End != w.End {
			t.Fatalf("candidate %d = %+v (%s), want %+v", i, got[i], got[i].Kind, w)
		}
	}
}

func runLayersCase(t *testing.T, c confCase, in []byte) {
	got := DecodeLayers(in, LayersOptions{MaxDepth: c.MaxDepth, MaxTotal: c.MaxTotal})
	if len(got.Nodes) != len(c.Layers) || got.Truncated != c.Truncated {
		t.Fatalf("DecodeLayers = %d nodes truncated=%v, want %d truncated=%v: %+v", len(got.Nodes), got.Truncated,
			len(c.Layers), c.Truncated, got.Nodes)
	}
	for i, w := range c.Layers {
		n := got.Nodes[i]
		data, _ := textOrHex(t, w.Data, w.DataHex)
		if n.Kind.String() != w.Kind || n.Parent != w.Parent || n.Start != w.Start || n.End != w.End ||
			n.Depth != w.Depth || n.Text != w.Text || n.Truncated != w.Truncated || !bytes.Equal(n.Data, data) {
			t.Fatalf("layer %d = %+v, want %+v (data %q)", i, n, w, data)
		}
	}
}
