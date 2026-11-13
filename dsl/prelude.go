package dsl

import (
	"fmt"
	"strings"
)

// The prelude is the standard library visible to every program: builtin
// types (spec 7.4) and functions (spec 7.6, 22.9). It is built once at
// package initialization and never mutated afterwards, so concurrent
// checks can share it.

// Builtin type constructors. Their TypeDef IDs are their prelude index.
var (
	defList    = opaque("List", "T")
	defMap     = opaque("Map", "K", "V")
	defStream  = opaque("Stream", "T")
	defOption  = &TypeDef{Name: "Option", Kind: DefEnum, Builtin: true}
	defResult  = &TypeDef{Name: "Result", Kind: DefEnum, Builtin: true}
	defInScope = opaque("InScope", "T")
	defPlan    = opaque("Plan", "T")
	defScope   = opaque("Scope")
	defEntity  = opaque("Entity")
	defVector  = opaque("Vector")

	defFqdn      = scalar("Fqdn")
	defIPAddress = scalar("IpAddress")
	defNetblock  = scalar("Netblock")
	defAsn       = scalar("Asn")
	defURL       = scalar("Url")

	defHost     = &TypeDef{Name: "Host", Kind: DefEnum, Builtin: true, Display: true}
	defSeverity = &TypeDef{Name: "Severity", Kind: DefEnum, Builtin: true, Display: true}

	defService        = &TypeDef{Name: "Service", Kind: DefRecord, Builtin: true}
	defTLSCertificate = &TypeDef{Name: "TlsCertificate", Kind: DefRecord, Builtin: true}
	defProduct        = &TypeDef{Name: "Product", Kind: DefRecord, Builtin: true}
	defOrganization   = &TypeDef{Name: "Organization", Kind: DefRecord, Builtin: true}
	defScopeDenied    = &TypeDef{Name: "ScopeDenied", Kind: DefRecord, Builtin: true}
	defBudget         = &TypeDef{Name: "Budget", Kind: DefRecord, Builtin: true}
	defApproval       = &TypeDef{Name: "Approval", Kind: DefRecord, Builtin: true}
	defDenied         = &TypeDef{Name: "Denied", Kind: DefRecord, Builtin: true}
	defRunInfo        = &TypeDef{Name: "RunInfo", Kind: DefRecord, Builtin: true}
	defPlanError      = &TypeDef{Name: "PlanError", Kind: DefRecord, Builtin: true}
	defGraphSummary   = &TypeDef{Name: "GraphSummary", Kind: DefRecord, Builtin: true}
	// defTaskError is the error of a par_map element with on_error =
	// "collect" (spec 8.6); appended last so earlier IDs stay stable.
	defTaskError = &TypeDef{Name: "TaskError", Kind: DefRecord, Builtin: true}
)

// builtinTypeDefs lists every builtin TypeDef in ID order.
var builtinTypeDefs = []*TypeDef{
	defList, defMap, defStream, defOption, defResult, defInScope, defPlan,
	defScope, defEntity, defVector, defFqdn, defIPAddress, defNetblock, defAsn,
	defURL, defHost, defSeverity, defService, defTLSCertificate, defProduct,
	defOrganization, defScopeDenied, defBudget, defApproval, defDenied,
	defRunInfo, defPlanError, defGraphSummary, defTaskError,
}

// scopeTargets are the types scope.check and scope.allows accept.
var scopeTargets = []*TypeDef{defFqdn, defIPAddress, defNetblock, defAsn, defURL, defHost, defEntity}

func opaque(name string, params ...string) *TypeDef {
	d := &TypeDef{Name: name, Kind: DefOpaque, Builtin: true}
	for i, p := range params {
		d.Params = append(d.Params, &TypeParam{Name: p, ID: -1 - i})
	}
	return d
}

func scalar(name string) *TypeDef {
	return &TypeDef{Name: name, Kind: DefScalar, Builtin: true, Display: true}
}

// BuiltinParam is one parameter of a builtin signature.
type BuiltinParam struct {
	Name     string
	Type     Type
	Optional bool
}

// Signature is one overload of a builtin function.
type Signature struct {
	Generics []*TypeParam
	Params   []BuiltinParam
	Result   Type
}

// Builtin is a standard-library function. ID is stable for a given
// version of this package and is the operand of the BUILTIN instruction
// for pure builtins. Op is the EFFECT kind for effectful builtins
// (OpNone for pure ones).
type Builtin struct {
	ID        int
	Name      string // "map", "scope.check", ...
	Overloads []*Signature
	Effects   EffectSet
	Op        OpKind
	// HigherOrder marks builtins whose function arguments are called
	// during the call, so their latent effects join the call's effects.
	HigherOrder bool
	special     builtinSpecial
}

type builtinSpecial int

const (
	specialNone builtinSpecial = iota
	specialFlatMap
	specialScopeTarget
	specialDisplay
)

// builtinSpec is the textual source of a builtin. sig uses the type syntax
// of the language; "Seq" expands to List and Stream overloads; a trailing
// `?` on a parameter name marks it optional.
type builtinSpec struct {
	sigs    []string
	effects EffectSet
	op      OpKind
	hof     bool
	special builtinSpecial
}

func seq(sig string) []string {
	return []string{strings.ReplaceAll(sig, "Seq", "List"), strings.ReplaceAll(sig, "Seq", "Stream")}
}

func cat(groups ...[]string) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

// builtinOrder fixes builtin IDs; append only.
var builtinOrder = []struct {
	name string
	spec builtinSpec
}{
	{"map", builtinSpec{sigs: seq("<A, B>(xs: Seq<A>, f: (A) -> B) -> Seq<B>"), hof: true}},
	{"filter", builtinSpec{sigs: seq("<A>(xs: Seq<A>, f: (A) -> Bool) -> Seq<A>"), hof: true}},
	{"filter_map", builtinSpec{sigs: seq("<A, B>(xs: Seq<A>, f: (A) -> Option<B>) -> Seq<B>"), hof: true}},
	{"flat_map", builtinSpec{sigs: seq("<A, R>(xs: Seq<A>, f: (A) -> R) -> R"), hof: true, special: specialFlatMap}},
	{"par_map", builtinSpec{sigs: seq("<A, B>(xs: Seq<A>, f: (A) -> B, limit: Int, on_error?: String) -> Seq<B>"), hof: true, op: OpParMap}},
	{"fold", builtinSpec{sigs: seq("<A, B>(xs: Seq<A>, init: B, f: (B, A) -> B) -> B"), hof: true}},
	{"any", builtinSpec{sigs: seq("<A>(xs: Seq<A>, f: (A) -> Bool) -> Bool"), hof: true}},
	{"all", builtinSpec{sigs: seq("<A>(xs: Seq<A>, f: (A) -> Bool) -> Bool"), hof: true}},
	{"first", builtinSpec{sigs: seq("<A>(xs: Seq<A>, f?: (A) -> Bool) -> Option<A>"), hof: true}},
	{"count", builtinSpec{sigs: seq("<A>(xs: Seq<A>, f?: (A) -> Bool) -> Int"), hof: true}},
	{"len", builtinSpec{sigs: cat(seq("<A>(xs: Seq<A>) -> Int"), []string{"(s: String) -> Int", "<K, V>(m: Map<K, V>) -> Int"})}},
	{"take", builtinSpec{sigs: seq("<A>(xs: Seq<A>, n: Int) -> Seq<A>")}},
	{"skip", builtinSpec{sigs: seq("<A>(xs: Seq<A>, n: Int) -> Seq<A>")}},
	{"sort_by", builtinSpec{sigs: seq("<A, K>(xs: Seq<A>, f: (A) -> K) -> Seq<A>"), hof: true}},
	{"group_by", builtinSpec{sigs: seq("<A, K>(xs: Seq<A>, f: (A) -> K) -> Map<K, List<A>>"), hof: true}},
	{"dedupe", builtinSpec{sigs: seq("<A>(xs: Seq<A>) -> Seq<A>")}},
	{"zip", builtinSpec{sigs: seq("<A, B>(xs: Seq<A>, ys: Seq<B>) -> Seq<(A, B)>")}},
	{"enumerate", builtinSpec{sigs: seq("<A>(xs: Seq<A>) -> Seq<(Int, A)>")}},
	{"reverse", builtinSpec{sigs: seq("<A>(xs: Seq<A>) -> Seq<A>")}},
	{"concat", builtinSpec{sigs: cat(seq("<A>(xs: Seq<A>, ys: Seq<A>) -> Seq<A>"), []string{"(a: String, b: String) -> String"})}},
	{"contains", builtinSpec{sigs: cat(seq("<A>(xs: Seq<A>, x: A) -> Bool"), []string{"(s: String, sub: String) -> Bool", "<K, V>(m: Map<K, V>, k: K) -> Bool"})}},
	{"starts_with", builtinSpec{sigs: []string{"(s: String, prefix: String) -> Bool"}}},
	{"ends_with", builtinSpec{sigs: []string{"(s: String, suffix: String) -> Bool"}}},
	{"split", builtinSpec{sigs: []string{"(s: String, sep: String) -> List<String>"}}},
	{"join", builtinSpec{sigs: seq("(xs: Seq<String>, sep: String) -> String")}},
	{"trim", builtinSpec{sigs: []string{"(s: String) -> String"}}},
	{"lower", builtinSpec{sigs: []string{"(s: String) -> String"}}},
	{"upper", builtinSpec{sigs: []string{"(s: String) -> String"}}},
	{"replace", builtinSpec{sigs: []string{"(s: String, from: String, to: String) -> String"}}},
	{"parse_int", builtinSpec{sigs: []string{"(s: String) -> Option<Int>"}}},
	{"parse_float", builtinSpec{sigs: []string{"(s: String) -> Option<Float>"}}},
	{"to_string", builtinSpec{sigs: []string{"<A>(x: A) -> String"}, special: specialDisplay}},
	{"get", builtinSpec{sigs: []string{"<K, V>(m: Map<K, V>, k: K) -> Option<V>"}}},
	{"keys", builtinSpec{sigs: []string{"<K, V>(m: Map<K, V>) -> List<K>"}}},
	{"values", builtinSpec{sigs: []string{"<K, V>(m: Map<K, V>) -> List<V>"}}},
	{"scope.allows", builtinSpec{sigs: []string{"<T>(x: T) -> Bool"}, special: specialScopeTarget}},
	{"scope.check", builtinSpec{sigs: []string{"<T>(x: T) -> Result<InScope<T>, ScopeDenied>"}, op: OpScopeCheck, special: specialScopeTarget}},
	{"similar", builtinSpec{sigs: []string{"(e: Entity, k: Int) -> List<Entity>"}, effects: EffectSet(EffRead), op: OpSimilarityQuery}},
	{"embed", builtinSpec{sigs: []string{"(text: String) -> Vector"}, effects: EffectSet(EffLLM), op: OpEmbed}},
	{"approve", builtinSpec{sigs: []string{"(role: String, reason: String, timeout?: Duration) -> Result<Approval, Denied>"}, op: OpApproval}},
	{"sleep", builtinSpec{sigs: []string{"(d: Duration) -> ()"}, effects: EffectSet(EffTime), op: OpSleep}},
	{"now", builtinSpec{sigs: []string{"() -> Time"}, effects: EffectSet(EffTime), op: OpNow}},
	{"emit_alert", builtinSpec{sigs: []string{"(sev: Severity, msg: String) -> ()"}, op: OpEmitAlert}},
	{"execute", builtinSpec{sigs: []string{"<T>(plan: Plan<T>) -> Result<T, PlanError>"}, op: OpExecutePlan}},
	{"log", builtinSpec{sigs: []string{"(level: String, msg: String) -> ()"}, op: OpLog}},
}

// preludeTables holds the builtin namespaces (the prelude variable is the
// one instance).
type preludeTables struct {
	types    map[string]*TypeDef
	builtins map[string]*Builtin
	list     []*Builtin
	// variants are the unqualified prelude constructors Some, None, Ok, Err.
	variants map[string]*Variant
	// approve is the builtin `require approval` desugars to.
	approve *Builtin
}

var prelude = buildPrelude()

// Builtins returns the standard-library functions in ID order. The
// returned slice must not be modified.
func Builtins() []*Builtin { return prelude.list }

// BuiltinTypes returns the builtin type definitions in ID order. The
// returned slice must not be modified.
func BuiltinTypes() []*TypeDef { return builtinTypeDefs }

func buildPrelude() *preludeTables {
	pt := &preludeTables{types: map[string]*TypeDef{}, builtins: map[string]*Builtin{}, variants: map[string]*Variant{}}
	for i, d := range builtinTypeDefs {
		d.ID = i
		pt.types[d.Name] = d
	}
	tp := func(name string) *TypeParam { return &TypeParam{Name: name, ID: -100} }
	T, E := tp("T"), tp("E")
	defOption.Params = []*TypeParam{T}
	defOption.Variants = []*Variant{
		{Def: defOption, Name: "Some", Index: 0, Payload: []Type{T}},
		{Def: defOption, Name: "None", Index: 1},
	}
	defResult.Params = []*TypeParam{T, E}
	defResult.Variants = []*Variant{
		{Def: defResult, Name: "Ok", Index: 0, Payload: []Type{T}},
		{Def: defResult, Name: "Err", Index: 1, Payload: []Type{E}},
	}
	for _, v := range append(append([]*Variant{}, defOption.Variants...), defResult.Variants...) {
		pt.variants[v.Name] = v
	}
	fq := &NamedType{Def: defFqdn}
	ip := &NamedType{Def: defIPAddress}
	defHost.Variants = []*Variant{
		{Def: defHost, Name: "Name", Index: 0, Payload: []Type{fq}},
		{Def: defHost, Name: "Ip", Index: 1, Payload: []Type{ip}},
	}
	for i, n := range []string{"Info", "Low", "Medium", "High", "Critical"} {
		defSeverity.Variants = append(defSeverity.Variants, &Variant{Def: defSeverity, Name: n, Index: i})
	}
	optStr := &NamedType{Def: defOption, Args: []Type{TString}}
	fields := func(d *TypeDef, fs ...any) {
		for i := 0; i+1 < len(fs); i += 2 {
			d.Fields = append(d.Fields, &Field{Name: fs[i].(string), Type: fs[i+1].(Type), Index: i / 2})
		}
	}
	fields(defService, "unique_id", TString, "service_type", TString, "output", TString, "output_length", TInt)
	fields(defTLSCertificate, "version", TString, "serial_number", TString, "subject_common_name", TString,
		"issuer_common_name", TString, "not_before", TTime, "not_after", TTime, "is_ca", TBool)
	fields(defProduct, "name", TString, "version", optStr, "vendor", optStr)
	fields(defOrganization, "unique_id", TString, "name", TString, "legal_name", optStr)
	fields(defScopeDenied, "target", TString, "reason", TString, "rule_id", optStr, "decision_id", TString)
	fields(defBudget, "tokens", TInt, "tool_calls", TInt, "iterations", TInt)
	fields(defApproval, "id", TString, "role", TString, "decided_by", TString, "decided_at", TTime)
	fields(defDenied, "id", TString, "role", TString, "reason", TString)
	fields(defRunInfo, "run_id", TString, "workflow", TString, "version", TInt, "started_at", TTime)
	fields(defPlanError, "code", TString, "message", TString)
	fields(defTaskError, "code", TString, "message", TString)
	fields(defGraphSummary, "entities_by_type", &NamedType{Def: defMap, Args: []Type{TString, TInt}},
		"new_since_last_run", TInt, "top_entities", &NamedType{Def: defList, Args: []Type{&NamedType{Def: defEntity}}})

	for id, b := range builtinOrder {
		bi := &Builtin{ID: id, Name: b.name, Effects: b.spec.effects, Op: b.spec.op,
			HigherOrder: b.spec.hof, special: b.spec.special}
		for _, s := range b.spec.sigs {
			sig, err := parseSignature(s, pt.types)
			if err != nil {
				// The prelude is fixed source text; a failure is a bug in
				// this package, not bad input.
				panic(fmt.Sprintf("dsl: prelude signature %s%s: %v", b.name, s, err))
			}
			bi.Overloads = append(bi.Overloads, sig)
		}
		pt.builtins[b.name] = bi
		pt.list = append(pt.list, bi)
	}
	pt.approve = pt.builtins["approve"]
	return pt
}

// parseSignature parses "<A, B>(name: Type, opt?: Type) -> Type".
func parseSignature(src string, types map[string]*TypeDef) (*Signature, error) {
	toks, diags := Lex("<prelude>", []byte(src))
	if len(diags) > 0 {
		return nil, fmt.Errorf("%s", diags[0].Message)
	}
	sink := newDiagSink("<prelude>")
	p := &parser{toks: toks, sink: sink}
	sig := &Signature{}
	gen := map[string]*TypeParam{}
	for i, g := range p.parseGenerics() {
		tpar := &TypeParam{Name: g.Name, ID: -1000 - i}
		sig.Generics = append(sig.Generics, tpar)
		gen[g.Name] = tpar
	}
	p.expect(TokLParen)
	for !p.at(TokRParen) && !p.at(TokEOF) {
		name := p.parseIdent("parameter")
		if name == nil {
			break
		}
		bp := BuiltinParam{Name: name.Name}
		if p.at(TokQuestion) {
			p.advance()
			bp.Optional = true
		}
		p.expect(TokColon)
		tx := p.parseType()
		t, err := preludeType(tx, types, gen)
		if err != nil {
			return nil, err
		}
		if ft, ok := t.(*FuncType); ok {
			ft.Open = true
		}
		bp.Type = t
		sig.Params = append(sig.Params, bp)
		if !p.at(TokComma) {
			break
		}
		p.advance()
	}
	p.expect(TokRParen)
	p.expect(TokArrow)
	rt, err := preludeType(p.parseType(), types, gen)
	if err != nil {
		return nil, err
	}
	sig.Result = rt
	if len(sink.diags) > 0 || !p.at(TokEOF) {
		return nil, fmt.Errorf("syntax error in signature")
	}
	return sig, nil
}

func preludeType(tx TypeExpr, types map[string]*TypeDef, gen map[string]*TypeParam) (Type, error) {
	switch t := tx.(type) {
	case *NamedTypeExpr:
		if g, ok := gen[t.Name.Name]; ok {
			return g, nil
		}
		if p := primByName(t.Name.Name); p != nil {
			return p, nil
		}
		d, ok := types[t.Name.Name]
		if !ok {
			return nil, fmt.Errorf("unknown type %s", t.Name.Name)
		}
		nt := &NamedType{Def: d}
		for _, a := range t.Args {
			at, err := preludeType(a, types, gen)
			if err != nil {
				return nil, err
			}
			nt.Args = append(nt.Args, at)
		}
		return nt, nil
	case *TupleTypeExpr:
		if len(t.Elems) == 0 {
			return TUnit, nil
		}
		tt := &TupleType{}
		for _, e := range t.Elems {
			et, err := preludeType(e, types, gen)
			if err != nil {
				return nil, err
			}
			tt.Elems = append(tt.Elems, et)
		}
		return tt, nil
	case *FuncTypeExpr:
		ft := &FuncType{}
		for _, e := range t.Params {
			et, err := preludeType(e, types, gen)
			if err != nil {
				return nil, err
			}
			ft.Params = append(ft.Params, et)
		}
		rt, err := preludeType(t.Result, types, gen)
		if err != nil {
			return nil, err
		}
		ft.Result = rt
		return ft, nil
	}
	return nil, fmt.Errorf("bad type")
}

// primByName returns the primitive type with the given name, or nil.
func primByName(name string) *PrimType {
	switch name {
	case "Int":
		return TInt
	case "Float":
		return TFloat
	case "Bool":
		return TBool
	case "String":
		return TString
	case "Duration":
		return TDuration
	case "Time":
		return TTime
	case "Unit":
		return TUnit
	}
	return nil
}
