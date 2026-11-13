# services/egressproxy

The scope-checked egress proxy (spec 3.2, 4.4, 4.5): the tool runner's only
way out. `Proxy` is an `http.Handler` serving CONNECT and plain-HTTP forward
requests.

For every request it

1. authenticates the internal token in `Proxy-Authorization` (407 otherwise);
2. loads the decision named by `X-Flowforge-Decision-Id` from the decision
   log, for the caller's tenant only; it must be an allow, at most
   `MaxDecisionAge` (1 h) old and, when `X-Flowforge-Run-Id` is sent, of
   that run;
3. checks that the decision's normalized target covers the requested host
   (scheme and port too for URL decisions); userinfo, mismatched `Host`,
   un-normalizable hosts (decimal IPs, ...) are refused;
4. checks the kill switches (fail closed when unknown);
5. connects only to the decision's pinned addresses, never re-resolving;
   loopback, private, link-local (169.254.169.254), CGNAT, multicast and
   unspecified addresses are refused unless the scope allows them with a
   cidr rule;
6. for plain HTTP, re-checks the `Location` of redirects with the scope
   service and refuses the response when the hop is denied (allowed hops get
   `X-Flowforge-Redirect-Decision`).

Every refusal answers 403 (or 407/502) with `X-Flowforge-Refused: <reason>`,
logs `egress.refused` and counts `ff_egress_refused_total{reason}`.

`proxy_test.go` is the M7 mock-target integration test: allowed request,
redirect to an out-of-scope host, DNS rebinding (the resolver changes after
the decision; 127.0.0.2 listener never reached), userinfo / decimal-IP /
suffix tricks, missing and unknown decisions, no token, kill switch,
CONNECT to another host.

## Notify path (G-SVC-08)

The notifier's webhook and Slack deliveries are not program targets, so
they carry `X-Flowforge-Egress-Purpose: notify` instead of a decision id.
The proxy then requires the `service:notifier` identity, CONNECT (https
only), a destination host on `NotifyAllowlist` (`FF_NOTIFY_ALLOWLIST`:
comma-separated names, `.example.com` for every name below a domain; empty
refuses everything), resolves the host itself and connects only when every
address passes `alerts.CheckResolved` (public unicast; spec 15.4), pinned
for the connection. Refusals use the same 403 + `X-Flowforge-Refused`
(`notify_not_notifier`, `notify_https_only`, `notify_not_allowlisted`,
`notify_resolve_failed`, `forbidden_address`). Tested end to end in
`services/notifier` (`TestRegress_G_SVC_08_WebhooksThroughEgressProxy`).

Standalone (`cmd/egress-proxy`) it reads the decision log from the scope
schema (`FF_DB_DSN`); in the dev stack it uses the scope service's
repository.
