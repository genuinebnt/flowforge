package xmlparse

import (
	"bytes"
	"encoding/hex"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

type xmlCase struct {
	Name       string `json:"name"`
	Input      string `json:"input"`
	InputHex   string `json:"input_hex"`
	RepeatOpen int    `json:"repeat_open"`
	Options    struct {
		MaxDepth      int `json:"max_depth"`
		MaxInputBytes int `json:"max_input_bytes"`
	} `json:"options"`
	Error     string  `json:"error"`
	Offset    *int    `json:"offset"`
	Grade     string  `json:"grade"`
	Canonical *string `json:"canonical"`
}

type xmlFile struct {
	Description string    `json:"description"`
	Cases       []xmlCase `json:"cases"`
}

func (c *xmlCase) input(t *testing.T) []byte {
	switch {
	case c.InputHex != "":
		b, err := hex.DecodeString(c.InputHex)
		if err != nil {
			t.Fatal(err)
		}
		return b
	case c.RepeatOpen > 0:
		return append(bytes.Repeat([]byte("<a>"), c.RepeatOpen), bytes.Repeat([]byte("</a>"), c.RepeatOpen)...)
	}
	return []byte(c.Input)
}

// TestXMLConformance runs conformance/xmlparse: well-formed documents with
// their canonical form, well-formedness errors with code and first
// offending byte, refused features, and limits with grades.
func TestXMLConformance(t *testing.T) {
	n := 0
	for _, f := range testkit.LoadCases[xmlFile](t, "xmlparse") {
		for _, c := range f.Data.Cases {
			n++
			t.Run(f.Name+"/"+c.Name, func(t *testing.T) {
				src := c.input(t)
				doc, err := Parse(src, Options{MaxDepth: c.Options.MaxDepth, MaxInputBytes: c.Options.MaxInputBytes})
				gotCode := ""
				if err != nil {
					gotCode = err.(*SyntaxError).Code.String()
				}
				if gotCode != c.Error {
					t.Fatalf("error %v, want %q", err, c.Error)
				}
				if c.Offset != nil && err.(*SyntaxError).Offset != *c.Offset {
					t.Errorf("offset %d, want %d", err.(*SyntaxError).Offset, *c.Offset)
				}
				if c.Grade != "" && doc.Grade.String() != c.Grade {
					t.Errorf("grade %s, want %s", doc.Grade, c.Grade)
				}
				if doc.Grade != GradeOf(err) {
					t.Errorf("document grade %s disagrees with GradeOf %s", doc.Grade, GradeOf(err))
				}
				if c.Canonical != nil {
					if got := string(doc.Canonical(true)); got != *c.Canonical {
						t.Errorf("canonical\n got  %q\n want %q", got, *c.Canonical)
					}
				}
			})
		}
	}
	if n < 80 {
		t.Fatalf("only %d cases", n)
	}
}
