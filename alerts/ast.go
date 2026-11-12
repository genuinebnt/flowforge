package alerts

// expr is a node of the condition / group_by expression tree. The checker
// fills in typ; the evaluator compiles the checked tree into closures.
type expr interface {
	exprSpan() span
}

// litExpr is a literal: string, integer, float, duration or boolean.
type litExpr struct {
	sp  span
	val value
}

// pathExpr is a dotted variable path such as entity.name.
type pathExpr struct {
	sp    span
	parts []string
	spans []span
}

// unaryExpr is -x or !x.
type unaryExpr struct {
	sp span
	op string
	x  expr
}

// binaryExpr is any infix operator, including the word operators
// (contains, starts_with, ends_with, matches) and in.
type binaryExpr struct {
	sp   span
	op   string
	opSp span
	l, r expr
}

// callExpr is a built-in function call.
type callExpr struct {
	sp     span
	name   string
	nameSp span
	args   []expr
}

// listExpr is a list literal [a, b, c].
type listExpr struct {
	sp    span
	elems []expr
}

// badExpr stands in for an invalid literal the lexer already reported.
type badExpr struct {
	sp span
}

// rangeExpr is a half-open integer range lo..hi, valid only after in.
type rangeExpr struct {
	sp     span
	lo, hi expr
}

func (e *litExpr) exprSpan() span    { return e.sp }
func (e *pathExpr) exprSpan() span   { return e.sp }
func (e *unaryExpr) exprSpan() span  { return e.sp }
func (e *binaryExpr) exprSpan() span { return e.sp }
func (e *callExpr) exprSpan() span   { return e.sp }
func (e *listExpr) exprSpan() span   { return e.sp }
func (e *rangeExpr) exprSpan() span  { return e.sp }
func (e *badExpr) exprSpan() span    { return e.sp }

// ruleAST is the parsed statement form of spec 15.2.
type ruleAST struct {
	name    string
	nameSp  span
	cond    expr
	actions []actionAST
	clauses []clauseAST
}

// actionAST is one action such as slack("sec-recon", severity = High).
type actionAST struct {
	name   string
	nameSp span
	sp     span
	args   []argAST
}

// argAST is a positional or named action argument.
type argAST struct {
	name   string // empty for positional arguments
	nameSp span
	val    expr
}

// clauseAST is one trailing clause. Only the fields of its kind are set.
type clauseAST struct {
	kind     string // throttle, group_by, digest, severity, quiet_hours
	sp       span   // the clause keyword
	duration int64  // throttle, digest (nanoseconds)
	valSp    span   // span of the clause value
	exprs    []expr // group_by
	ident    string // severity
	text     string // quiet_hours
}
