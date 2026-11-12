package dsl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// grammarProductions lists the EBNF productions of spec 7.3 and the parse
// case that exercises each one (dsl_001 requires one passing case per
// production).
var grammarProductions = map[string]string{
	"program":        "g_workflow_decl",
	"item":           "g_annotations",
	"annotations":    "g_annotations",
	"annotation":     "g_annotations",
	"use_decl":       "g_use_decl",
	"path":           "g_use_decl",
	"workflow_decl":  "g_workflow_decl",
	"fn_decl":        "g_fn_decl",
	"tool_decl":      "g_tool_decl",
	"effect":         "g_tool_decl",
	"params":         "g_fn_decl",
	"param":          "g_fn_decl",
	"generics":       "g_fn_decl",
	"type_decl":      "g_type_decl",
	"field":          "g_type_decl",
	"enum_decl":      "g_enum_decl",
	"variant":        "g_enum_decl",
	"type":           "g_types",
	"block":          "g_block_tail",
	"stmt":           "g_expr_stmt",
	"let_stmt":       "g_let_stmt",
	"for_stmt":       "g_for_stmt",
	"require_stmt":   "g_require_stmt",
	"return":         "g_return_stmt",
	"expr":           "p_arith",
	"pipe":           "g_pipes",
	"or":             "p_logic",
	"and":            "p_logic",
	"equality":       "p_range_relational",
	"relational":     "p_range_relational",
	"range":          "p_range_relational",
	"additive":       "p_arith",
	"multiplicative": "p_arith",
	"unary":          "p_unary",
	"postfix":        "g_postfix",
	"primary":        "g_primary",
	"record_lit":     "g_primary",
	"lambda":         "g_lambda",
	"if_expr":        "g_if_expr",
	"match_expr":     "g_match_expr",
	"arm":            "g_match_expr",
	"agent_expr":     "g_agent_expr",
	"args":           "g_named_args",
	"arg":            "g_named_args",
	"named_arg":      "g_agent_expr",
	"pattern":        "g_patterns",
	"literal":        "g_literals",
}

// TestDSL001_Grammar: every grammar production has a passing parse case
// whose AST equals the golden, and the precedence table holds (dsl_001).
func TestDSL001_Grammar(t *testing.T) {
	dir := testkit.ConformanceDir(t, "dsl/parse")
	for prod, c := range grammarProductions {
		if _, err := os.Stat(filepath.Join(dir, c+".ff")); err != nil {
			t.Errorf("production %s: case %s missing", prod, c)
		}
	}
	for _, c := range loadSuite(t, "dsl/parse", "g_", "p_") {
		t.Run(c.Name, func(t *testing.T) {
			res := runParseCase(t, c)
			if len(res.Diagnostics) > 0 {
				t.Fatalf("grammar case has diagnostics: %v", codes(res.Diagnostics))
			}
		})
	}
	// The spec 7.1 programs parse cleanly and survive a format round trip.
	for _, c := range loadSuite(t, "dsl/check", "spec_") {
		t.Run(c.Name, func(t *testing.T) {
			f, diags := Parse(c.Name+".ff", c.Src)
			if len(diags) > 0 {
				t.Fatalf("diagnostics: %v", diags)
			}
			assertRoundTrip(t, f)
		})
	}
	t.Run("pipe_desugaring", func(t *testing.T) {
		e, diags := ParseExpr("x.ff", []byte("a |> f(x) |> g"))
		if len(diags) > 0 {
			t.Fatal(diags)
		}
		outer, ok := e.(*CallExpr)
		if !ok || outer.Pipe != PipeBare || len(outer.Args) != 1 {
			t.Fatalf("outer: %#v", e)
		}
		inner, ok := outer.Args[0].Value.(*CallExpr)
		if !ok || inner.Pipe != PipeCall || len(inner.Args) != 2 || inner.Fn.(*Ident).Name != "f" {
			t.Fatalf("inner: %#v", outer.Args[0].Value)
		}
		if FormatExpr(e) != "a |> f(x) |> g" {
			t.Fatalf("format: %q", FormatExpr(e))
		}
	})
}

// TestDSL002_SyntaxErrors: one case per lexing and parsing error code,
// with recovery reporting several errors in one file (dsl_002).
func TestDSL002_SyntaxErrors(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range loadSuite(t, "dsl/parse", "e_") {
		t.Run(c.Name, func(t *testing.T) {
			res := runParseCase(t, c)
			if len(res.Diagnostics) == 0 {
				t.Fatal("expected diagnostics")
			}
			expectCodeInName(t, c.Name, res.Diagnostics)
			for _, d := range res.Diagnostics {
				seen[d.Code] = true
				assertSpanInSource(t, c.Src, d)
			}
			if strings.HasPrefix(c.Name, "e_recovery") && len(res.Diagnostics) < 2 {
				t.Fatalf("recovery case reported %d diagnostics, want >= 2", len(res.Diagnostics))
			}
		})
	}
	for _, code := range []string{CodeUnterminatedString, CodeInvalidEscape, CodeLiteralTooLarge,
		CodeUnknownDurationUnit, CodeUnexpectedChar, CodeUnterminatedComment, CodeMalformedNumber,
		CodeBadInterpolation, CodeUnexpectedToken, CodeMissingSemi, CodeNonAssocChain, CodeTooLarge} {
		if !seen[code] {
			t.Errorf("no parse case reports %s", code)
		}
	}
	t.Run("E0602_source_too_large", func(t *testing.T) {
		src := make([]byte, MaxSourceBytes+1)
		for i := range src {
			src[i] = ' '
		}
		_, diags := Parse("big.ff", src)
		if len(diags) != 1 || diags[0].Code != CodeTooLarge {
			t.Fatalf("got %v", codes(diags))
		}
	})
	t.Run("E0602_long_chain", func(t *testing.T) {
		src := "workflow w() { let x = 1" + strings.Repeat(" + 1", maxChain+10) + "; }"
		_, diags := Parse("chain.ff", []byte(src))
		if len(diags) != 1 || diags[0].Code != CodeTooLarge {
			t.Fatalf("got %v", codes(diags))
		}
	})
	t.Run("diagnostics_in_source_order", func(t *testing.T) {
		_, diags := Parse("o.ff", []byte("workflow a() { let x = 1 let y = \"\\q\"; }\nfn b( { }"))
		for i := 1; i < len(diags); i++ {
			if diags[i].Primary.Start.Offset < diags[i-1].Primary.Start.Offset {
				t.Fatalf("not in source order: %v", diags)
			}
		}
		if len(diags) < 3 {
			t.Fatalf("want at least 3 diagnostics, got %v", codes(diags))
		}
	})
}

// TestDSL003_TypeErrors: mismatched types, non-exhaustive matches, unknown
// names and wrong arity (dsl_003).
func TestDSL003_TypeErrors(t *testing.T) {
	for _, c := range loadSuite(t, "dsl/check", "t_", "n_", "m_") {
		t.Run(c.Name, func(t *testing.T) {
			a := runCheckCase(t, c)
			expectCodeInName(t, c.Name, a.Diagnostics)
			if strings.HasSuffix(c.Name, "_ok") && !a.OK {
				t.Fatalf("expected ok, got %v", codes(a.Diagnostics))
			}
		})
	}
}

// TestDSL004_EffectErrors: effects inside pure functions, missing effect
// declarations, policy-forbidden classes and approvals (dsl_004).
func TestDSL004_EffectErrors(t *testing.T) {
	for _, c := range loadSuite(t, "dsl/check", "f_", "a_") {
		t.Run(c.Name, func(t *testing.T) {
			a := runCheckCase(t, c)
			expectCodeInName(t, c.Name, a.Diagnostics)
			if strings.HasSuffix(c.Name, "_ok") && !a.OK {
				t.Fatalf("expected ok, got %v", codes(a.Diagnostics))
			}
		})
	}
	t.Run("policy_unknown_skips_policy_errors", func(t *testing.T) {
		src := string(testkit.ReadFile(t, "dsl/check", "f_E0403_missing_approval.ff"))
		a := Analyze(src, Options{})
		if !a.OK {
			t.Fatalf("without a policy only internal consistency is checked: %v", codes(a.Diagnostics))
		}
		a = Analyze(src, Options{Policy: Policy{MaxClass: core.ClassIntrusive, Known: true}})
		if a.OK || a.Diagnostics[0].Code != CodeMissingApproval {
			t.Fatalf("want E0403, got %v", codes(a.Diagnostics))
		}
	})
	t.Run("registry_tools", func(t *testing.T) {
		src := "workflow w(n: Fqdn) -> Option<IpAddress> { resolve(n) }"
		opts := Options{Tools: []ToolSpec{{Name: "resolve", Signature: "(Fqdn) -> Option<IpAddress>", ParamNames: []string{"name"}, Effects: []string{"passive"}}}}
		a := Analyze(src, opts)
		if !a.OK || strings.Join(a.InferredEffects, ",") != "passive" {
			t.Fatalf("got %+v", a)
		}
		opts = Options{ToolSource: "tool resolve(name: Fqdn) -> Option<IpAddress> ! passive"}
		f, _ := Parse("w.ff", []byte(src))
		prog, diags := Check(f, opts)
		if HasErrors(diags) || len(prog.Tools) != 1 || !prog.Tools[0].FromRegistry || prog.Tools[0].Effects != EffectSet(EffPassive) {
			t.Fatalf("got %v %+v", diags, prog.Tools)
		}
		// A source declaration that disagrees with the registry is E0203.
		bad := src + "\ntool resolve(name: Fqdn) -> Option<IpAddress> ! active"
		if a := Analyze(bad, opts); a.OK || a.Diagnostics[0].Code != CodeUnknownTool {
			t.Fatalf("want E0203, got %v", codes(a.Diagnostics))
		}
	})
}

// TestDSL005_ScopeTyping: an active tool call with an unchecked target is
// E0301; the same with scope.check compiles (dsl_005).
func TestDSL005_ScopeTyping(t *testing.T) {
	for _, c := range loadSuite(t, "dsl/check", "s_", "rest_") {
		t.Run(c.Name, func(t *testing.T) {
			a := runCheckCase(t, c)
			expectCodeInName(t, c.Name, a.Diagnostics)
			if strings.HasSuffix(c.Name, "_ok") && !a.OK {
				t.Fatalf("expected ok, got %v", codes(a.Diagnostics))
			}
		})
	}
	t.Run("rest_example_location", func(t *testing.T) {
		src := "workflow recon(roots: List<Fqdn>) { let h = httpx(Host::Name(roots[0])); }\n" +
			"tool httpx(host: InScope<Host>) -> Option<HttpService> ! active\n" +
			"type HttpService = { url: Url, status: Int, title: Option<String>, tech: List<Product> }\n"
		a := Analyze(src, Options{})
		if a.OK || len(a.Diagnostics) != 1 {
			t.Fatalf("got %+v", a)
		}
		d := a.Diagnostics[0]
		arg := "Host::Name(roots[0])"
		start := strings.Index(src, arg)
		if d.Code != CodeTypeMismatch || d.Primary.File != "recon.ff" ||
			d.Primary.Start.Offset != start || d.Primary.End.Offset != start+len(arg) ||
			d.Primary.End.Col != 71 || d.Primary.End.Offset != 70 {
			t.Fatalf("diagnostic %+v", d)
		}
		if d.Message != "httpx expects InScope<Host> but found Host" ||
			d.Help != "Call scope.check(Host::Name(roots[0]))? to obtain an InScope<Host>." ||
			d.Primary.Label != "wrap in scope.check(...)?" {
			t.Fatalf("diagnostic text %+v", d)
		}
		if strings.Join(a.InferredEffects, ",") != "active" || a.InferredClass != core.ClassActive {
			t.Fatalf("effects %+v", a)
		}
	})
	t.Run("checked_call_is_typed", func(t *testing.T) {
		src := "workflow w(n: Fqdn) { let web = httpx(scope.check(Host::Name(n))?); }\n" +
			"tool httpx(host: InScope<Host>) -> Option<Url> ! active\n"
		f, _ := Parse("w.ff", []byte(src))
		_, diags := Check(f, Options{})
		if HasErrors(diags) {
			t.Fatal(diags)
		}
		var call *CallExpr
		Walk(f.Items[0], func(n Node) bool {
			if c, ok := n.(*CallExpr); ok && c.Kind == CallTool {
				call = c
			}
			return true
		})
		if call == nil || call.Tool.Name != "httpx" || call.Own != EffectSet(EffActive) {
			t.Fatalf("tool call not resolved: %+v", call)
		}
		try := call.Args[0].Value.(*TryExpr)
		if try.Mode != TryFailRun || try.Type().String() != "InScope<Host>" {
			t.Fatalf("try %v %v", try.Mode, try.Type())
		}
		sc := try.X.(*CallExpr)
		if sc.Kind != CallBuiltin || sc.Builtin.Name != "scope.check" || sc.Builtin.Op != OpScopeCheck {
			t.Fatalf("scope.check not resolved: %+v", sc)
		}
	})
}

// findLambdas returns the lambdas of a file in source order.
func findLambdas(f *File) []*Lambda {
	var out []*Lambda
	for _, it := range f.Items {
		Walk(it, func(n Node) bool {
			if l, ok := n.(*Lambda); ok {
				out = append(out, l)
			}
			return true
		})
	}
	return out
}

func captureNames(l *Lambda) string {
	var parts []string
	for _, c := range l.Captures {
		s := c.Binding.Name
		if c.Outer >= 0 {
			s += "@" + itoa(c.Outer)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ",")
}

// TestDSL006_Closures: closures capture by value, shadowing, and
// immutability violations (dsl_006).
func TestDSL006_Closures(t *testing.T) {
	for _, c := range loadSuite(t, "dsl/check", "c_") {
		t.Run(c.Name, func(t *testing.T) {
			a := runCheckCase(t, c)
			expectCodeInName(t, c.Name, a.Diagnostics)
		})
	}
	check := func(t *testing.T, src string) *File {
		t.Helper()
		f, diags := Parse("c.ff", []byte(src))
		if len(diags) > 0 {
			t.Fatal(diags)
		}
		if _, diags := Check(f, Options{}); HasErrors(diags) {
			t.Fatal(diags)
		}
		return f
	}
	t.Run("captures", func(t *testing.T) {
		f := check(t, string(testkit.ReadFile(t, "dsl/check", "c_captures_ok.ff")))
		ls := findLambdas(f)
		if len(ls) != 3 {
			t.Fatalf("lambdas: %d", len(ls))
		}
		// add = x => x + base + k
		if got := captureNames(ls[0]); got != "base,k" {
			t.Errorf("add captures %q", got)
		}
		// x => xs |> fold(0, (acc, y) => acc + y + x + base): the middle
		// lambda captures xs and, on behalf of the inner one, base.
		if got := captureNames(ls[1]); got != "xs,base" {
			t.Errorf("middle captures %q", got)
		}
		// The inner lambda takes x from the middle lambda's slot and base
		// from the middle lambda's capture 1.
		if got := captureNames(ls[2]); got != "x,base@1" {
			t.Errorf("inner captures %q", got)
		}
		// Identifiers inside the inner lambda resolve to captures.
		var refs []string
		Walk(ls[2].Body, func(n Node) bool {
			if id, ok := n.(*Ident); ok {
				switch id.Ref.Kind {
				case RefLocal:
					refs = append(refs, id.Name+":local"+itoa(id.Ref.Binding.Slot))
				case RefCapture:
					refs = append(refs, id.Name+":cap"+itoa(id.Ref.Capture))
				}
			}
			return true
		})
		if strings.Join(refs, " ") != "acc:local0 y:local1 x:cap0 base:cap1" {
			t.Errorf("inner refs %v", refs)
		}
	})
	t.Run("shadowing_and_capture_by_value", func(t *testing.T) {
		f := check(t, string(testkit.ReadFile(t, "dsl/check", "c_shadow_capture.ff")))
		fn := f.Items[0].(*FnDecl)
		g := findLambdas(f)[0]
		if len(g.Captures) != 1 {
			t.Fatalf("captures %d", len(g.Captures))
		}
		outerX := g.Captures[0].Binding
		var innerX *Binding
		for _, b := range fn.Func.Frame.Bindings {
			if b.Name == "x" && b != outerX {
				innerX = b
			}
		}
		if innerX == nil || innerX.ID == outerX.ID || innerX.Slot == outerX.Slot {
			t.Fatalf("shadowing binding not distinct: %+v %+v", innerX, outerX)
		}
	})
	t.Run("slots", func(t *testing.T) {
		f := check(t, "fn f(a: Int, b: Int) -> Int { let c = a + b; let (d, e) = (c, c); d + e }")
		fr := f.Items[0].(*FnDecl).Func.Frame
		var names []string
		for i, b := range fr.Bindings {
			if b.Slot != i || b.Frame != fr {
				t.Fatalf("binding %s has slot %d at %d", b.Name, b.Slot, i)
			}
			names = append(names, b.Name)
		}
		if strings.Join(names, ",") != "a,b,c,d,e" || fr.Bindings[0].Kind != BindParam {
			t.Fatalf("frame %v", names)
		}
	})
	t.Run("compensation_binds_it", func(t *testing.T) {
		f := check(t, "tool post(x: Int) -> Int ! write\ntool undo(x: Int) -> () ! write\n"+
			"workflow w(n: Int) { let id = post(n) compensate undo(it + n); }")
		let := f.Items[2].(*WorkflowDecl).Body.Stmts[0].(*LetStmt)
		comp := let.Compensate
		if comp == nil || !comp.Synthetic || comp.Params[0].Binding.Kind != BindIt || comp.Params[0].Binding.Slot != 0 {
			t.Fatalf("compensation %+v", comp)
		}
		if got := captureNames(comp); got != "n" {
			t.Fatalf("compensation captures %q", got)
		}
	})
	t.Run("immutability", func(t *testing.T) {
		a := Analyze("fn f() -> Int { let x = 1; x = 2; x }", Options{})
		if a.OK || a.Diagnostics[0].Code != CodeImmutable || len(a.Diagnostics[0].Secondary) != 1 {
			t.Fatalf("got %+v", a.Diagnostics)
		}
	})
}

// TestDSLProgramAPI pins the typed-tree contract the compiler relies on.
func TestDSLProgramAPI(t *testing.T) {
	src := testkit.ReadFile(t, "dsl/check", "spec_recon.ff")
	f, diags := Parse("recon.ff", src)
	if len(diags) > 0 {
		t.Fatal(diags)
	}
	prog, diags := Check(f, Options{})
	if HasErrors(diags) {
		t.Fatal(diags)
	}
	w := prog.Workflow("recon")
	if w == nil || w.Class != core.ClassActive || w.Effects.String() != "passive, active" {
		t.Fatalf("workflow summary %+v", w)
	}
	if w.Decl.Attrs.Timeout != 2*3600*1_000_000_000 {
		t.Fatalf("timeout %d", w.Decl.Attrs.Timeout)
	}
	var toolNames []string
	for _, tl := range w.Tools {
		toolNames = append(toolNames, tl.Name)
	}
	if strings.Join(toolNames, ",") != "subfinder,resolve,httpx" {
		t.Fatalf("tools %v", toolNames)
	}
	for _, op := range []OpKind{OpToolCall, OpScopeCheck, OpParMap, OpEmitAlert} {
		if !w.Ops.Has(op) {
			t.Errorf("missing op %v in %v", op, w.Ops.Names())
		}
	}
	body := w.Decl.Body
	types := map[string]string{}
	for _, s := range body.Stmts {
		if l, ok := s.(*LetStmt); ok {
			bp := l.Pat.(*BindPat)
			types[bp.Name.Name] = bp.Binding.Type.String()
			if bp.Name.Name == "web" && (l.Attrs.RetryMax != 3 || l.Attrs.BackoffMax != 30_000_000_000) {
				t.Errorf("retry attrs %+v", l.Attrs)
			}
		}
	}
	want := map[string]string{"subs": "Stream<Fqdn>", "live": "Stream<IpAddress>", "web": "Stream<HttpService>"}
	for k, v := range want {
		if types[k] != v {
			t.Errorf("%s: %s, want %s", k, types[k], v)
		}
	}
	// No expression is left without a concrete type.
	Walk(body, func(n Node) bool {
		if e, ok := n.(Expr); ok {
			if _, bad := hasVars(e.Type()); bad || e.Type() == nil {
				t.Errorf("unresolved type at %+v: %v", e.Span(), e.Type())
			}
		}
		return true
	})
	if prog.Types[0] != defList || prog.Types[len(builtinTypeDefs)].Name != "HttpService" {
		t.Fatalf("type table order")
	}
}
