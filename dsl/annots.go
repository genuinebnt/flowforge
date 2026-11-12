package dsl

// This file validates annotations (spec 7.5, 8.7, 9.5, 9.8).

// annotTarget is where an annotation is written.
type annotTarget int

const (
	onWorkflow annotTarget = iota
	onFn
	onTool
	onType
	onEnum
	onUse
	onLet
)

var targetNames = [...]string{"a workflow", "a fn", "a tool", "a type", "an enum", "a use declaration", "a let statement"}

// knownAnnotations maps each annotation to the targets it may appear on.
var knownAnnotations = map[string][]annotTarget{
	"timeout":    {onWorkflow, onLet},
	"retry":      {onLet},
	"idempotent": {onWorkflow, onFn, onTool},
	"deprecated": {onWorkflow, onFn, onTool, onType, onEnum},
	"effects":    {onWorkflow},
	"on_unknown": {onWorkflow},
}

var annotationHelp = map[string]string{
	"timeout":    "#[timeout(d)] applies to workflows and let statements.",
	"retry":      "#[retry(max = n, backoff = exp(initial, max))] applies to let statements whose right side performs effects.",
	"idempotent": "#[idempotent] applies to workflows, fns and tools.",
	"deprecated": "#[deprecated(\"msg\")] applies to items.",
	"effects":    "#[effects(max = N)] applies to workflows.",
	"on_unknown": "#[on_unknown(retry)] applies to workflows.",
}

// MaxEffectsPerRun is the largest value of #[effects(max = N)] (spec 8.7).
const MaxEffectsPerRun = 1_000_000

func (c *checker) checkItemAnnotations(it Item) {
	var target annotTarget
	var attrs *Attrs
	switch d := it.(type) {
	case *WorkflowDecl:
		target, attrs = onWorkflow, &d.Attrs
	case *FnDecl:
		target, attrs = onFn, &d.Attrs
	case *ToolDecl:
		target, attrs = onTool, &d.Attrs
	case *TypeDecl:
		target, attrs = onType, &d.Attrs
	case *EnumDecl:
		target, attrs = onEnum, &d.Attrs
	case *UseDecl:
		target, attrs = onUse, &d.Attrs
	default:
		return
	}
	c.sink = c.mainSink
	c.checkAnnotations(it.Annots(), target, attrs, false)
}

func (c *checker) checkLetAnnotations(s *LetStmt, performs bool) {
	c.checkAnnotations(s.Annot, onLet, &s.Attrs, performs)
}

func (c *checker) checkAnnotations(annots []*Annotation, target annotTarget, attrs *Attrs, performs bool) {
	seen := map[string]bool{}
	for _, a := range annots {
		name := a.Name.Name
		targets, known := knownAnnotations[name]
		if !known {
			var names []string
			for n := range knownAnnotations {
				names = append(names, n)
			}
			d := c.sink.errorf(CodeUnknownAnnotation, a.Name.Span(), "unknown annotation",
				"unknown annotation `%s`", name)
			if sug := suggest(name, sortedStrings(names)); sug != "" {
				d.withHelp("Did you mean `%s`?", sug)
			} else {
				d.withHelp("Known annotations: deprecated, effects, idempotent, on_unknown, retry, timeout.")
			}
			continue
		}
		allowed := false
		for _, t := range targets {
			if t == target {
				allowed = true
			}
		}
		if !allowed {
			c.sink.errorf(CodeMisplacedAnnotation, a.Sp, "not allowed here",
				"annotation `%s` is not allowed on %s", name, targetNames[target]).
				withHelp("%s", annotationHelp[name])
			continue
		}
		if seen[name] {
			c.sink.errorf(CodeBadAnnotationArgs, a.Sp, "duplicate", "annotation `%s` is given more than once", name)
			continue
		}
		seen[name] = true
		switch name {
		case "timeout":
			if d, ok := c.oneDuration(a); ok {
				attrs.Timeout = d
			}
		case "retry":
			c.retryAnnotation(a, attrs)
			if !performs {
				c.sink.errorf(CodeMisplacedAnnotation, a.Sp, "nothing to retry",
					"#[retry] requires the right side of the let statement to perform effects").
					withHelp("%s", annotationHelp["retry"])
			}
		case "idempotent":
			if len(a.Args) > 0 {
				c.badAnnotArgs(a, "#[idempotent] takes no arguments")
			}
			attrs.Idempotent = true
		case "deprecated":
			attrs.HasDeprecated = true
			switch {
			case len(a.Args) == 0:
			case len(a.Args) == 1 && a.Args[0].Name == nil:
				if s, ok := plainString(a.Args[0].Value); ok {
					attrs.Deprecated = s
				} else {
					c.badAnnotArgs(a, "#[deprecated] takes one string literal")
				}
			default:
				c.badAnnotArgs(a, "#[deprecated] takes one string literal")
			}
		case "effects":
			if len(a.Args) != 1 || a.Args[0].Name == nil || a.Args[0].Name.Name != "max" {
				c.badAnnotArgs(a, "expected #[effects(max = N)]")
				continue
			}
			n, ok := a.Args[0].Value.(*IntLit)
			if !ok || n.Value < 1 || n.Value > MaxEffectsPerRun {
				c.badAnnotArgs(a, "#[effects(max = N)] needs an integer N between 1 and 1000000")
				continue
			}
			attrs.MaxEffects = n.Value
		case "on_unknown":
			if len(a.Args) != 1 || a.Args[0].Name != nil {
				c.badAnnotArgs(a, "expected #[on_unknown(retry)]")
				continue
			}
			id, ok := a.Args[0].Value.(*Ident)
			if !ok || id.Name != "retry" {
				c.badAnnotArgs(a, "expected #[on_unknown(retry)]")
				continue
			}
			attrs.OnUnknownRetry = true
		}
	}
}

func (c *checker) badAnnotArgs(a *Annotation, msg string) {
	c.sink.errorf(CodeBadAnnotationArgs, a.Sp, "invalid arguments", "%s", msg)
}

func (c *checker) oneDuration(a *Annotation) (int64, bool) {
	if len(a.Args) != 1 || a.Args[0].Name != nil {
		c.badAnnotArgs(a, "expected #[timeout(d)] with one duration such as 30s")
		return 0, false
	}
	d, ok := a.Args[0].Value.(*DurationLit)
	if !ok || d.Nanos <= 0 {
		c.badAnnotArgs(a, "expected #[timeout(d)] with one positive duration such as 30s")
		return 0, false
	}
	return d.Nanos, true
}

func (c *checker) retryAnnotation(a *Annotation, attrs *Attrs) {
	for _, arg := range a.Args {
		if arg.Name == nil {
			c.badAnnotArgs(a, "#[retry] takes named arguments: max = n, backoff = exp(initial, max)")
			return
		}
		switch arg.Name.Name {
		case "max":
			n, ok := arg.Value.(*IntLit)
			if !ok || n.Value < 1 {
				c.badAnnotArgs(a, "#[retry] max must be a positive integer")
				return
			}
			attrs.RetryMax = n.Value
		case "backoff":
			call, ok := arg.Value.(*CallExpr)
			fn, isIdent := Expr(nil), false
			if ok {
				fn = call.Fn
				_, isIdent = fn.(*Ident)
			}
			if !ok || !isIdent || fn.(*Ident).Name != "exp" || len(call.Args) != 2 {
				c.badAnnotArgs(a, "#[retry] backoff must be exp(initial, max)")
				return
			}
			ini, ok1 := call.Args[0].Value.(*DurationLit)
			mx, ok2 := call.Args[1].Value.(*DurationLit)
			if !ok1 || !ok2 || ini.Nanos <= 0 || mx.Nanos < ini.Nanos {
				c.badAnnotArgs(a, "#[retry] backoff = exp(initial, max) needs durations with 0 < initial <= max")
				return
			}
			attrs.BackoffInitial, attrs.BackoffMax = ini.Nanos, mx.Nanos
		default:
			c.badAnnotArgs(a, "#[retry] takes max and backoff")
			return
		}
	}
	if attrs.RetryMax == 0 {
		c.badAnnotArgs(a, "#[retry] requires max = n")
	}
}

// plainString returns the text of a string literal without interpolation.
func plainString(e Expr) (string, bool) {
	s, ok := e.(*StringLit)
	if !ok || s.Interpolated() {
		return "", false
	}
	out := ""
	for _, p := range s.Parts {
		out += p.Lit
	}
	return out, true
}

func sortedStrings(xs []string) []string {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
	return xs
}
