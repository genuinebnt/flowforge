# services/toolrunner

ToolRunnerService (spec 6, 11.5).

* `Adapter` (spec 6.3): `Spec() Spec` and `Run(ctx, input, *ToolCtx,
  Emitter) (output, error)`. Adapters stream observations (at most 500
  entities each; validated with `oam.Observation.Validate`), progress and
  skipped targets, return `*ToolError{Code, Message, Retryable}`, and must
  stop within 2 s of cancellation (`Execute` waits `CancelGrace`, then
  returns).
* `ToolCtx.Allowed` re-checks a concrete target with ScopeService.Check
  (resolving and pinning for active classes) before the adapter touches it;
  a deny is streamed as `Skipped` (spec 4.4, tool-runner layer).
* `Egress` is the only route out: HTTP through the egress proxy (forward
  proxy for http, CONNECT for https) with the decision id, run id, effect
  path and an internal token in `Proxy-Authorization`; every redirect hop is
  re-checked and carries its own decision; `Dial` opens TCP via CONNECT.
* Adapters: `dnsresolve` (passive; FQDN + IPAddress, `dns_record` edges with
  BasicDNSRelation headers), `httpprobe` (active; URL, FQDN, IPAddress,
  Service, Product; domain, ip_address, port, product_used edges; page title
  as SimpleProperty; output the prelude `Service`), `tcpconnect` (active;
  Service + `port` PortRelation per open port), `mock` (deterministic, no
  target). `spec_sha256` is the canonical hash of the engine-side spec with
  `convert.DefaultToolRetry`.

Not here: sandboxes (tools run in-process), retry policies per tool (the
proto ToolSpec has no field for them).
