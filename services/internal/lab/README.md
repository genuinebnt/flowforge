# services/internal/lab

The in-process lab of V2_DESIGN §10: a small, deterministic internet for
end-to-end tests of the content workflow surface, built so that later
evaluation harnesses (v4) can run other workflows against other sites.

| Piece | Where | What |
|---|---|---|
| in-scope TLS listener | 127.0.0.1, random port (`ListenerHTTPS`) | virtual hosts by `Host`, one Ed25519 certificate for every in-scope host, signed by the lab CA (`ClientTLS`, `RootCAs`) |
| in-scope HTTP listener | 127.0.0.1 (`ListenerHTTP`) | plain HTTP, redirects |
| out-of-scope HTTP listener | 127.0.0.2 (`ListenerOut`) | the hosts a monitor must never contact |
| authoritative DNS | 127.0.0.1, UDP and TCP on one port (`DNSAddr`) | A records of the lab hosts (in → 127.0.0.1, out → 127.0.0.2), NODATA for other types, NXDOMAIN inside the lab zones, REFUSED outside; `Resolver()` is a `scope.Resolver` over it |
| clock | `Lab.Clock` (`core.Clock`) | jumps to each deploy's time, so weeks of history run in seconds |
| logs | `Hits()`, `Queries()` | every request (listener, Host, SNI, method, path, deploy) and every DNS question |

Everything but the ports is deterministic: certificates come from fixed
seeds, per-response volatile values (`{{nonce}}`, `{{ts}}`, `{{ts_s}}`,
`{{iso}}`, `{{request_id}}`) from `core.SplitMix64` with the manifest's
seed, times from the lab clock. Out-of-scope hosts live on 127.0.0.2,
which needs Linux's loopback /8.

## Sites

A site is `conformance/lab/<site>/manifest.json` (`Manifest`), read from
the file system `Start` gets (the conformance directory, so a site reuses
other suites' data, e.g. `conformance/content/sites/spa`):

* `hosts`: names with scope `in` or `out`;
* `deploys`: each the previous deploy's files plus `files` (added or
  replaced, keyed by listener, host and path) minus `remove`. A file is a
  body (`file`, `type`, `headers` as "Name: value" lines, `template` to
  expand placeholders in the body; headers always are) or a redirect
  (`redirect` Location template with `{{path}}`, `status`); `prefix`
  serves every path below `path`. Ports are `{{https}}`, `{{http}}`,
  `{{out_http}}`;
* `expect`: what monitoring the deploys must produce: the planted level-3
  changes (deploy, snapshot URL, `<op> <kind>|<key>`, a regex query that
  finds it), the renamed bundles and their predecessors, notifier rules
  with their alert counts, and paths robots.txt keeps a polite monitor
  away from.

`conformance/lab/shop` is the acceptance site: `shop.lab.test` (TLS; a
39 KB product page, account, about, products and catalog pages, robots.txt
with a disallowed `/private/`, a sitemap with an out-of-scope entry,
security.txt, webpack bundles with source maps, a lazily loaded chunk, a
stable store chunk, CSS) with a plain-HTTP listener that redirects;
`api.lab.test` (OpenAPI document, GraphQL endpoint); out of scope,
`evil.lab.test` (linked from the page, the target of `/go`, a sitemap
entry) and `cdn.thirdparty.test` (the third-party script of d3). Six
weekly deploys:

| Deploy | Changes |
|---|---|
| d1 | baseline |
| d2 | only volatile values, formatting and build hashes (page `masked_only`); bundle renamed `main.3f9a1c2b` → `main.77e0d41a` with modules reordered and locals renamed (`reordered`); CSS renamed, same content |
| d3 | page: POST form with its parameters, third-party script without SRI, the AWS documentation example key pair, CSP loosened, X-Frame-Options removed, HttpOnly dropped, lazy chunk renamed `index-a1b2c3d4` → `app-e5f6a7b8`; main bundle renamed `main.5b6c7d8e` with an admin endpoint and tinylib 1.2.0 → 1.3.0; the renamed lazy chunk adds an admin endpoint |
| d4 | page: CSP header removed; main bundle renamed `main.c4d5e6f7` with a DELETE endpoint; OpenAPI document gains a DELETE operation |
| d5, d6 | nothing but per-response nonces, timestamps and request ids |

## The reference workflow (`conformance/workflows/monitor.ff`)

`monitor(roots, hosts, budget)` runs once per deploy:

1. `crawl_root` scope-checks each root and crawls it: the crawl reads
   robots.txt per origin (`/private/admin` is never requested), scope-checks
   every link before queueing it (evil.lab.test is refused, never
   resolved), fetches at most `budget` URLs whose revisit time has come,
   and returns the in-scope frontier.
2. `discover` runs `wellknown` per host: robots.txt, security.txt, the
   sitemap (its out-of-scope entry is refused), the OpenAPI document and
   the GraphQL endpoint of api.lab.test.
3. `capture` scope-checks each URL and `collect`s it with profile
   `default`; for the page that also captures its scripts, styles and
   source maps, so renamed bundles link to their predecessor. URLs crawl
   or wellknown already stored in this run are not fetched again.
   `par_map(..., on_error = "collect")` lets one failing URL fail alone.
4. `unpack_sourcemap` adds the maps' original sources; every version this
   run stored (`new_in_run`, not degraded) is diffed against its
   predecessor (`diff_latest`).
5. Level-3 changes default feeds show (`semantic && alertable`) are
   ordered by score (ranking only) and each emitted as an alert
   (`emit_alert`); the run's output is one report line per changed
   version.

Independently, every capture's `content.changes` event reaches the graph
(observations, `content_changed`, `previous_version`) and the notifier,
whose rules see the change as the `diff` variable.

## Acceptance (`TestLabAcceptance_Memory`, `TestLabAcceptance_Postgres`)

The test starts the lab and the dev stack (engine, tool runner, egress
proxy, scope, content, graph, notifier; the stack resolves through the
lab's DNS, trusts its CA and runs the tool runner on the lab clock),
creates a program whose scope allows the in-scope hosts and 127.0.0.1/32,
the manifest's notifier rules with a webhook, registers monitor.ff and
runs it once per deploy. Measured 2026-11-13 (memory and Postgres alike):

| §10 item | Assertion | Result |
|---|---|---|
| 1 capture each deploy | every page, script, style and map the deploy serves was requested in that deploy and has a stored version valid at its time | all 6 deploys |
| 2 exactly the planted level-3 changes, no noise | the level-3 changes of all 49 stored versions equal the 18 planted ones (secret keys matched up to the rule, their hash is tenant-keyed), each alertable; no version is a text-only (`structural`) change; each run's report lists exactly its deploy's changed versions | 18 = 18; outcomes: 23 baselines, 6 changed, 18 masked_only, 1 normalized_equal, 1 reordered |
| 3 renamed chunks linked | each renamed bundle's first version has its predecessor's version as lineage parent, and the graph has File → File `previous_version` | 5 renames: the main bundle three times, the lazy chunk, the CSS |
| 4 alerts once each | per rule, exactly the planted count; the webhook received each once with a valid signature; content alerts carry `diff` (URL, changes, summary, excerpt), none carries the key | content_level3 6, new_endpoint 4, security_header_removed 2, leaked_secret 1, workflow_alert 6 (19 deliveries) |
| 5 searchable | each planted change is found at its version by `search_changes` run through the tool runner, by `kind:<kind>_<op>` and by the manifest's regex | 18 × 2 queries |
| 6 no out-of-scope contact | no request on the out-of-scope listener or for an out-of-scope Host, no DNS question for an out-of-scope name, no robots-disallowed path requested, and refusals recorded for both out-of-scope hosts | 254 requests, 0 out of scope; 538 DNS questions, 0 out of scope; 4 refused targets |
| 7 storage < 25 % of raw | raw snapshots (capture record + redacted body of every version: CSTORE-INV-08's measure, `contentstore.Store.SnapshotStorage`) against the versions' raw size | 46.2 KB of 683 KB = 6.8 %; with normalized texts and features 16.3 %; all records including diffs 18.4 % |

## Running

```sh
go test ./services/internal/lab/                      # memory
FF_TEST_PG_DSN=postgres://... go test ./services/internal/lab/ -run Postgres
FF_LAB_DEBUG=1 go test ./services/internal/lab/ -v    # also log every baseline and each version's storage
```

The content tools' own tests (`services/toolrunner/content_test.go`) use
the same lab without the proxy and the engine.

Known limitations and findings: `OPEN_QUESTIONS.md`.
