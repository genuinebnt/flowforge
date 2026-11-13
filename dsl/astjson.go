package dsl

import (
	"strconv"

	"github.com/genuinebnt/flowforge/core"
)

// ASTJSON returns the canonical JSON form of a file's syntax tree, used by
// the conformance goldens (conformance/dsl/README.md documents the
// schema). It contains structure only: no spans and no checker
// annotations, so two trees are equal exactly when their ASTJSON is.
//
// PORT: port this printer exactly or the parse goldens are useless
// (docs/PORTING_RUST.md 5.11). Numbers are emitted as JSON strings:
// ints and duration nanoseconds in decimal, floats with
// strconv.FormatFloat(f, 'g', -1, 64), the shortest round-trip digits in
// exponent form when the decimal exponent is below -4 or at least 6
// ("1e+06", "1.23456789e+08", "1e-05", "0.0001"), with a sign and at least
// two exponent digits. Rust's {} and {:e} formats differ; write the
// algorithm out.
func ASTJSON(f *File) ([]byte, error) { return core.CanonicalJSON(ASTValue(f)) }

// ASTValue returns the canonical AST as plain JSON data (maps, slices,
// strings, numbers, booleans).
func ASTValue(f *File) any {
	items := []any{}
	for _, it := range f.Items {
		items = append(items, itemJSON(it))
	}
	return map[string]any{"items": items}
}

// ExprValue returns the canonical JSON data of one expression.
func ExprValue(e Expr) any { return exprJSON(e) }

// obj builds a node object, dropping nil, empty-slice and false values.
func obj(kind string, kv ...any) map[string]any {
	m := map[string]any{"kind": kind}
	for i := 0; i+1 < len(kv); i += 2 {
		k := kv[i].(string)
		switch v := kv[i+1].(type) {
		case nil:
			continue
		case []any:
			if len(v) == 0 {
				continue
			}
		case bool:
			if !v {
				continue
			}
		case map[string]any:
			if v == nil {
				continue
			}
		}
		m[k] = kv[i+1]
	}
	return m
}

func identsJSON(ids []*Ident) []any {
	out := []any{}
	for _, id := range ids {
		out = append(out, id.Name)
	}
	return out
}

func annotsJSON(as []*Annotation) []any {
	out := []any{}
	for _, a := range as {
		m := map[string]any{"name": a.Name.Name}
		if a.HasParens || len(a.Args) > 0 {
			m["args"] = argsJSON(a.Args)
		}
		out = append(out, m)
	}
	return out
}

func argsJSON(as []*Arg) []any {
	out := []any{}
	for _, a := range as {
		m := map[string]any{"value": exprJSON(a.Value)}
		if a.Name != nil {
			m["name"] = a.Name.Name
		}
		out = append(out, m)
	}
	return out
}

func paramsJSON(ps []*Param) []any {
	out := []any{}
	for _, p := range ps {
		out = append(out, map[string]any{"name": p.Name.Name, "type": typeJSON(p.TypeX)})
	}
	return out
}

func effectsJSON(ec *EffectClause) any {
	if ec == nil {
		return nil
	}
	return identsJSON(ec.Names)
}

func optType(t TypeExpr) any {
	if t == nil {
		return nil
	}
	return typeJSON(t)
}

func itemJSON(it Item) any {
	switch d := it.(type) {
	case *WorkflowDecl:
		return obj("workflow", "name", d.Name.Name, "annotations", annotsJSON(d.Annot),
			"params", paramsJSON(d.Params), "result", optType(d.Result),
			"effects", effectsJSON(d.Effects), "body", blockJSON(d.Body))
	case *FnDecl:
		return obj("fn", "name", d.Name.Name, "annotations", annotsJSON(d.Annot),
			"generics", identsJSON(d.Generics), "params", paramsJSON(d.Params),
			"result", optType(d.Result), "effects", effectsJSON(d.Effects), "body", blockJSON(d.Body))
	case *ToolDecl:
		return obj("tool", "name", d.Name.Name, "annotations", annotsJSON(d.Annot),
			"params", paramsJSON(d.Params), "result", optType(d.Result), "effects", effectsJSON(d.Effects))
	case *TypeDecl:
		fields := []any{}
		for _, f := range d.Fields {
			fields = append(fields, map[string]any{"name": f.Name.Name, "type": typeJSON(f.TypeX)})
		}
		return obj("type", "name", d.Name.Name, "annotations", annotsJSON(d.Annot),
			"generics", identsJSON(d.Generics), "fields", fields)
	case *EnumDecl:
		vs := []any{}
		for _, v := range d.Variants {
			m := map[string]any{"name": v.Name.Name}
			if len(v.Payload) > 0 {
				ps := []any{}
				for _, t := range v.Payload {
					ps = append(ps, typeJSON(t))
				}
				m["payload"] = ps
			}
			vs = append(vs, m)
		}
		return obj("enum", "name", d.Name.Name, "annotations", annotsJSON(d.Annot),
			"generics", identsJSON(d.Generics), "variants", vs)
	case *UseDecl:
		return obj("use", "annotations", annotsJSON(d.Annot), "path", identsJSON(d.Path))
	}
	return obj("unknown_item")
}

func typeJSON(t TypeExpr) any {
	switch t := t.(type) {
	case *NamedTypeExpr:
		args := []any{}
		for _, a := range t.Args {
			args = append(args, typeJSON(a))
		}
		return obj("named", "name", t.Name.Name, "args", args)
	case *TupleTypeExpr:
		el := []any{}
		for _, e := range t.Elems {
			el = append(el, typeJSON(e))
		}
		if len(el) == 0 {
			return obj("unit")
		}
		return obj("tuple", "elems", el)
	case *FuncTypeExpr:
		ps := []any{}
		for _, e := range t.Params {
			ps = append(ps, typeJSON(e))
		}
		return obj("func", "params", ps, "result", typeJSON(t.Result))
	}
	return obj("bad_type")
}

func blockJSON(b *BlockExpr) any {
	if b == nil {
		return obj("block")
	}
	stmts := []any{}
	for _, s := range b.Stmts {
		stmts = append(stmts, stmtJSON(s))
	}
	var tail any
	if b.Tail != nil {
		tail = exprJSON(b.Tail)
	}
	return obj("block", "stmts", stmts, "tail", tail)
}

func stmtJSON(s Stmt) any {
	switch s := s.(type) {
	case *LetStmt:
		var comp any
		if s.Compensate != nil {
			comp = exprJSON(s.Compensate.Body)
		}
		return obj("let", "annotations", annotsJSON(s.Annot), "pattern", patJSON(s.Pat),
			"type", optType(s.TypeX), "value", exprJSON(s.Value), "compensate", comp)
	case *ForStmt:
		return obj("for", "pattern", patJSON(s.Pat), "iter", exprJSON(s.Iter), "body", blockJSON(s.Body))
	case *RequireStmt:
		return obj("require_approval", "args", argsJSON(s.Args))
	case *ReturnStmt:
		var v any
		if s.Value != nil {
			v = exprJSON(s.Value)
		}
		return obj("return", "value", v)
	case *ExprStmt:
		return obj("expr", "expr", exprJSON(s.X))
	case *AssignStmt:
		return obj("assign", "target", s.Target.Name, "value", exprJSON(s.Value))
	}
	return obj("bad_stmt")
}

var pipeNames = [...]string{"", "bare", "call"}

func exprJSON(e Expr) any {
	switch x := e.(type) {
	case nil:
		return nil
	case *IntLit:
		return obj("int", "value", strconv.FormatInt(x.Value, 10))
	case *FloatLit:
		return obj("float", "value", strconv.FormatFloat(x.Value, 'g', -1, 64))
	case *DurationLit:
		return obj("duration", "nanos", strconv.FormatInt(x.Nanos, 10))
	case *BoolLit:
		return obj("bool", "value", strconv.FormatBool(x.Value))
	case *StringLit:
		parts := []any{}
		for _, p := range x.Parts {
			if p.X != nil {
				parts = append(parts, map[string]any{"expr": exprJSON(p.X)})
			} else {
				parts = append(parts, map[string]any{"lit": p.Lit})
			}
		}
		return obj("string", "parts", parts)
	case *Ident:
		return obj("ident", "name", x.Name)
	case *PathExpr:
		return obj("path", "segments", identsJSON(x.Segs))
	case *TupleExpr:
		el := []any{}
		for _, e := range x.Elems {
			el = append(el, exprJSON(e))
		}
		if len(el) == 0 {
			return obj("unit")
		}
		return obj("tuple", "elems", el)
	case *ListExpr:
		el := []any{}
		for _, e := range x.Elems {
			el = append(el, exprJSON(e))
		}
		return obj("list", "elems", el)
	case *RecordLit:
		fs := []any{}
		for _, f := range x.Fields {
			fs = append(fs, map[string]any{"name": f.Name.Name, "value": exprJSON(f.Value)})
		}
		return obj("record", "path", identsJSON(x.Path), "fields", fs)
	case *Lambda:
		ps := []any{}
		for _, p := range x.Params {
			ps = append(ps, p.Name.Name)
		}
		return obj("lambda", "params", ps, "body", exprJSON(x.Body))
	case *IfExpr:
		return obj("if", "cond", exprJSON(x.Cond), "then", blockJSON(x.Then), "else", exprJSON(x.Else))
	case *MatchExpr:
		arms := []any{}
		for _, a := range x.Arms {
			m := map[string]any{"pattern": patJSON(a.Pat), "body": exprJSON(a.Body)}
			if a.Guard != nil {
				m["guard"] = exprJSON(a.Guard)
			}
			arms = append(arms, m)
		}
		return obj("match", "scrutinee", exprJSON(x.Scrutinee), "arms", arms)
	case *AgentExpr:
		return obj("agent", "name", x.Name.Name, "args", argsJSON(x.Args))
	case *BlockExpr:
		return blockJSON(x)
	case *UnaryExpr:
		op := "-"
		if x.Op == OpNot {
			op = "!"
		}
		return obj("unary", "op", op, "operand", exprJSON(x.X))
	case *BinaryExpr:
		return obj("binary", "op", x.Op.String(), "left", exprJSON(x.X), "right", exprJSON(x.Y))
	case *CallExpr:
		var pipe any
		if x.Pipe != PipeNone {
			pipe = pipeNames[x.Pipe]
		}
		return obj("call", "callee", exprJSON(x.Fn), "args", argsJSON(x.Args), "pipe", pipe)
	case *IndexExpr:
		return obj("index", "target", exprJSON(x.X), "index", exprJSON(x.Index))
	case *FieldExpr:
		return obj("field", "target", exprJSON(x.X), "name", x.Name.Name)
	case *TryExpr:
		return obj("try", "operand", exprJSON(x.X))
	case *CastExpr:
		return obj("cast", "operand", exprJSON(x.X), "type", typeJSON(x.To))
	case *BadExpr:
		return obj("bad")
	}
	return obj("bad")
}

func patJSON(p Pattern) any {
	switch x := p.(type) {
	case *WildcardPat:
		return obj("wildcard")
	case *BindPat:
		return obj("bind", "name", x.Name.Name)
	case *LitPat:
		return obj("literal", "value", exprJSON(x.Lit))
	case *VariantPat:
		args := []any{}
		for _, a := range x.Args {
			args = append(args, patJSON(a))
		}
		return obj("variant", "path", identsJSON(x.Path), "parens", x.HasParens, "args", args)
	case *TuplePat:
		el := []any{}
		for _, e := range x.Elems {
			el = append(el, patJSON(e))
		}
		if len(el) == 0 {
			return obj("unit")
		}
		return obj("tuple", "elems", el)
	case *RecordPat:
		fs := []any{}
		for _, f := range x.Fields {
			m := map[string]any{"name": f.Name.Name}
			if f.Pat != nil {
				m["pattern"] = patJSON(f.Pat)
			}
			fs = append(fs, m)
		}
		return obj("record", "path", identsJSON(x.Path), "fields", fs)
	}
	return obj("bad_pattern")
}
