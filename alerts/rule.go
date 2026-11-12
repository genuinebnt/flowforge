package alerts

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxSourceLen bounds the size of one rule's source text.
const MaxSourceLen = 64 << 10

// ErrRuleInvalid is returned when a rule with error diagnostics is used
// (for example enabled or added to a RuleSet); spec 15.3.
var ErrRuleInvalid = errors.New("alerts: rule has compile errors")

// ActionKind is a channel kind (spec 15.4).
type ActionKind string

// Channel kinds.
const (
	ActionSlack   ActionKind = "slack"
	ActionEmail   ActionKind = "email"
	ActionWebhook ActionKind = "webhook"
)

// Action is one compiled action. Target is the Slack channel (empty: the
// channel's default_channel), the e-mail address or the webhook channel
// name.
type Action struct {
	Kind   ActionKind `json:"kind"`
	Target string     `json:"target,omitempty"`
}

// Options tunes compilation.
type Options struct {
	// File names the source in diagnostics. Default "rule".
	File string
	// LoadLocation resolves quiet_hours zone names. Default
	// time.LoadLocation.
	LoadLocation func(name string) (*time.Location, error)
}

// Rule is a compiled, type-checked alert rule. It is immutable and safe
// for concurrent use.
type Rule struct {
	Name       string
	Source     string
	Actions    []Action
	Severity   Severity
	Throttle   time.Duration // 0: no throttling
	Digest     time.Duration // 0: deliver each match
	GroupBy    []string      // source text of the group_by expressions
	QuietHours *QuietHours   // nil: none

	cond       evalFn
	groups     []evalFn
	eventTypes []string // non-nil: the rule can only match these event types
}

// Compile compiles one rule with default options.
func Compile(source string) (*Rule, []Diagnostic) { return CompileWith(source, Options{}) }

// CompileWith compiles one rule. It returns every diagnostic it can
// produce, in source order; the rule is nil if any has error severity.
func CompileWith(source string, opts Options) (*Rule, []Diagnostic) {
	if opts.File == "" {
		opts.File = "rule"
	}
	if opts.LoadLocation == nil {
		opts.LoadLocation = time.LoadLocation
	}
	diag := newDiagSink(opts.File, source)
	if len(source) > MaxSourceLen {
		diag.add(CodeTooLarge, span{0, 0}, "rule starts here", "rule source is %d bytes; the limit is %d", len(source), MaxSourceLen)
		return nil, diag.sorted()
	}
	if !utf8.ValidString(source) {
		i := firstInvalidUTF8(source)
		diag.add(CodeInvalidUTF8, span{i, i + 1}, "invalid byte", "rule source is not valid UTF-8")
		return nil, diag.sorted()
	}
	toks, runToEOF := lex(source, diag)
	ast := parseRule(toks, runToEOF, diag)
	if ast == nil {
		return nil, diag.sorted()
	}
	r := (&ruleCompiler{diag: diag, src: source, opts: opts}).compile(ast)
	ds := diag.sorted()
	if HasErrors(ds) {
		return nil, ds
	}
	return r, ds
}

// MustCompile compiles a rule known to be valid (constants, tests) and
// panics otherwise; it is for programmer-supplied source only.
func MustCompile(source string) *Rule {
	r, ds := Compile(source)
	if r == nil {
		panic(fmt.Sprintf("alerts: MustCompile: %v", ds))
	}
	return r
}

func firstInvalidUTF8(s string) int {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			return i
		}
		i += size
	}
	return 0
}

type ruleCompiler struct {
	diag *diagSink
	src  string
	opts Options
}

func (rc *ruleCompiler) compile(ast *ruleAST) *Rule {
	r := &Rule{Name: ast.name, Source: rc.src, Severity: DefaultSeverity}
	ck := &checker{diag: rc.diag}
	if ast.cond != nil {
		t, fn := ck.check(ast.cond)
		switch {
		case t.k == kInvalid:
		case t.k != kBool:
			rc.diag.add(CodeConditionNotBool, ast.cond.exprSpan(), "has type "+t.String(), "the condition must be Bool, found %s", t)
		default:
			r.cond = fn
			r.eventTypes = requiredEventTypes(ast.cond)
		}
	}
	sevFrom := rc.actions(ast.actions, r)
	rc.clauses(ast.clauses, r, ck, sevFrom)
	return r
}

// severitySource remembers where a severity was set, for conflicts.
type severitySource struct {
	set bool
	sev Severity
	sp  span
}

func (rc *ruleCompiler) actions(actions []actionAST, r *Rule) severitySource {
	var sev severitySource
	for _, a := range actions {
		kind := ActionKind(a.name)
		var minPos, maxPos int
		switch kind {
		case ActionSlack:
			minPos, maxPos = 0, 1
		case ActionEmail, ActionWebhook:
			minPos, maxPos = 1, 1
		default:
			rc.diag.add(CodeUnknownAction, a.nameSp, "unknown action", "unknown action `%s`", a.name).
				Help = "actions are slack(\"channel\"), email(\"address\") and webhook(\"name\")"
			continue
		}
		act := Action{Kind: kind}
		var positional []argAST
		named := map[string]bool{}
		ok := true
		for _, arg := range a.args {
			if arg.name == "" {
				positional = append(positional, arg)
				continue
			}
			if arg.name != "severity" {
				rc.diag.add(CodeUnknownArgument, arg.nameSp, "unknown argument", "unknown argument `%s` for `%s`", arg.name, a.name).
					Help = "the only named action argument is severity = High"
				ok = false
				continue
			}
			if named[arg.name] {
				rc.diag.add(CodeUnknownArgument, arg.nameSp, "repeated", "argument `%s` given twice", arg.name)
				ok = false
				continue
			}
			named[arg.name] = true
			s, valSp, good := rc.severityArg(arg.val)
			if !good {
				ok = false
				continue
			}
			if !rc.mergeSeverity(&sev, s, valSp) {
				ok = false
			}
		}
		if len(positional) < minPos || len(positional) > maxPos {
			rc.diag.add(CodeArity, a.sp, "wrong number of arguments", "`%s` takes %s positional argument(s), found %d", a.name, arityText(minPos, maxPos), len(positional))
			continue
		}
		for _, arg := range positional {
			if _, bad := arg.val.(*badExpr); bad {
				ok = false // already reported by the lexer
				continue
			}
			lit, isLit := arg.val.(*litExpr)
			if !isLit || lit.val.k != kString {
				rc.diag.add(CodeActionArgument, arg.val.exprSpan(), "not a string literal", "arguments of `%s` must be string literals", a.name)
				ok = false
				continue
			}
			act.Target = lit.val.s
			if !rc.validTarget(kind, lit) {
				ok = false
			}
		}
		if ok {
			r.Actions = append(r.Actions, act)
		}
	}
	return sev
}

func arityText(lo, hi int) string {
	if lo == hi {
		return fmt.Sprint(lo)
	}
	return fmt.Sprintf("%d to %d", lo, hi)
}

func (rc *ruleCompiler) validTarget(kind ActionKind, lit *litExpr) bool {
	t := lit.val.s
	switch kind {
	case ActionEmail:
		local, domain, found := strings.Cut(t, "@")
		if !found || local == "" || domain == "" || strings.ContainsAny(t, " \t\r\n<>,;") || strings.Contains(domain, "@") || !strings.Contains(domain, ".") {
			rc.diag.add(CodeInvalidAddress, lit.sp, "not an e-mail address", "invalid e-mail address %q", t)
			return false
		}
	case ActionWebhook, ActionSlack:
		if strings.TrimSpace(t) == "" || strings.ContainsAny(t, "\r\n") {
			rc.diag.add(CodeInvalidAddress, lit.sp, "empty or multi-line", "invalid %s target %q", kind, t)
			return false
		}
	}
	return true
}

// severityArg reads `severity = IDENT` in an action.
func (rc *ruleCompiler) severityArg(e expr) (Severity, span, bool) {
	p, ok := e.(*pathExpr)
	if !ok || len(p.parts) != 1 {
		rc.diag.add(CodeActionArgument, e.exprSpan(), "expected a severity name", "severity must be one of Info, Low, Medium, High, Critical")
		return 0, span{}, false
	}
	s, ok := rc.severity(p.parts[0], p.sp)
	return s, p.sp, ok
}

func (rc *ruleCompiler) severity(id string, sp span) (Severity, bool) {
	s, ok := severityFromIdent(id)
	if !ok {
		d := rc.diag.add(CodeUnknownSeverity, sp, "unknown severity", "unknown severity `%s`", id)
		if v, err := ParseSeverity(id); err == nil {
			d.Help = "did you mean " + v.ident() + "?"
		} else {
			d.Help = "severities are Info, Low, Medium, High and Critical"
		}
	}
	return s, ok
}

// mergeSeverity records a severity; a second, different one is a conflict
// (an alert event has exactly one severity, spec 10.6).
func (rc *ruleCompiler) mergeSeverity(cur *severitySource, s Severity, sp span) bool {
	if !cur.set {
		*cur = severitySource{set: true, sev: s, sp: sp}
		return true
	}
	if cur.sev == s {
		return true
	}
	d := rc.diag.add(CodeSeverityConflict, sp, "severity "+s.ident(), "conflicting severities %s and %s", cur.sev.ident(), s.ident())
	d.Secondary = append(d.Secondary, rc.diag.label(cur.sp, "first set here"))
	d.Help = "an alert has one severity; set it once"
	return false
}

func (rc *ruleCompiler) clauses(clauses []clauseAST, r *Rule, ck *checker, sev severitySource) {
	seen := map[string]span{}
	for _, c := range clauses {
		if first, dup := seen[c.kind]; dup {
			d := rc.diag.add(CodeDuplicateClause, c.sp, "repeated clause", "duplicate `%s` clause", c.kind)
			d.Secondary = append(d.Secondary, rc.diag.label(first, "first given here"))
			continue
		}
		seen[c.kind] = c.sp
		switch c.kind {
		case "throttle", "digest":
			if c.duration <= 0 {
				rc.diag.add(CodeInvalidDuration, c.valSp, "must be positive", "`%s` needs a positive duration", c.kind)
				continue
			}
			if c.kind == "throttle" {
				r.Throttle = time.Duration(c.duration)
			} else {
				r.Digest = time.Duration(c.duration)
			}
		case "group_by":
			for _, e := range c.exprs {
				t, fn := ck.check(e)
				if t.k == kInvalid {
					continue
				}
				if !t.isScalar() {
					rc.diag.add(CodeGroupByType, e.exprSpan(), "has type "+t.String(), "group_by values must be scalars, found %s", t)
					continue
				}
				r.groups = append(r.groups, fn)
				r.GroupBy = append(r.GroupBy, trimmedText(rc.src, e.exprSpan()))
			}
		case "severity":
			if s, ok := rc.severity(c.ident, c.valSp); ok {
				rc.mergeSeverity(&sev, s, c.valSp)
			}
		case "quiet_hours":
			q, err := parseQuietHours(c.text, rc.opts.LoadLocation)
			switch {
			case errors.Is(err, ErrUnknownZone):
				rc.diag.add(CodeUnknownZone, c.valSp, "unknown zone", "%v", err).
					Help = "use an IANA zone name such as Asia/Kolkata or UTC"
			case err != nil:
				rc.diag.add(CodeInvalidQuiet, c.valSp, "invalid window", "%v", err).
					Help = "write \"HH:MM-HH:MM Zone\", for example \"22:00-07:00 Asia/Kolkata\""
			default:
				r.QuietHours = q
			}
		}
	}
	if sev.set {
		r.Severity = sev.sev
	}
}

// requiredEventTypes extracts the envelope types a condition can match,
// from top-level conjuncts of the form event.type == "x" or event.type in
// ["x", "y"] (and disjunctions of those). nil means "any type". The rule
// set uses it as an index; it never changes the result of evaluation.
func requiredEventTypes(e expr) []string {
	switch n := e.(type) {
	case *binaryExpr:
		switch n.op {
		case "&&":
			if t := requiredEventTypes(n.l); t != nil {
				return t
			}
			return requiredEventTypes(n.r)
		case "||":
			l, r := requiredEventTypes(n.l), requiredEventTypes(n.r)
			if l == nil || r == nil {
				return nil
			}
			return append(append([]string{}, l...), r...)
		case "==":
			if isEventTypePath(n.l) {
				if s, ok := stringLit(n.r); ok {
					return []string{s}
				}
			}
			if isEventTypePath(n.r) {
				if s, ok := stringLit(n.l); ok {
					return []string{s}
				}
			}
		case "in":
			if !isEventTypePath(n.l) {
				return nil
			}
			list, ok := n.r.(*listExpr)
			if !ok {
				return nil
			}
			out := []string{}
			for _, el := range list.elems {
				s, ok := stringLit(el)
				if !ok {
					return nil
				}
				out = append(out, s)
			}
			return out
		}
	}
	return nil
}

func isEventTypePath(e expr) bool {
	p, ok := e.(*pathExpr)
	return ok && len(p.parts) == 2 && p.parts[0] == "event" && p.parts[1] == "type"
}

func stringLit(e expr) (string, bool) {
	l, ok := e.(*litExpr)
	if !ok || l.val.k != kString {
		return "", false
	}
	return l.val.s, true
}
