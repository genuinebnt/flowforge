# services/toolrunner

ToolRunnerService (spec 6, 11.5; V2_DESIGN §8).

* `Adapter` (spec 6.3): `Spec() Spec` and `Run(ctx, input, *ToolCtx,
  Emitter) (output, error)`. Adapters stream observations (at most 500
  entities each; validated with `oam.Observation.Validate`), progress and
  skipped targets, return `*ToolError{Code, Message, Retryable}`, and must
  stop within 2 s of cancellation (`Execute` waits `CancelGrace`, then
  returns).
* `ToolCtx.Allowed` re-checks a concrete target with ScopeService.Check
  (resolving and pinning for active classes; `AllowedPinned` asks for pins
  whatever the class) before the adapter touches it; a deny is streamed as
  `Skipped` and the target is never contacted (spec 4.4, tool-runner layer).
* `Network` is the only route out: `Egress` (the egress proxy) in
  production, recorded fixtures in tests (`ToolCtx.Net`).
  `HTTPClient` (program targets; every redirect hop re-checked with its own
  decision), `IntelClient` (intelligence sources under the passive decision;
  hops must stay on the proxy's allowlist), `DialTCP` (CONNECT, or SOCKS5
  when `Egress.SOCKS`, declaring the call's ports in `X-Flowforge-Ports`) and
  `LookupDNS` (the proxy's DNS forwarder, ticket in an EDNS option).
  Refusals are `*RefusedError{Reason, RetryAfter}` (`errors.Is(err,
  ErrRefused)`).

## Tools

| Tool | DSL signature | Class | Emits |
|------|---------------|-------|-------|
| `dnsresolve` 2.0.0 | `(Fqdn) -> List<IpAddress>` | passive | via the DNS forwarder: A/AAAA (`dns_record` BasicDNSRelation to IPAddress), CNAME (FQDN → FQDN, every hop scope-checked, a denied hop is skipped and its addresses unused), MX (PrefDNSRelation), NS, TXT (`DNSRecordProperty` `dns_record`, strings joined); without a forwarder A/AAAA from the runner's resolver |
| `httpprobe` 1.0.0 | `(InScope<Host>) -> Option<Service>` | active | unchanged |
| `portscan` 2.0.0 | `(InScope<Host>, List<Int>) -> List<Int>` | active | Service + `port` PortRelation per open port; paced per target (default 1/rate), `Parallel` workers, rate-limit refusals retried after the proxy's hint, policy refusals reported as skipped |
| `tcpconnect` 1.1.0 | same | active | alias of `portscan` |
| `tlscert` 1.0.0 | `(InScope<Host>, Int) -> Option<TlsCertificate>` | passive (D3) | TLS handshake through a tunnel under a pinned decision, SNI = host; `InsecureSkipVerify` only to read the chain, the handshake is aborted when the certificates arrive (no application data). Service (`tls`), `port`, `certificate`, TLSCertificate, `common_name` / `san_dns_name` → FQDN |
| `ctlog` 1.0.0 | `(Fqdn) -> List<Fqdn>` | passive | crt.sh search (`%.name`): `name_value` split on newlines, wildcards → base name, e-mails and IPs dropped, deduplicated; output = names at or below the input. Per certificate id (newest first, at most 50) the DER is downloaded, because a TLSCertificate is keyed by its SHA-256 fingerprint; `common_name` / `san_dns_name` edges |
| `rdap` 1.0.0 | `(Host) -> List<Organization>` | passive | IANA bootstrap (dns/ipv4/ipv6, cached 24 h) → registry; domain (registrable domain) → DomainRecord (events, status, DNSSEC, port43) with `registration` from the FQDN; ip → IPNetRecord, Netblock (cidr0 or the range as prefixes) with `registration` and `contains`; Organizations from entities with roles `registrant` / `administrative` (edges labelled by role); redirects followed through the proxy |
| `asn` 1.0.0 | `(IpAddress) -> Option<Asn>` | passive | RDAP ip network (+ ARIN origin-AS extension), Team Cymru `origin(6).asn.cymru.com` TXT via the DNS forwarder (announced prefix, origin AS), RDAP autnum; AutonomousSystem `announces` Netblock `contains` IPAddress, AutnumRecord (`registration`), IPNetRecord |
| `mock` 1.0.0 | `(String) -> String` | none | test tool |

`spec_sha256` is the canonical hash of the engine-side spec with
`convert.DefaultToolRetry` (`Spec.EngineSpec`).

Organizations never carry personal data: individuals without an
organization name and redacted contacts are skipped. Their `unique_id` is
`rdap:<HANDLE>`, or `rdap-name:<lowercased name>` without a usable handle.

## Tests

* `fixtures_test.go` runs every intelligence/network tool against recorded
  fixtures in `conformance/tools/<tool>/*.json` (crt.sh JSON and DER, IANA
  bootstrap files, PIR / Verisign / ARIN / RIPE RDAP objects including a
  301 referral, Team Cymru TXT, DNS answers, TCP and TLS endpoints with
  deterministic Ed25519 certificates) and asserts the output, error,
  skipped targets, contacted URLs, dials, DNS queries and observations
  exactly. Each tool has a scope-denial fixture: skipped, nothing contacted.
  `go test ./services/toolrunner -run TestToolFixtures -update` re-records
  the `expect` members, which are then reviewed by hand.
* `fuzz_test.go`: `FuzzCTLogJSON`, `FuzzRDAPJSON` (bootstrap, objects,
  jCards; every derived asset is valid for oam), `TestRangeCIDRs`,
  `TestCymru`.
* The egress integration (real proxy, SOCKS5, DNS, rate limits, intel
  sources) is in `services/egressproxy/v2_test.go`.

## Open questions

* `registration` from FQDN → DomainRecord and AutonomousSystem →
  AutnumRecord is an oam endpoint-table extension (oam OPEN_QUESTIONS).
* Registrant/administrative organizations are linked from the record with
  the RDAP role as label (no ContactRecord in between).
* A wildcard SAN `*.x` is reported as the FQDN `x`.
* `tlscert` re-checks with `AllowedPinned` (CheckRequest.resolve = true): a
  passive decision that resolves and pins (D3); without pins the proxy
  refuses the tunnel (`no_pinned_ips`).

Not here: sandboxes (tools run in-process), retry policies per tool (the
proto ToolSpec has no field for them).
