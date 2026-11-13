package jsparse

import (
	"strings"
	"testing"
)

// TestJSTolerantRecovery pins the recovery behaviour: after an error the
// parser resumes at the next statement boundary (a `;`, the `}` closing
// the enclosing block, or a statement keyword starting a line), so the
// statements around the damage survive and extraction still works.
func TestJSTolerantRecovery(t *testing.T) {
	cases := []struct {
		src       string
		diags     int    // number of diagnostics
		firstCode Code   // code of the first diagnostic
		want      string // top-level statement kinds, comma separated
		strings   string // StringLiterals values, comma separated
	}{
		{"var a = ;\nfunction f(){ return 1 }\nfetch('/api')", 1, CodeUnexpectedToken,
			"VariableDeclaration,FunctionDeclaration,ExpressionStatement", "/api"},
		{"function g(){ x = ; y('/in') }; z('/out')", 1, CodeUnexpectedToken,
			"FunctionDeclaration,EmptyStatement,ExpressionStatement", "/in,/out"},
		{"a(}\nb('/b');\nif (\nconst c = '/c'", 2, CodeUnexpectedToken,
			"ExpressionStatement,ExpressionStatement,IfStatement,VariableDeclaration", "/b,/c"},
		{"x = `tpl ${ ) }`; ok('/ok')", 1, CodeUnexpectedToken,
			"ExpressionStatement,ExpressionStatement", "tpl ,/ok"},
		{"let let = 1; let let = 2; f('/still')", 3, CodeLetName,
			"VariableDeclaration,VariableDeclaration,ExpressionStatement", "/still"},
		{"}}} g('/after-braces')", 3, CodeUnexpectedToken,
			"ExpressionStatement", "/after-braces"},
	}
	for _, c := range cases {
		prog, err := Parse([]byte(c.src), Options{Tolerant: true})
		if err != nil {
			t.Fatalf("%q: tolerant mode returned %v", c.src, err)
		}
		if len(prog.Diagnostics) != c.diags || prog.Diagnostics[0].Code != c.firstCode {
			t.Errorf("%q: diagnostics %v, want %d starting with %v", c.src, prog.Diagnostics, c.diags, c.firstCode)
		}
		var kinds []string
		for _, s := range prog.List(prog.Nodes[prog.Root].List) {
			kinds = append(kinds, prog.Nodes[s].Kind.String())
		}
		if got := strings.Join(kinds, ","); got != c.want {
			t.Errorf("%q: statements %s, want %s", c.src, got, c.want)
		}
		var strs []string
		for _, u := range prog.StringLiterals() {
			strs = append(strs, u.Value)
		}
		if got := strings.Join(strs, ","); got != c.strings {
			t.Errorf("%q: strings %s, want %s", c.src, got, c.strings)
		}
		if err := checkArena(prog, false); err != nil {
			t.Errorf("%q: %v", c.src, err)
		}
	}
}
