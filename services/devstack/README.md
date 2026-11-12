# services/devstack

`Start(ctx, Options) (*Stack, error)` runs every internal service in one
process on loopback ports over real gRPC: scope, engine (workers), tool
runner, egress proxy, graph (+ analytics), similarity and notifier, with one
ephemeral internal signing key and one in-process event bus. In-memory
stores by default; `Options.DBDSN` selects Postgres (migrations applied).

`Stack` exposes the clients (`Scope`, `Engine`, `ToolRunner`, `Graph`,
`Analytics`, `Notifier`, `Similarity`), `Addrs`, the keys (`Keys`, `Tokens`,
`Verifier`), the `Bus`, `Metrics`, and `As(ctx, tenant, actor, role)` to set
the identity the clients mint tokens for. `cmd/flowforge dev` and the
gateway's tests use it.

Options for tests: `Resolver` (scope checks, dnsresolve, webhook
destinations), `ProbePorts` (httpprobe), `Secrets`, `AllowInsecureWebhooks`,
`Engine` (poll intervals), `Ports`.

`e2e_test.go` is the end-to-end test (see services/README.md);
`auth_test.go` checks that every service rejects missing or invalid tokens.
