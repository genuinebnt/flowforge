package enginetest

import (
	"context"
	"fmt"
	"time"

	bc "github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/engine"
)

// Tool specs used by the test workflows.
var (
	// SubfinderSpec enumerates subdomains (passive).
	SubfinderSpec = engine.ToolSpec{Name: "subfinder", Version: "2.6.0", InputType: "Fqdn", OutputType: "List<Fqdn>",
		EffectClass: "passive", Idempotent: true}
	// HttpxSpec probes a host (active, needs a scope proof).
	HttpxSpec = engine.ToolSpec{Name: "httpx", Version: "1.6.9", InputType: "InScope<Host>", OutputType: "HttpService",
		Observes: []string{"URL", "Service"}, EffectClass: "active", Idempotent: true, TimeoutMs: 300000,
		Retry: engine.RetryPolicy{MaxAttempts: 3, RetryOn: []string{"unavailable", "deadline_exceeded"},
			Backoff: engine.Backoff{InitialMs: 1000, MaxMs: 30000, Multiplier: 2, Jitter: "full"}}}
	// EchoSpec returns its input (read, idempotent).
	EchoSpec = engine.ToolSpec{Name: "echo", Version: "1", InputType: "Int", OutputType: "Int", EffectClass: "read", Idempotent: true}
	// FlakySpec is a retried tool (read, not idempotent).
	FlakySpec = engine.ToolSpec{Name: "flaky", Version: "1", InputType: "Int", OutputType: "Int", EffectClass: "read",
		Retry: engine.RetryPolicy{MaxAttempts: 4, Backoff: engine.Backoff{InitialMs: 100, MaxMs: 1000, Multiplier: 2, Jitter: "full"}}}
	// PostSpec posts a ledger entry (financial_write, not idempotent).
	PostSpec = engine.ToolSpec{Name: "post", Version: "1", InputType: "Int", OutputType: "String", EffectClass: "financial_write"}
	// UndoSpec reverses a ledger entry (financial_write, idempotent).
	UndoSpec = engine.ToolSpec{Name: "undo", Version: "1", InputType: "String", OutputType: "Unit", EffectClass: "financial_write", Idempotent: true}
	// BlockSpec is a slow step (read, idempotent).
	BlockSpec = engine.ToolSpec{Name: "block", Version: "1", InputType: "Int", OutputType: "Int", EffectClass: "read", Idempotent: true}
)

// AllSpecs lists every test tool.
func AllSpecs() []engine.ToolSpec {
	return []engine.ToolSpec{SubfinderSpec, HttpxSpec, EchoSpec, FlakySpec, PostSpec, UndoSpec, BlockSpec}
}

func pin(b *bc.Builder, tools map[string]engine.ToolSpec, names ...string) error {
	for _, n := range names {
		t, ok := tools[n]
		if !ok {
			return fmt.Errorf("tool %s is not registered", n)
		}
		b.Tool(n, t.SpecSHA256)
	}
	return nil
}

// Recon is the eng_001 workflow, a hand-assembled module equivalent in
// spirit to the recon example of spec 7.1:
//
//	type HttpService = { url: Url, status: Int }
//	type Report = { new_assets: Int, alerts: Int }
//	workflow recon(roots: List<Fqdn>) -> Report {
//	  let t0 = now();
//	  let subs = subfinder(roots[0]);
//	  let web = for n in subs {
//	    match scope.check(Host::Name(n)) { Ok(p) => [httpx(p)], Err(_) => [] } };
//	  emit_alert(Severity::High, "found {len(web)} services");
//	  log(LogLevel::Info, "recon done");
//	  Report { new_assets: len(web), alerts: 1 }
//	}
var Recon = Program{Build: func(tools map[string]engine.ToolSpec) (*bc.Module, error) {
	b := bc.NewBuilder()
	if err := pin(b, tools, "subfinder", "httpx"); err != nil {
		return nil, err
	}
	b.SetEffects(bc.EffTime | bc.EffPassive | bc.EffScope | bc.EffActive | bc.EffAlert)
	b.Record("HttpService", bc.Field{Name: "url", Type: "Url"}, bc.Field{Name: "status", Type: "Int"})
	report := b.Record("Report", bc.Field{Name: "new_assets", Type: "Int"}, bc.Field{Name: "alerts", Type: "Int"})
	f := b.Func("recon", 1, 0)
	b.SetEntry(f.Index())
	b.Signature(f.Index(), []bc.Field{{Name: "roots", Type: "List<Fqdn>"}}, "Report")
	t0, subs := f.Local(), f.Local()
	acc, xs, i, res, out := f.Local(), f.Local(), f.Local(), f.Local(), f.Local()
	f.Effect(bc.EffectNow, 0)
	f.Store(t0)
	f.Load(0)
	f.Int(0)
	f.Op(bc.OpListGet)
	f.ToolCall("subfinder", 1)
	f.Store(subs)
	f.Op(bc.OpNewList)
	f.Load(subs)
	f.Int(0)
	head := f.Here()
	f.Store(i)
	f.Store(xs)
	f.Store(acc)
	f.Load(i)
	f.Load(xs)
	f.Op(bc.OpListLen, bc.OpLt)
	exit := f.NewLabel()
	f.JumpIfFalse(exit)
	f.Load(xs)
	f.Load(i)
	f.Op(bc.OpListGet)
	f.MakeEnum(bc.TypeHost, bc.VariantName, 1)
	f.Effect(bc.EffectScopeCheck, 1)
	f.Store(res)
	f.Load(res)
	f.Emit(bc.OpTestVariant, int64(bc.VariantOk))
	denied, next := f.NewLabel(), f.NewLabel()
	f.JumpIfFalse(denied)
	f.Emit(bc.OpUnpack, 1)
	f.ToolCall("httpx", 1)
	f.Store(out)
	f.Load(acc)
	f.Load(out)
	f.Op(bc.OpListPush)
	f.Jump(next)
	f.Bind(denied)
	f.Op(bc.OpPop)
	f.Load(acc)
	f.Bind(next)
	f.Load(xs)
	f.Load(i)
	f.Int(1)
	f.Op(bc.OpAddI)
	f.Loop(head)
	f.Bind(exit)
	f.MakeEnum(bc.TypeSeverity, 3, 0)
	f.String("found ")
	f.Load(acc)
	f.Op(bc.OpListLen)
	f.String(" services")
	f.Emit(bc.OpInterp, 3)
	f.Effect(bc.EffectEmitAlert, 2)
	f.Op(bc.OpPop)
	f.MakeEnum(bc.TypeLogLevel, 1, 0)
	f.String("recon done")
	f.Effect(bc.EffectLog, 2)
	f.Op(bc.OpPop)
	f.Load(acc)
	f.Op(bc.OpListLen)
	f.Int(1)
	f.MakeRec(report, 2)
	f.Op(bc.OpRet)
	return b.Build()
}}

// Chain returns a workflow of n sequential echo calls:
//
//	workflow chain() -> Int { echo(echo(...echo(0 + 1)... + 1) + 1) }
func Chain(n int) Program {
	return Program{Build: func(tools map[string]engine.ToolSpec) (*bc.Module, error) {
		b := bc.NewBuilder()
		if err := pin(b, tools, "echo"); err != nil {
			return nil, err
		}
		b.SetEffects(bc.EffRead)
		f := b.Func("chain", 0, 0)
		b.SetEntry(f.Index())
		b.Signature(f.Index(), nil, "Int")
		f.Int(0)
		for range n {
			f.Int(1)
			f.Op(bc.OpAddI)
			f.ToolCall("echo", 1)
		}
		f.Op(bc.OpRet)
		return b.Build()
	}}
}

// Multi is the multi-step workflow of the crash-recovery property
// (eng_008):
//
//	workflow multi(roots: List<Fqdn>) -> Int {
//	  let t = now();
//	  let p = scope.check(Host::Name(roots[0]))?;
//	  let h = httpx(p);
//	  let xs = [1, 2, 3] |> par_map(x => echo(x), limit = 2);
//	  sleep(1s);
//	  log(LogLevel::Info, "probed");
//	  emit_alert(Severity::Low, "done");
//	  echo(len(xs) + h.status)
//	}
var Multi = Program{Build: func(tools map[string]engine.ToolSpec) (*bc.Module, error) {
	b := bc.NewBuilder()
	if err := pin(b, tools, "httpx", "echo"); err != nil {
		return nil, err
	}
	b.SetEffects(bc.EffTime | bc.EffScope | bc.EffActive | bc.EffRead | bc.EffAlert)
	b.Record("HttpService", bc.Field{Name: "url", Type: "Url"}, bc.Field{Name: "status", Type: "Int"})
	g := b.Func("probe_one", 1, 0)
	g.Load(0)
	g.ToolCall("echo", 1)
	g.Op(bc.OpRet)
	f := b.Func("multi", 1, 0)
	b.SetEntry(f.Index())
	b.Signature(f.Index(), []bc.Field{{Name: "roots", Type: "List<Fqdn>"}}, "Int")
	t, h, xs := f.Local(), f.Local(), f.Local()
	f.Effect(bc.EffectNow, 0)
	f.Store(t)
	f.Load(0)
	f.Int(0)
	f.Op(bc.OpListGet)
	f.MakeEnum(bc.TypeHost, bc.VariantName, 1)
	f.Effect(bc.EffectScopeCheck, 1)
	f.Op(bc.OpTry)
	f.ToolCall("httpx", 1)
	f.Store(h)
	f.Int(1)
	f.Int(2)
	f.Int(3)
	f.Emit(bc.OpMakeList, 3)
	f.Closure(g.Index(), 0)
	f.Int(2)
	f.Int(bc.ParMapFail)
	f.Effect(bc.EffectParMap, 4)
	f.Store(xs)
	f.Duration(int64(time.Second))
	f.Effect(bc.EffectSleep, 1)
	f.Op(bc.OpPop)
	f.MakeEnum(bc.TypeLogLevel, 1, 0)
	f.String("probed")
	f.Effect(bc.EffectLog, 2)
	f.Op(bc.OpPop)
	f.MakeEnum(bc.TypeSeverity, 1, 0)
	f.String("done")
	f.Effect(bc.EffectEmitAlert, 2)
	f.Op(bc.OpPop)
	f.Load(xs)
	f.Op(bc.OpListLen)
	f.Load(h)
	f.Emit(bc.OpGetField, 1)
	f.Op(bc.OpAddI)
	f.ToolCall("echo", 1)
	f.Op(bc.OpRet)
	return b.Build()
}}

// SagaOptions shapes the Saga workflow.
type SagaOptions struct {
	Approval bool          // start with require approval(role, "post", ApprovalTTL)
	Role     string        // approval role (default "finance_ap")
	TTL      time.Duration // approval timeout (default 1h)
	Steps    int           // number of compensated post steps (default 2)
	Block    bool          // end with block(0), a step tests can hold or fail
}

// Saga returns a workflow with compensated steps:
//
//	workflow saga(amount: Int) -> List<String> {
//	  require approval(role = "finance_ap", reason = "post", timeout = 1h);  // Approval
//	  let id1 = post(amount) compensate undo(it);                           // Steps times
//	  ...
//	  block(0);                                                              // Block
//	  [id1, ...]
//	}
func Saga(o SagaOptions) Program {
	if o.Role == "" {
		o.Role = "finance_ap"
	}
	if o.TTL == 0 {
		o.TTL = time.Hour
	}
	if o.Steps == 0 {
		o.Steps = 2
	}
	return Program{Build: func(tools map[string]engine.ToolSpec) (*bc.Module, error) {
		b := bc.NewBuilder()
		if err := pin(b, tools, "post", "undo", "block"); err != nil {
			return nil, err
		}
		b.SetEffects(bc.EffFinancialWrite | bc.EffRead | bc.EffApproval)
		undo := b.Func("undo_post", 0, 1)
		undo.Cap(0)
		undo.ToolCall("undo", 1)
		undo.Op(bc.OpRet)
		f := b.Func("saga", 1, 0)
		b.SetEntry(f.Index())
		b.Signature(f.Index(), []bc.Field{{Name: "amount", Type: "Int"}}, "List<String>")
		if o.Approval {
			f.String(o.Role)
			f.String("post")
			f.Duration(int64(o.TTL))
			f.Effect(bc.EffectApproval, 3)
			f.Op(bc.OpTry, bc.OpPop)
		}
		ids := make([]uint16, o.Steps)
		for k := range ids {
			ids[k] = f.Local()
			f.Load(0)
			f.Int(int64(k))
			f.Op(bc.OpAddI)
			f.ToolCall("post", 1)
			f.Store(ids[k])
			f.Load(ids[k])
			f.PushComp(undo.Index(), 1)
		}
		if o.Block {
			f.Int(0)
			f.ToolCall("block", 1)
			f.Op(bc.OpPop)
		}
		for _, id := range ids {
			f.Load(id)
		}
		f.Emit(bc.OpMakeList, int64(len(ids)))
		f.Op(bc.OpRet)
		return b.Build()
	}}
}

// Flaky is `workflow flaky() -> Int { flaky(1) }`.
var Flaky = Program{Build: func(tools map[string]engine.ToolSpec) (*bc.Module, error) {
	b := bc.NewBuilder()
	if err := pin(b, tools, "flaky"); err != nil {
		return nil, err
	}
	b.SetEffects(bc.EffRead)
	f := b.Func("flaky", 0, 0)
	b.SetEntry(f.Index())
	b.Signature(f.Index(), nil, "Int")
	f.Int(1)
	f.ToolCall("flaky", 1)
	f.Op(bc.OpRet)
	return b.Build()
}}

// ParMapEcho is
//
//	workflow fan(xs: List<Int>) -> List<Int> { xs |> par_map(x => block(x), limit = n) }
//
// with on_error collect when collect is set.
func ParMapEcho(limit int, collect bool) Program {
	return Program{Build: func(tools map[string]engine.ToolSpec) (*bc.Module, error) {
		b := bc.NewBuilder()
		if err := pin(b, tools, "block"); err != nil {
			return nil, err
		}
		b.SetEffects(bc.EffRead)
		g := b.Func("step", 1, 0)
		g.Load(0)
		g.ToolCall("block", 1)
		g.Op(bc.OpRet)
		f := b.Func("fan", 1, 0)
		b.SetEntry(f.Index())
		res := "List<Int>"
		if collect {
			res = "List<Result<Int, TaskError>>"
		}
		b.Signature(f.Index(), []bc.Field{{Name: "xs", Type: "List<Int>"}}, res)
		f.Load(0)
		f.Closure(g.Index(), 0)
		f.Int(int64(limit))
		mode := int64(bc.ParMapFail)
		if collect {
			mode = bc.ParMapCollect
		}
		f.Int(mode)
		f.Effect(bc.EffectParMap, 4)
		f.Op(bc.OpRet)
		return b.Build()
	}}
}

// Agent is a workflow with an agent_call effect (registration must reject
// it, docs/DECISIONS.md D1).
var Agent = Program{Build: func(tools map[string]engine.ToolSpec) (*bc.Module, error) {
	b := bc.NewBuilder()
	b.SetEffects(bc.EffLLM)
	f := b.Func("triage", 0, 0)
	b.SetEntry(f.Index())
	b.Signature(f.Index(), nil, "Unit")
	f.String("goal")
	f.Effect(bc.EffectAgentCall, 1)
	f.Op(bc.OpRet)
	return b.Build()
}}

// Blocking returns a handler that blocks until ctx is done (or release is
// closed, when it answers with its input) and signals started once.
func Blocking(started chan<- struct{}, release <-chan struct{}) ToolHandler {
	return func(ctx context.Context, call engine.ToolCall) (engine.ToolResult, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return engine.ToolResult{}, ctx.Err()
		case <-release:
			return engine.ToolResult{Output: call.Input}, nil
		}
	}
}
