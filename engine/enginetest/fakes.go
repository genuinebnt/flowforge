package enginetest

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
)

// ToolHandler answers one tool call.
type ToolHandler func(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error)

// FakeToolExec is the in-memory engine.ToolExecutor (spec 24.3
// FakeToolExec): per-tool handlers, and a record of every call with its
// idempotency key. It is safe for concurrent use.
type FakeToolExec struct {
	mu       sync.Mutex
	handlers map[string]ToolHandler
	calls    []engine.ToolCall
}

var _ engine.ToolExecutor = (*FakeToolExec)(nil)

// NewFakeToolExec returns an executor without handlers (calls to unknown
// tools fail with a permanent error).
func NewFakeToolExec() *FakeToolExec { return &FakeToolExec{handlers: map[string]ToolHandler{}} }

// Handle sets the handler of a tool.
func (f *FakeToolExec) Handle(tool string, h ToolHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[tool] = h
}

// Execute implements engine.ToolExecutor.
func (f *FakeToolExec) Execute(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, call)
	h := f.handlers[call.Tool]
	f.mu.Unlock()
	if h == nil {
		return engine.ToolResult{}, &engine.ToolError{Code: "unknown_tool", Message: call.Tool}
	}
	return h(ctx, call)
}

// Calls returns the calls so far.
func (f *FakeToolExec) Calls() []engine.ToolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// CallsTo returns the calls of one tool.
func (f *FakeToolExec) CallsTo(tool string) []engine.ToolCall {
	var out []engine.ToolCall
	for _, c := range f.Calls() {
		if c.Tool == tool {
			out = append(out, c)
		}
	}
	return out
}

// Output returns a handler that always answers with the JSON of v.
func Output(v any) ToolHandler {
	b, err := json.Marshal(v)
	return func(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
		if err != nil {
			return engine.ToolResult{}, err
		}
		return engine.ToolResult{Output: b}, nil
	}
}

// Echo returns a handler that answers with its input.
func Echo() ToolHandler {
	return func(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
		return engine.ToolResult{Output: call.Input}, nil
	}
}

// MemIngester is the in-memory engine.Ingester. It counts the entities
// and edges of each envelope and de-duplicates by observation id.
type MemIngester struct {
	mu   sync.Mutex
	seen map[string]bool
	// Requests records every Ingest call.
	Requests []engine.IngestRequest
	// Fail makes Ingest fail.
	Fail error
}

// NewMemIngester returns an empty ingester.
func NewMemIngester() *MemIngester { return &MemIngester{seen: map[string]bool{}} }

// Ingest implements engine.Ingester.
func (m *MemIngester) Ingest(ctx context.Context, req engine.IngestRequest) ([]engine.IngestResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	m.Requests = append(m.Requests, req)
	out := make([]engine.IngestResult, 0, len(req.Observations))
	for _, raw := range req.Observations {
		var o struct {
			ID       string            `json:"observation_id"`
			Entities []json.RawMessage `json:"entities"`
			Edges    []json.RawMessage `json:"edges"`
		}
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, err
		}
		m.seen[o.ID] = true
		out = append(out, engine.IngestResult{ObservationID: o.ID, EntityCount: len(o.Entities), EdgeCount: len(o.Edges)})
	}
	return out, nil
}

// Unique returns the number of distinct observation ids ingested.
func (m *MemIngester) Unique() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.seen)
}

// MemPublisher is the in-memory outbox: it keeps envelopes in publication
// order, dropping repeated event ids.
type MemPublisher struct {
	mu   sync.Mutex
	seen map[core.UUID]bool
	envs []engine.Envelope
}

// NewMemPublisher returns an empty publisher.
func NewMemPublisher() *MemPublisher { return &MemPublisher{seen: map[core.UUID]bool{}} }

// Publish implements engine.Publisher.
func (p *MemPublisher) Publish(ctx context.Context, envs []engine.Envelope) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range envs {
		if !p.seen[e.EventID] {
			p.seen[e.EventID] = true
			p.envs = append(p.envs, e)
		}
	}
	return nil
}

// Envelopes returns the envelopes of a topic ("" for all).
func (p *MemPublisher) Envelopes(topic string) []engine.Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []engine.Envelope
	for _, e := range p.envs {
		if topic == "" || e.Topic == topic {
			out = append(out, e)
		}
	}
	return out
}

// Program is a hand-assembled workflow the FakeCompiler returns for a
// source text.
type Program struct {
	// Build assembles the module. It is given the registered tools so it
	// can pin their spec hashes in the TOOLS table.
	Build func(tools map[string]engine.ToolSpec) (*bytecode.Module, error)
	// Diagnostics are returned with the module (warnings) or instead of
	// it (errors).
	Diagnostics []engine.Diagnostic
	// Effects and Class override the values derived from the module.
	Effects    []string
	Class      core.ActivityClass
	Attributes engine.WorkflowAttributes
}

// FakeCompiler is the engine.Compiler double: it maps source texts to
// hand-assembled modules. Unknown sources produce an E0101 diagnostic.
type FakeCompiler struct {
	mu       sync.Mutex
	programs map[string]Program
	// Requests records every Check and Compile request.
	Requests []engine.CompileRequest
}

var _ engine.Compiler = (*FakeCompiler)(nil)

// NewFakeCompiler returns a compiler without programs.
func NewFakeCompiler() *FakeCompiler { return &FakeCompiler{programs: map[string]Program{}} }

// Add maps source to p.
func (c *FakeCompiler) Add(source string, p Program) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.programs[source] = p
}

// Check implements engine.Compiler.
func (c *FakeCompiler) Check(ctx context.Context, req engine.CompileRequest) (engine.CheckResult, error) {
	res, err := c.Compile(ctx, req)
	return res.CheckResult, err
}

// Compile implements engine.Compiler.
func (c *FakeCompiler) Compile(ctx context.Context, req engine.CompileRequest) (engine.CompileResult, error) {
	c.mu.Lock()
	c.Requests = append(c.Requests, req)
	p, ok := c.programs[req.Source]
	c.mu.Unlock()
	file := req.Name + ".ff"
	if !ok {
		d := engine.Diagnostic{Code: "E0101", Severity: "error", Message: "unexpected token",
			Primary:   engine.SourceSpan{File: file, Start: engine.SourcePos{Line: 1, Col: 1}, End: engine.SourcePos{Line: 1, Col: 2}, Label: "here"},
			Secondary: []engine.SourceSpan{}}
		return engine.CompileResult{CheckResult: engine.CheckResult{Diagnostics: []engine.Diagnostic{d}}}, nil
	}
	res := engine.CompileResult{CheckResult: engine.CheckResult{Diagnostics: p.Diagnostics, Effects: p.Effects, Class: p.Class}, Attributes: p.Attributes}
	if engine.HasErrors(p.Diagnostics) {
		return res, nil
	}
	tools := map[string]engine.ToolSpec{}
	for _, t := range req.Tools {
		tools[t.Name] = t
	}
	m, err := p.Build(tools)
	if err != nil {
		return engine.CompileResult{}, fmt.Errorf("enginetest: build %s: %w", req.Name, err)
	}
	if res.Effects == nil {
		res.Effects = maskEffects(m, tools)
	}
	for _, e := range res.Effects {
		if cl, err := core.ParseActivityClass(e); err == nil && cl > res.Class {
			res.Class = cl
		}
	}
	if req.Policy != nil && res.Class > req.Policy.MaxClass {
		res.Diagnostics = append(res.Diagnostics, engine.Diagnostic{Code: "E0402", Severity: "error",
			Message: "effect exceeds the program's max_class", Secondary: []engine.SourceSpan{},
			Primary: engine.SourceSpan{File: file, Start: engine.SourcePos{Line: 1, Col: 1}, End: engine.SourcePos{Line: 1, Col: 1}}})
		return res, nil
	}
	b, err := m.Encode()
	if err != nil {
		return engine.CompileResult{}, err
	}
	res.Module, res.OK = b, true
	return res, nil
}

// maskEffects lists the effect names of the module's declared mask plus
// the classes of the tools it pins.
func maskEffects(m *bytecode.Module, tools map[string]engine.ToolSpec) []string {
	mask := m.Effects
	for i := range m.Tools {
		if t, ok := tools[m.ToolName(i)]; ok {
			if bit, ok := bytecode.ParseEffect(t.EffectClass); ok {
				mask |= bit
			}
		}
	}
	var out []string
	for _, n := range []string{"read", "passive", "active", "intrusive", "write", "financial_write", "llm", "time", "scope", "approval", "alert", "plan"} {
		if bit, _ := bytecode.ParseEffect(n); mask&bit != 0 {
			out = append(out, n)
		}
	}
	return out
}
