package engine

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
)

// CheckWorkflowRequest is proto CheckWorkflowRequest.
type CheckWorkflowRequest struct {
	Name      string // optional; names the source file in diagnostics
	Source    string
	ProgramID core.UUID // zero: no policy check
}

// RegisterWorkflowRequest is proto RegisterWorkflowRequest.
type RegisterWorkflowRequest struct {
	Name      string
	Source    string
	ProgramID core.UUID
}

// CheckWorkflow runs the front end and, with a program, the policy check
// (spec 9.1). It returns diagnostics only; agent effects are reported as
// an error diagnostic because registration would reject them.
func (e *Engine) CheckWorkflow(ctx context.Context, c Caller, req CheckWorkflowRequest) (CheckResult, error) {
	creq, err := e.compileRequest(ctx, c, req.Name, req.Source, req.ProgramID)
	if err != nil {
		return CheckResult{}, err
	}
	res, err := e.d.Compiler.Check(ctx, creq)
	if err != nil {
		return CheckResult{}, errInternal("check workflow", err)
	}
	agent := slices.ContainsFunc(res.Effects, isAgentEffectName)
	var kinds []bytecode.EffectKind
	if !agent && !HasErrors(res.Diagnostics) {
		// The effect names of spec 7.5 do not distinguish agent calls (llm),
		// so look at the code the workflow compiles to.
		if cres, err := e.d.Compiler.Compile(ctx, creq); err == nil && len(cres.Module) > 0 {
			if m, err := bytecode.Decode(cres.Module); err == nil {
				kinds = agentEffects(m)
				agent = len(kinds) > 0
			}
		}
	}
	if agent {
		res.Diagnostics = append(res.Diagnostics, agentDiagnostic(fileName(req.Name), kinds))
	}
	res.OK = !HasErrors(res.Diagnostics)
	return res, nil
}

func isAgentEffectName(s string) bool { return s == "agent_call" || s == "execute_plan" }

func fileName(name string) string {
	if name == "" {
		return "workflow.ff"
	}
	return name + ".ff"
}

func agentDiagnostic(file string, kinds []bytecode.EffectKind) Diagnostic {
	what := "agent_call or execute_plan"
	if len(kinds) > 0 {
		names := make([]string, len(kinds))
		for i, k := range kinds {
			names[i] = k.String()
		}
		what = strings.Join(names, ", ")
	}
	return Diagnostic{
		Code: DiagAgentEffectsDisabled, Severity: "error",
		Message:   "agent effects not enabled: the workflow uses " + what,
		Primary:   SourceSpan{File: file, Start: SourcePos{Line: 1, Col: 1}, End: SourcePos{Line: 1, Col: 1}, Label: "agent step"},
		Secondary: []SourceSpan{},
		Help:      "Agent steps (spec 16) are not available yet; see docs/DECISIONS.md D1.",
	}
}

// compileRequest assembles the compiler input: the registered tools and,
// when a program is named, its current policy.
func (e *Engine) compileRequest(ctx context.Context, c Caller, name, source string, program core.UUID) (CompileRequest, error) {
	if strings.TrimSpace(source) == "" {
		return CompileRequest{}, errInvalid("source is empty")
	}
	tools, err := e.d.Tools.ListTools(ctx)
	if err != nil {
		return CompileRequest{}, errInternal("list tools", err)
	}
	req := CompileRequest{Name: name, Source: source, Tools: tools}
	if !program.IsZero() {
		pol, err := e.programPolicy(ctx, c.Tenant, program)
		if err != nil {
			return CompileRequest{}, err
		}
		req.Policy = pol
	}
	return req, nil
}

func (e *Engine) programPolicy(ctx context.Context, tenant, program core.UUID) (*ProgramPolicy, error) {
	ver, err := e.d.Scope.CurrentVersion(ctx, tenant, program)
	if err != nil {
		return nil, notFoundOr("program", "scope version", err)
	}
	p, err := e.d.Scope.Policy(ctx, tenant, program, ver)
	if err != nil {
		return nil, notFoundOr("program", "scope policy", err)
	}
	doc := p.Document()
	return &ProgramPolicy{ProgramID: program, ScopeVersion: ver, MaxClass: p.MaxClass(), Prohibited: doc.Activity.Prohibited}, nil
}

func validName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

func diagnosticsError(ds []Diagnostic) *core.Error {
	err := apiErr(core.KindInvalidArgument, core.ReasonDSLDiagnostics, "the workflow does not compile (%d diagnostics)", len(ds))
	for _, d := range ds {
		err.Details = append(err.Details, d)
	}
	return err
}

// RegisterWorkflow checks, compiles, verifies and publishes a new
// immutable version of a workflow (spec 9.1). Workflows that use agent
// effects are rejected (docs/DECISIONS.md D1).
func (e *Engine) RegisterWorkflow(ctx context.Context, c Caller, req RegisterWorkflowRequest) (WorkflowVersion, error) {
	if !validName(req.Name) {
		return WorkflowVersion{}, errInvalid("invalid workflow name %q", req.Name)
	}
	creq, err := e.compileRequest(ctx, c, req.Name, req.Source, req.ProgramID)
	if err != nil {
		return WorkflowVersion{}, err
	}
	res, err := e.d.Compiler.Compile(ctx, creq)
	if err != nil {
		return WorkflowVersion{}, errInternal("compile workflow", err)
	}
	if HasErrors(res.Diagnostics) || !res.OK || len(res.Module) == 0 {
		ds := res.Diagnostics
		if !HasErrors(ds) {
			ds = append(ds, Diagnostic{Code: "E0000", Severity: "error", Message: "the compiler produced no module", Secondary: []SourceSpan{}})
		}
		return WorkflowVersion{}, diagnosticsError(ds)
	}
	mod, err := bytecode.Decode(res.Module)
	if err != nil {
		return WorkflowVersion{}, errInternal("decode compiled module", err)
	}
	if kinds := agentEffects(mod); len(kinds) > 0 || slices.ContainsFunc(res.Effects, isAgentEffectName) {
		return WorkflowVersion{}, diagnosticsError(append(slices.Clone(res.Diagnostics), agentDiagnostic(fileName(req.Name), kinds)))
	}
	lm, err := e.loadModule(ctx, res.Module)
	if err != nil {
		return WorkflowVersion{}, errInvalid("module rejected: %v", err)
	}
	if _, ok := lm.prog.Module().Signature(lm.prog.Module().Entry); !ok {
		return WorkflowVersion{}, errInvalid("module rejected: the entry function has no signature")
	}
	if res.Attributes.MaxEffects < 0 || res.Attributes.MaxEffects > 1_000_000 {
		return WorkflowVersion{}, errInvalid("#[effects(max = %d)] is out of range", res.Attributes.MaxEffects)
	}
	effects := res.Effects
	if len(effects) == 0 {
		effects = maskNames(lm.prog.InferredEffects())
	}
	class := res.Class
	for _, name := range effects {
		class = max(class, activityClass(name))
	}
	v := WorkflowVersion{
		ID: e.ids.New(), Tenant: c.Tenant, Name: req.Name, Status: VersionPublished,
		Source: req.Source, SourceSHA256: sha256.Sum256([]byte(req.Source)),
		Module: res.Module, ModuleSHA256: lm.prog.Hash(), Effects: effects, InferredClass: class,
		Diagnostics: res.Diagnostics, Attributes: workflowAttributes(lm.prog.Module(), res.Attributes), ProgramID: req.ProgramID,
		CreatedBy: c.Actor, CreatedAt: e.now(),
	}
	if v.Diagnostics == nil {
		v.Diagnostics = []Diagnostic{}
	}
	stored, err := e.d.Repo.CreateWorkflowVersion(ctx, v)
	if err != nil {
		return WorkflowVersion{}, errInternal("store workflow version", err)
	}
	return stored, nil
}

// maskNames lists the names of the effect bits in m.
func maskNames(m bytecode.EffectMask) []string {
	var out []string
	for _, n := range []string{"read", "passive", "active", "intrusive", "write", "financial_write", "llm", "time", "scope", "approval", "alert", "plan"} {
		if bit, ok := bytecode.ParseEffect(n); ok && m&bit != 0 {
			out = append(out, n)
		}
	}
	return out
}

// GetWorkflowVersion returns version n of a workflow (0: latest
// published).
func (e *Engine) GetWorkflowVersion(ctx context.Context, c Caller, name string, n int32) (WorkflowVersion, error) {
	v, err := e.d.Repo.GetWorkflowVersion(ctx, c.Tenant, name, n)
	if err != nil {
		return WorkflowVersion{}, notFoundOr("workflow version", "get workflow version", err)
	}
	return v, nil
}

// ListWorkflows returns the latest version of each workflow, ordered by
// name (proto ListWorkflows).
func (e *Engine) ListWorkflows(ctx context.Context, c Caller, p Page) ([]WorkflowVersion, string, error) {
	var tok struct {
		Name string `json:"name"`
	}
	if err := decodeToken(p.Token, &tok); err != nil {
		return nil, "", err
	}
	size := pageSize(p.Size)
	vs, err := e.d.Repo.ListWorkflows(ctx, c.Tenant, tok.Name, size+1)
	if err != nil {
		return nil, "", errInternal("list workflows", err)
	}
	next := ""
	if len(vs) > size {
		vs = vs[:size]
		tok.Name = vs[size-1].Name
		next = encodeToken(tok)
	}
	return vs, next, nil
}

// DeprecateWorkflowVersion blocks new runs on a version; existing runs
// continue (spec 9.1).
func (e *Engine) DeprecateWorkflowVersion(ctx context.Context, c Caller, name string, n int32) (WorkflowVersion, error) {
	if n <= 0 {
		return WorkflowVersion{}, errInvalid("a version number is required")
	}
	v, err := e.d.Repo.SetWorkflowVersionStatus(ctx, c.Tenant, name, n, VersionDeprecated)
	if err != nil {
		return WorkflowVersion{}, notFoundOr("workflow version", "deprecate workflow version", err)
	}
	return v, nil
}

// ---- pagination ----

const (
	defaultPageSize = 50
	maxPageSize     = 500
)

func pageSize(n int) int {
	switch {
	case n <= 0:
		return defaultPageSize
	case n > maxPageSize:
		return maxPageSize
	}
	return n
}

func encodeToken(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeToken(tok string, v any) error {
	if tok == "" {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err == nil {
		err = json.Unmarshal(b, v)
	}
	if err != nil {
		return errInvalid("invalid page token")
	}
	return nil
}
