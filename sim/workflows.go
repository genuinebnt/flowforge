package sim

import (
	"github.com/genuinebnt/flowforge/engine"
)

// The workflows the workloads submit. They are compiled from DSL source
// by the real compiler (package compiler) through the engine's Compiler
// port; the tools are generated from the registry (toolSpecs), so the
// sources declare none.
const (
	// srcScan: linear tool calls with scope checks (spec 7.1 recon
	// shape): a scope.check per root, an active probe with the proof, a
	// passive resolve that reports observations, an alert per live root.
	srcScan = `
workflow scan(roots: List<Fqdn>) -> Int {
  let live = roots |> filter_map(r => match scope.check(Host::Name(r)) {
    Ok(p) => httpx(p),
    Err(_) => None,
  });
  let ips = roots |> filter_map(resolve);
  for code in live {
    emit_alert(Severity::High, "live service status {code}");
  }
  len(live) + len(ips)
}
`
	// srcFanout: par_map fan-out of scope-checked probes.
	srcFanout = `
workflow fanout(names: List<Fqdn>) -> Int {
  let codes = names |> par_map(n => match scope.check(Host::Name(n)) {
    Ok(p) => httpx(p),
    Err(_) => None,
  }, limit = 3) |> filter_map(x => x);
  log("info", "fanout probed {len(codes)}");
  len(codes)
}
`
	// srcSaga: two compensated writes, an approval, then a financial
	// write that may fail permanently; a denial or a failure runs the
	// compensations in reverse order (spec 9.6).
	srcSaga = `
workflow saga(amount: Int) -> Int {
  let a = reserve("seat") compensate release(it);
  let b = reserve("meal") compensate release(it);
  require approval(role = "finance_ap", reason = "charge {amount}", timeout = 1h);
  let c = charge(amount + a + b);
  c
}
`
	// srcRetry: timers and retries of a transiently failing tool.
	srcRetry = `
workflow retry_timer(n: Int) -> Int {
  sleep(20s);
  let x = flaky(n);
  sleep(40s);
  let y = flaky(x + 1);
  log("info", "flaky done {y}");
  y
}
`
	// srcLong: a long run of timed probes; the workloads cancel it, flip
	// a kill switch under it or change its program's scope mid-run.
	srcLong = `
workflow longrun(names: List<Fqdn>) -> Int {
  let codes = names |> map(n => {
    sleep(30s);
    match scope.check(Host::Name(n)) {
      Ok(p) => match httpx(p) { Some(c) => c, None => 0 },
      Err(_) => 0,
    }
  });
  fold(codes, 0, (acc, c) => acc + c)
}
`
)

// Workflow names.
const (
	wfScan    = "scan"
	wfFanout  = "fanout"
	wfSaga    = "saga"
	wfRetry   = "retry_timer"
	wfLongrun = "longrun"
)

var workflowSources = []struct{ name, src string }{
	{wfScan, srcScan}, {wfFanout, srcFanout}, {wfSaga, srcSaga}, {wfRetry, srcRetry}, {wfLongrun, srcLong},
}

// Tool names.
const (
	toolResolve = "resolve"
	toolHTTPX   = "httpx"
	toolReserve = "reserve"
	toolRelease = "release"
	toolCharge  = "charge"
	toolFlaky   = "flaky"
)

// toolSpecs returns the registered tools.
func toolSpecs() []engine.ToolSpec {
	backoff := engine.Backoff{InitialMs: 200, MaxMs: 5000, Multiplier: 2, Jitter: "full"}
	return []engine.ToolSpec{
		{Name: toolResolve, Version: "1.0.0", InputType: "Fqdn", OutputType: "Option<IpAddress>", EffectClass: "passive",
			Idempotent: true, Observes: []string{"FQDN", "IPAddress"}, Retry: engine.RetryPolicy{MaxAttempts: 4, Backoff: backoff}, TimeoutMs: 10_000},
		{Name: toolHTTPX, Version: "1.0.0", InputType: "InScope<Host>", OutputType: "Option<Int>", EffectClass: "active",
			Idempotent: true, Retry: engine.RetryPolicy{MaxAttempts: 4, Backoff: backoff}, TimeoutMs: 10_000},
		{Name: toolReserve, Version: "1.0.0", InputType: "String", OutputType: "Int", EffectClass: "write",
			Retry: engine.RetryPolicy{MaxAttempts: 3, Backoff: backoff}, TimeoutMs: 10_000},
		{Name: toolRelease, Version: "1.0.0", InputType: "Int", OutputType: "Unit", EffectClass: "write",
			Idempotent: true, Retry: engine.RetryPolicy{MaxAttempts: 6, Backoff: backoff}, TimeoutMs: 10_000},
		{Name: toolCharge, Version: "1.0.0", InputType: "Int", OutputType: "Int", EffectClass: "financial_write",
			Retry: engine.RetryPolicy{MaxAttempts: 3, Backoff: backoff}, TimeoutMs: 10_000},
		{Name: toolFlaky, Version: "1.0.0", InputType: "Int", OutputType: "Int", EffectClass: "read",
			Idempotent: true, Retry: engine.RetryPolicy{MaxAttempts: 6, Backoff: backoff}, TimeoutMs: 10_000},
	}
}
