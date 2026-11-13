# services/toolrunner

ToolRunnerService (spec 6, 11.5; V2_DESIGN §8).

* `Adapter` (spec 6.3): `Spec() Spec` and `Run(ctx, input, *ToolCtx,
  Emitter) (output, error)`. Adapters stream observations (at most 500
  entities each; validated with `oam.Observation.Validate`), progress and
  skipped targets, return `*ToolError{Code, Message, Retryable, RetryAfter}`, and must
  stop within 2 s of cancellation (`Execute` waits `CancelGrace`, then
  returns).
* `ToolCtx.Allowed` re-checks a concrete target with ScopeService.Check
  (resolving and pinning for active classes; `AllowedPinned` asks for pins
  whatever the class) before the adapter touches it; a deny is streamed as
  `Skipped` and the target is never contacted (spec 4.4, tool-runner layer).
* `Network` is the only route out: `Egress` (the egress proxy) in
  production, recorded fixtures in tests (`ToolCtx.Net`).
  `HTTPClient` (program targets; every redirect hop re-checked with its own
  decision, G-SVC-07: inside a TLS tunnel the proxy cannot see a Location,
  so the runner checks each hop with an active, resolving scope check
  whose decision is recorded and pinned, and opens the next hop's CONNECT
  under it; the proxy in turn admits each tunnel only for the host and
  port its decision covers. https is tunnelled by the runner itself
  (CONNECT, then the target's TLS), so the decrypted response can be
  recorded; connections are never reused), `IntelClient` (intelligence sources under the passive decision;
  hops must stay on the proxy's allowlist), `DialTCP` (CONNECT, or SOCKS5
  when `Egress.SOCKS`, declaring the call's ports in `X-Flowforge-Ports`) and
  `LookupDNS` (the proxy's DNS forwarder, ticket in an EDNS option).
  Refusals are `*RefusedError{Reason, RetryAfter}` (`errors.Is(err,
  ErrRefused)`).
* **Edge outcome** (G-ENG-09). The proxy is the enforcement point of the
  per-target limit and of kill switches. When it refused a request of an
  execution with `kill_switch` (engaged after the runner's re-check: a
  race) the execution ends as `kill_switch_active`, and with
  `rate_limited` / `concurrency_limited` as `rate_limited` carrying the
  longest retry delay (`ToolError.retry_after`); either replaces the
  adapter's result, which lacks what the edge refused. A rate-limit
  refusal the adapter retries itself is taken over
  (`ToolCtx.RetryingEdgeLimit`; portscan paces and retries) and not
  reported. The engine cancels
  a raced run with reason `kill_switch_race[:level]` and waits a
  rate-limit delay out without counting an attempt (spec 9.5).
* **Raw capture** (`rawcapture.go`, spec 6.1 raw_ref, G-SVC-16). With
  `Options.Raw`, each execution records the protocol bytes the egress
  paths read, as read: every HTTP response off its connection (after TLS),
  every DNS response message from the forwarder, and what a TLS server
  sent during `tlscert`'s handshake; at most `DefaultRawLimit` (8 MiB) per
  execution (`RawLimit`), later bytes cut and flagged. HTTP records are
  redacted before storage (Set-Cookie values and credential headers become
  typed placeholders, credential-shaped strings are replaced by the
  content secret rules); DNS and TLS records are public and kept as
  captured. The records form an FFRW v1 transcript (`EncodeRawTranscript`;
  bytes in the `rawcapture.go` package comment) stored by a `RawStore`:
  `ChunkRaw` in the chunk store (namespace `r<yyyymmdd>-<tenant>`, ref
  `chunk://<ns>/<manifest>`), or `BlobRaw` (one object per transcript,
  named by its SHA-256) where chunkstore has no backend (Cloud Storage,
  G-CST-02); `RawStoreFor` picks one from the services' object store. Every
  observation names the transcript captured up to its emission.

## Tools

| Tool | DSL signature | Class | Emits |
|------|---------------|-------|-------|
| `dnsresolve` 2.0.0 | `(Fqdn) -> List<IpAddress>` | passive | only via the proxy's DNS forwarder (G-SVC-06: the runner has no resolver; no proxy is `no_egress`, a proxy without a forwarder `no_dns_forwarder`): A/AAAA (`dns_record` BasicDNSRelation to IPAddress), CNAME (FQDN → FQDN, every hop scope-checked, a denied hop is skipped and its addresses unused), MX (PrefDNSRelation), NS, TXT (`DNSRecordProperty` `dns_record`, strings joined) |
| `httpprobe` 1.0.0 | `(InScope<Host>) -> Option<Service>` | active | unchanged |
| `portscan` 2.0.0 | `(InScope<Host>, List<Int>) -> List<Int>` | active | Service + `port` PortRelation per open port; paced per target (default 1/rate), `Parallel` workers, rate-limit refusals retried after the proxy's hint, policy refusals reported as skipped |
| `tcpconnect` 1.1.0 | same | active | alias of `portscan` |
| `tlscert` 1.0.0 | `(InScope<Host>, Int) -> Option<TlsCertificate>` | passive (D3) | TLS handshake through a tunnel under a pinned decision, SNI = host; `InsecureSkipVerify` only to read the chain, the handshake is aborted when the certificates arrive (no application data). Service (`tls`), `port`, `certificate`, TLSCertificate, `common_name` / `san_dns_name` → FQDN |
| `ctlog` 1.0.0 | `(Fqdn) -> List<Fqdn>` | passive | crt.sh search (`%.name`): `name_value` split on newlines, wildcards → base name, e-mails and IPs dropped, deduplicated; output = names at or below the input. Per certificate id (newest first, at most 50) the DER is downloaded, because a TLSCertificate is keyed by its SHA-256 fingerprint; `common_name` / `san_dns_name` edges |
| `rdap` 1.0.0 | `(Host) -> List<Organization>` | passive | IANA bootstrap (dns/ipv4/ipv6, cached 24 h) → registry; domain (registrable domain) → DomainRecord (events, status, DNSSEC, port43) with `registration` from the FQDN; ip → IPNetRecord, Netblock (cidr0 or the range as prefixes) with `registration` and `contains`; Organizations from entities with roles `registrant` / `administrative` (edges labelled by role); redirects followed through the proxy |
| `asn` 1.0.0 | `(IpAddress) -> Option<Asn>` | passive | RDAP ip network (+ ARIN origin-AS extension), Team Cymru `origin(6).asn.cymru.com` TXT via the DNS forwarder (announced prefix, origin AS), RDAP autnum; AutonomousSystem `announces` Netblock `contains` IPAddress, AutnumRecord (`registration`), IPNetRecord |
| `collect` 1.0.0 | `(InScope<Url>, String) -> Snapshot` (capture profile name) | active | fetches through the proxy with the profile's request shape, every redirect hop re-checked and fetched under its own decision (a hop out of scope is not followed: the redirect response is the capture), body read up to 8 MiB (longer: truncated, outcome partial), stored with `IngestCapture` (run id, effect path); for an HTML page also its scripts and styles and each script's source map (at most 32, `page_url` set for lineage, each scope-checked, a denied one skipped and never contacted). Output: the page's Snapshot with the subresources in `resources` |
| `crawl` 1.0.0 | `(InScope<Url>, Int) -> List<Url>` (fetch budget, at most 500) | active | breadth-first frontier: every URL canonicalized (netnorm) and scope-checked before it is queued, robots.txt per origin (RFC 9309: 2xx parsed, 4xx allow all, 5xx/unreachable/out of scope disallow all; agent token `flowforge`), links, frames and sitemap entries of HTML and sitemap captures (a redirected capture contributes its target), revisit policy per (program, URL, profile): a URL not yet due is expanded from its stored open version without a fetch. Output: every in-scope, robots-allowed URL reached, fetched or not |
| `wellknown` 1.0.0 | `(InScope<Host>) -> List<Url>` | active | per configured scheme and port: robots.txt (always stored), `/.well-known/security.txt` then `/security.txt`, the sitemaps robots.txt names (else `/sitemap.xml`; an index followed one level, at most 10), OpenAPI/Swagger at `/openapi.json`, `/swagger.json`, `/v3/api-docs`, `/api-docs`, `/.well-known/openapi.json` (found when it yields schema endpoints), GraphQL `GET /graphql?query={__typename}` (JSON `data`/`errors`). Probes store 2xx answers only; a resource counts when its sniffed kind is the expected one. Output: found resources, then in-scope sitemap entries |
| `extract` 1.0.0 | `(Snapshot) -> Features` | read | `GetSnapshot(include_features)` grouped by kind (URLs canonical; secrets only as `<rule>:<preview>`); a derived `#src/` snapshot gives that document's features |
| `unpack_sourcemap` 1.0.0 | `(Snapshot) -> List<Snapshot>` | read | the `#src/<path>` originals of a stored source map (or of the map a script names): from the features JSON (`sourcemap_source` with content, and `doc` suffixes); ids and URLs are the map's plus the suffix |
| `diff_latest` 1.0.0 | `(Snapshot) -> ChangeSet` | read | `DiffSnapshots` with `from` empty (parent, lineage parent, or the baseline); a derived snapshot gives its document's artifact; `summary` adds `<kind>_<op>` counts |
| `search_changes` 1.0.0 | `(String, Int) -> List<ChangeHit>` | read | `SearchChanges` (V2 §7 query language, limit 1-1000) |
| `mock` 1.0.0 | `(String) -> String` | none | test tool |

### Content tools (V2_DESIGN §8)

`ContentAdapters(ContentConfig)` builds the seven content tools over one
content client (`ContentClient`, the part of `ContentService` they call;
`wire.ToolRunnerOptions.ContentAddr` dials it, with a 12 MiB message
limit `ContentMaxMessage` for capture bodies). Configuration: named
capture profiles besides `default` (credential headers and cookie jars
refused), body bound, redirect and subresource limits, crawl budget cap,
the revisit policy and its state store, wellknown's ports, User-Agent.

* **Scope.** Every URL a content tool touches is checked twice: a passive
  check first (no resolution), then the active one that resolves and
  pins. Scope's active check resolves a name before it matches the allow
  rules (spec 4.3 step 7), so the passive step keeps out-of-scope names
  out of DNS entirely (the lab's DNS log proves it). A denied URL is
  streamed as `Skipped` and never contacted.
* **One capture per run.** A (URL, profile) whose open version the calling
  run already stored is returned without a fetch, so crawl, wellknown and
  collect in one run never split a deploy's change across two versions
  that differ only in per-response volatile values. `new_in_run` of a
  Snapshot compares the version's run with the calling run.
* **Emission.** The content tools stream no observations: the content
  service publishes each capture's oam observation (URL, File, Endpoint,
  ContentProperty, HTTPHeaderProperty, `redirects_to`, `previous_version`,
  ... with coverage-gated authority) in its `content.changes` event, so
  every content fact has one writer. The raw response is the capture
  stored in the tenant's chunk store (raw snapshot), not a `raw/` blob.
* **Revisit** (`RevisitPolicy`, docs/ALGORITHMS.md "Crawl revisit
  policy"): `AdaptiveRevisit` (default; halve on change, double on no
  change, [1 h, 7 d], first interval 24 h) or `FixedRevisit`;
  `FF_CRAWL_REVISIT=adaptive|fixed`. State is per tool-runner process
  (`MemRevisitStore`); after a restart every URL is due.

Every tool declares a retry policy and an idempotency class (G-SVC-09),
carried in the proto ToolSpec (`retry`, `idempotency`) and honoured by the
engine (spec 6.2, 9.5): `RetryTransient` (3 attempts, 1-30 s, on
`unavailable`, `deadline_exceeded`) for target-facing tools, `RetryIntel`
(4 attempts, 2-60 s) for ctlog, rdap and asn, `RetryContent` (adds
`content_unavailable`) for the content tools, `RetryNever` for mock; every
built-in tool is `idempotent`. The runner's own tool timeout is reported
as `deadline_exceeded`. `spec_sha256` is the canonical hash of the
engine-side spec, policy and class included (`Spec.EngineSpec`; a spec
without a policy gets `convert.DefaultToolRetry` on both sides).

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
  sources) is in `services/egressproxy/v2_test.go`; HTTPS redirect hops,
  raw transcripts and edge outcomes through the real proxy in
  `services/egressproxy/regress_v2_test.go` (`TestRegress_G_SVC_07_*`,
  `TestRegress_G_SVC_16_RawProtocolBytes`, `TestRegress_G_ENG_09_EdgeOutcome`).
* `regress_v2_test.go`: dnsresolve without a forwarder, the ToolSpec
  retry fields and hashes, the FFRW bytes, redaction, the capture bound
  and the chunk-store raw store.
* `content_test.go` runs the content tools against the lab
  (`services/internal/lab`) with an in-process content service and a
  network that dials the decision's pinned address: the §8 signature
  table (`TestContentTools_Signatures`), monitor.ff compiled against the
  registry as the engine does (`TestContentTools_MonitorWorkflowCompiles`,
  active; refused by a passive program), collect with subresources,
  lineage across a renamed bundle, the third-party script skipped and
  in-run reuse; redirects in and out of scope; crawl's robots, scope,
  budget and not-due/due revisits on the lab clock; wellknown on both lab
  hosts; extract, diff_latest, unpack_sourcemap (and extract on a derived
  original), search_changes, a bad query and a cross-program read.
  `revisit_test.go`: the adaptive schedule, `TestRevisitProp_Bounded`, and
  `BenchmarkToolAlgo/revisit/...`.
* The whole surface end to end (engine, proxy, content, graph, notifier)
  is `services/internal/lab` `TestLabAcceptance_*`.

## Open questions

* `registration` from FQDN → DomainRecord and AutonomousSystem →
  AutnumRecord is an oam endpoint-table extension (oam OPEN_QUESTIONS).
* Registrant/administrative organizations are linked from the record with
  the RDAP role as label (no ContactRecord in between).
* A wildcard SAN `*.x` is reported as the FQDN `x`.
* `tlscert` re-checks with `AllowedPinned` (CheckRequest.resolve = true): a
  passive decision that resolves and pins (D3); without pins the proxy
  refuses the tunnel (`no_pinned_ips`).

* **Content tools.** extract and diff_latest do not re-emit the graph
  facts V2 §8 lists for them: those were published, with authority, when
  the capture was stored, and a second writer would apply authority twice.
  Cookie-jar profiles are refused (sandbox work, ROADMAP_V2 A1). Revisit
  state is in memory. A page reached through a redirect has its
  references resolved against the requested URL by content (the lab's
  OPEN_QUESTIONS 1): crawl expands a redirect's target instead of the
  redirected capture's links, collect's subresources of such a page are
  fetched through the original origin. unpack_sourcemap's derived
  snapshots have no body hash, size or media type of their own.

Not here: sandboxes (tools run in-process). Raw transcripts are not yet
garbage-collected inside a chunk namespace; whole namespaces expire with
the day they were written (object-age lifecycle).
