package bytecode_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	bc "github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

func randModule(r core.Rng) *bc.Module {
	n := func(k int) int { return core.IntN(r, k) }
	m := &bc.Module{FormatVersion: bc.FormatVersion, Effects: bc.EffectMask(r.Uint64()), Entry: uint16(n(4))}
	for i := n(8); i > 0; i-- {
		switch n(5) {
		case 0:
			m.Consts = append(m.Consts, bc.IntConst(int64(r.Uint64())))
		case 1:
			m.Consts = append(m.Consts, bc.FloatConst(core.Float64(r)*1e6-5e5))
		case 2:
			m.Consts = append(m.Consts, bc.DurationConst(int64(n(1e9))))
		case 3:
			m.Consts = append(m.Consts, bc.TimeConst(int64(r.Uint64()>>1)))
		default:
			b := make([]byte, n(12))
			for j := range b {
				b[j] = byte('a' + n(26))
			}
			m.Consts = append(m.Consts, bc.StringConst(string(b)))
		}
	}
	for i := n(4); i > 0; i-- {
		td := bc.TypeDef{Kind: bc.TypeKind(n(3)), Name: uint32(n(10))}
		switch td.Kind {
		case bc.KindEnum:
			for j := n(4); j > 0; j-- {
				v := bc.VariantDef{Name: uint32(n(10))}
				for k := n(3); k > 0; k-- {
					v.Payload = append(v.Payload, uint32(n(10)))
				}
				td.Variants = append(td.Variants, v)
			}
		default:
			for j := n(4); j > 0; j-- {
				td.Fields = append(td.Fields, bc.FieldDef{Name: uint32(n(10)), Type: uint32(n(10))})
			}
			if td.Kind == bc.KindSignature {
				td.Func, td.Result = uint16(n(4)), bc.NoType
			}
		}
		m.Types = append(m.Types, td)
	}
	for i := 1 + n(3); i > 0; i-- {
		f := bc.Func{Name: uint32(n(10)), Arity: uint8(n(4)), NLocals: uint16(n(20)), NCaps: uint8(n(3))}
		for j := 1 + n(40); j > 0; j-- {
			f.Code = append(f.Code, byte(r.Uint64()))
		}
		off := 0
		for off < len(f.Code) && n(3) > 0 {
			f.Spans = append(f.Spans, bc.Span{CodeOffset: uint32(off), SourceOffset: uint32(n(1000)), Len: uint32(n(50))})
			off += 1 + n(5)
		}
		m.Funcs = append(m.Funcs, f)
	}
	for i := n(3); i > 0; i-- {
		var t bc.Tool
		t.Name = uint32(n(10))
		for j := range t.SpecSHA256 {
			t.SpecSHA256[j] = byte(r.Uint64())
		}
		m.Tools = append(m.Tools, t)
	}
	m.Meta = bc.Meta{CompilerVersion: "ffc", WorkflowName: "w"}
	return m
}

func TestBCProp_EncodeDecodeIdentity(t *testing.T) {
	r := testkit.Rng(t)
	for i := 0; i < testkit.Cases(); i++ {
		m := randModule(r)
		b, err := m.Encode()
		if err != nil {
			t.Fatal(err)
		}
		got, err := bc.Decode(b)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		if !reflect.DeepEqual(got, m) {
			t.Fatalf("case %d: decode(encode(m)) != m\n got %+v\nwant %+v", i, got, m)
		}
		again, _ := got.Encode()
		if !bytes.Equal(again, b) {
			t.Fatalf("case %d: encode(decode(b)) != b", i)
		}
		// Verify must never panic, whatever the module.
		_, _ = bc.Verify(m, tools())
	}
}

func FuzzBytecodeDecode(f *testing.F) {
	sample, err := sampleModule(f).Encode()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(sample)
	f.Add([]byte("FFBC\x01\x00\x00\x00"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := bc.Decode(b)
		if err != nil {
			return
		}
		again, err := m.Encode()
		if err != nil {
			t.Fatalf("decoded module does not encode: %v", err)
		}
		if !bytes.Equal(again, b) {
			t.Fatal("encode(decode(b)) != b")
		}
		_, _ = bc.Verify(m, tools())
		for i := range m.Funcs {
			_ = bc.Disassemble(m, i)
		}
	})
}

// verifyConformance is one file of conformance/bytecode/verify.
type verifyConformance struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	ModuleHex   string `json:"module_hex"`
	Tools       []struct {
		Name       string   `json:"name"`
		SpecSHA256 string   `json:"spec_sha256"`
		Effects    []string `json:"effects"`
	} `json:"tools"`
	// Expect is "accept", a bytecode.RuleID, or "decode_error" for modules
	// that Decode already rejects.
	Expect string `json:"expect"`
}

// TestBCConformance_Verify runs conformance/bytecode/verify. Set
// FF_UPDATE_CONFORMANCE=1 to regenerate the files from the bc_002 cases.
func TestBCConformance_Verify(t *testing.T) {
	if os.Getenv("FF_UPDATE_CONFORMANCE") == "1" {
		writeVerifyConformance(t)
	}
	for _, cf := range testkit.LoadCases[verifyConformance](t, "bytecode/verify") {
		c := cf.Data
		t.Run(c.Name, func(t *testing.T) {
			raw, err := hex.DecodeString(c.ModuleHex)
			if err != nil {
				t.Fatal(err)
			}
			reg := bc.ToolMap{}
			for _, tl := range c.Tools {
				var info bc.ToolInfo
				h, err := hex.DecodeString(tl.SpecSHA256)
				if err != nil || len(h) != 32 {
					t.Fatalf("bad spec hash %q", tl.SpecSHA256)
				}
				copy(info.SpecSHA256[:], h)
				for _, e := range tl.Effects {
					bit, ok := bc.ParseEffect(e)
					if !ok {
						t.Fatalf("unknown effect %q", e)
					}
					info.Effects |= bit
				}
				reg[tl.Name] = info
			}
			m, err := bc.Decode(raw)
			if err == nil {
				_, err = bc.Verify(m, reg)
			}
			got := "accept"
			if err != nil {
				got = bc.RuleID(err)
				if got == "" {
					got = "decode_error"
				}
			}
			if got != c.Expect {
				t.Fatalf("got %s, want %s (%v)", got, c.Expect, err)
			}
		})
	}
}

func writeVerifyConformance(t *testing.T) {
	dir := testkit.ConformanceDir(t, "bytecode/verify")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, c := range verifyCases() {
		m := c.Build(t)
		b, err := m.Encode()
		if err != nil {
			t.Fatal(err)
		}
		expect := "accept"
		if c.Rule != nil {
			expect = bc.RuleID(c.Rule)
		}
		if _, err := bc.Decode(b); err != nil {
			expect = "decode_error" // Decode already rejects this module
		}
		if _, err := bc.Verify(m, tools()); (err == nil) != (c.Rule == nil) || (err != nil && !errors.Is(err, c.Rule)) {
			t.Fatalf("%s: case does not hold: %v", c.Name, err)
		}
		doc := map[string]any{
			"name":        c.Name,
			"description": "bc_002 verifier case; expect is accept or a bytecode.RuleID",
			"module_hex":  hex.EncodeToString(b),
			"tools":       []any{map[string]any{"name": "httpx", "spec_sha256": hex.EncodeToString(toolHash[:]), "effects": []string{"active"}}},
			"expect":      expect,
		}
		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, c.Name+".json"), append(out, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
