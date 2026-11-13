package jsparse

import (
	"fmt"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

type extractCases struct {
	Cases []struct {
		Name    string `json:"name"`
		Module  bool   `json:"module"`
		Source  string `json:"source"`
		Strings []struct {
			Value    string `json:"value"`
			Kind     string `json:"kind"`
			Callee   string `json:"callee"`
			Receiver string `json:"receiver"`
			Arg      int    `json:"arg"`
			Key      string `json:"key"`
		} `json:"strings"`
		Modules []struct {
			Specifier string `json:"specifier"`
			Dynamic   bool   `json:"dynamic"`
			Literal   bool   `json:"literal"`
		} `json:"modules"`
		SourceMappingURL *string `json:"sourceMappingURL"`
		Tables           []struct {
			Context string `json:"context"`
			Entries []struct {
				Path  string `json:"path"`
				Kind  string `json:"kind"`
				Value string `json:"value"`
			} `json:"entries"`
		} `json:"tables"`
	} `json:"cases"`
}

var stringKindNames = map[StringKind]string{StringLiteral: "string", TemplateLiteral: "template", TaggedTemplate: "tagged"}

var valueKindNames = map[ValueKind]string{ValueOther: "other", ValueString: "string", ValueTemplate: "template",
	ValueNumber: "number", ValueBool: "bool", ValueNull: "null", ValueIdentifier: "identifier",
	ValueFunction: "function", ValueCall: "call", ValueRegExp: "regexp"}

// TestJSExtract runs conformance/jsparse/extract: string literals with
// call context, module references, the sourceMappingURL rule and literal
// tables.
func TestJSExtract(t *testing.T) {
	for _, file := range testkit.LoadCases[extractCases](t, "jsparse/extract") {
		for _, c := range file.Data.Cases {
			t.Run(file.Name+"/"+c.Name, func(t *testing.T) {
				st := SourceScript
				if c.Module {
					st = SourceModule
				}
				prog, err := Parse([]byte(c.Source), Options{SourceType: st, Tokens: true})
				if err != nil {
					t.Fatal(err)
				}
				if c.Strings != nil {
					got := prog.StringLiterals()
					var gs, ws []string
					for _, u := range got {
						gs = append(gs, fmt.Sprintf("%q %s callee=%q recv=%q arg=%d key=%q", u.Value, stringKindNames[u.Kind], u.CalleePath, u.ReceiverType, u.ArgIndex, u.PropertyKey))
					}
					for _, w := range c.Strings {
						ws = append(ws, fmt.Sprintf("%q %s callee=%q recv=%q arg=%d key=%q", w.Value, w.Kind, w.Callee, w.Receiver, w.Arg, w.Key))
					}
					if fmt.Sprint(gs) != fmt.Sprint(ws) {
						t.Errorf("strings\n got %q\nwant %q", gs, ws)
					}
				}
				if c.Modules != nil {
					var gs, ws []string
					for _, m := range prog.ModuleRefs() {
						gs = append(gs, fmt.Sprintf("%q dyn=%v lit=%v", m.Specifier, m.Dynamic, m.Literal))
					}
					for _, m := range c.Modules {
						ws = append(ws, fmt.Sprintf("%q dyn=%v lit=%v", m.Specifier, m.Dynamic, m.Literal))
					}
					if fmt.Sprint(gs) != fmt.Sprint(ws) {
						t.Errorf("modules\n got %q\nwant %q", gs, ws)
					}
				}
				if c.SourceMappingURL != nil {
					for _, tokens := range []bool{true, false} {
						p2, _ := Parse([]byte(c.Source), Options{SourceType: st, Tokens: tokens})
						got, _, _ := p2.SourceMappingURL()
						if got != *c.SourceMappingURL {
							t.Errorf("sourceMappingURL (tokens=%v) = %q, want %q", tokens, got, *c.SourceMappingURL)
						}
					}
				}
				if c.Tables != nil {
					tables := prog.LiteralTables(3, 100)
					var gs, ws []string
					for _, tb := range tables {
						gs = append(gs, "context="+tb.Context)
						for _, e := range tb.Entries {
							gs = append(gs, fmt.Sprintf("%s %s %q", e.Path, valueKindNames[e.Kind], e.Value))
						}
					}
					for _, tb := range c.Tables {
						ws = append(ws, "context="+tb.Context)
						for _, e := range tb.Entries {
							ws = append(ws, fmt.Sprintf("%s %s %q", e.Path, e.Kind, e.Value))
						}
					}
					if fmt.Sprint(gs) != fmt.Sprint(ws) {
						t.Errorf("tables\n got %q\nwant %q", gs, ws)
					}
				}
			})
		}
	}
}
