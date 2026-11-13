# services/egressproxy

The scope-checked egress proxy (spec 3.2, 4.4, 4.5; V2_DESIGN §9): the tool
runner's only way out. `Proxy` serves

* `ServeHTTP`: CONNECT (any TCP port) and plain-HTTP forward requests, plus
  the control endpoint `POST http://egress.flowforge.invalid/v1/tickets`;
* `ServeSOCKS`: SOCKS5 (RFC 1928, `socks5` package), CONNECT only;
* `ServeDNSUDP` / `ServeDNSTCP`: a DNS forwarder (RFC 1035, `dnsmsg`).

## Authorization (every listener)

1. The caller is authenticated: the internal token in `Proxy-Authorization`
   (407 otherwise), or a **ticket** for SOCKS5 and DNS. A ticket is issued by
   the control endpoint after the same validation as a proxied request
   (token, decision, run, effect path, declared ports, kill switch); it is
   bound to all of them and lives at most 60 s. SOCKS5 sends it as
   username = decision id, password = ticket (RFC 1929 caps both at 255
   octets, too short for an internal token); DNS sends it in EDNS option
   65280 (local-use range, RFC 6891 §9). Tickets are per replica, in memory.
2. The decision named by `X-Flowforge-Decision-Id` (or the ticket) is loaded
   for the caller's tenant: an allow, at most `MaxDecisionAge` (1 h) old and,
   when a run id is sent, of that run. It is reloaded on every use.
3. **The decision must cover (host, port).** A URL decision covers its host
   and its URL's port (and only tunnels under an https decision). A
   host-level decision (Host or IP; the scope service decides hosts, not
   ports) covers its host for TCP to the ports of `PortPolicy` for the
   program (default 80 and 443; the dev stack uses `CommonPorts`), narrowed
   to the ports the tool call declared (`X-Flowforge-Ports`, bound into the
   ticket for SOCKS5). Anything else is `port_not_allowed`. Userinfo,
   mismatched `Host`, un-normalizable hosts and IP literals under a name
   decision are refused (permission attaches to the name, spec 4.5).
4. **Intelligence sources.** When the host is on the allowlist (`Intel`,
   default `DefaultIntelSources()`: crt.sh, IANA bootstrap, rdap.org, the
   RIR and large gTLD RDAP servers, Team Cymru) and the decision is of class
   passive, the source is the target (V2_DESIGN §8): its configured or
   resolved public addresses are dialled and each connection (and each
   allowlisted DNS name) is recorded in the decision log with reason
   `intel_source`. Other classes get `intel_requires_passive`. Redirects of
   such requests must stay on the allowlist.
5. Kill switches (fail closed when unknown).
6. **Per-target rate limit**: GCRA (`limiter`, monotonic time from the
   injectable `Clock`) on `rl:h:<program>:<registrable domain or ip>` at the
   scope document's rate (`DefaultRate` when unset; an intel source may set
   its own), plus a concurrency cap per key held for the request or tunnel's
   lifetime. It applies to every HTTP request, CONNECT tunnel and SOCKS5
   tunnel, so a tool's internal parallelism cannot exceed the program rate.
   Denials answer 429 with `Retry-After` and `X-Flowforge-Retry-After-Ms`
   (SOCKS5: reply 2) and are logged and counted
   (`ff_limiter_denied_total{level="proxy_host"|"proxy_host_concurrency"}`).
7. Only the decision's pinned addresses are dialled, never re-resolved
   (DNS rebinding cannot redirect a connection); loopback, private,
   link-local (169.254.169.254), CGNAT, multicast and unspecified addresses
   need an explicit cidr rule. Plain-HTTP redirects are re-checked with the
   scope service (`X-Flowforge-Redirect-Decision`).

Every refusal answers 403/407/429/502 with `X-Flowforge-Refused: <reason>`
(SOCKS5: reply 2, 5 or 7; DNS: REFUSED), logs `egress.refused` and counts
`ff_egress_refused_total{reason}`.

## DNS forwarder policy

A query is answered only when it carries a valid ticket whose decision is
still an allow, no kill switch is engaged, and the name passes a **passive**
scope check (DECISIONS D3; recorded by the scope service; reverse names are
checked as the IP they denote) or is an allowlisted intelligence source for a
passive decision (recorded as `intel_source`). Everything else is `REFUSED`;
ANY is `NOTIMP`; malformed queries get `FORMERR`. Answers come from the
injectable `Upstream` (`ServerUpstream`, `NetUpstream`, `ResolverUpstream`,
`RecordsUpstream`); records of a CNAME chain are kept only up to the first
hop that fails the same check; at most 64 records; UDP answers are cut to
the client's EDNS size (at most 1232 octets, 512 without EDNS) with TC set,
TCP answers to 16 KiB.

## Tests

`proxy_test.go` is the M7 mock-target test (allowed request, redirect to an
out-of-scope host, DNS rebinding, userinfo / decimal-IP / suffix tricks,
missing and unknown decisions, no token, kill switch, CONNECT to another
host). `v2_test.go`: CONNECT to an allowed host:port works and other or
undeclared ports are refused; SOCKS5 allowed, other host, IP literal,
undeclared port, forged ticket, BIND (reply 7), and DNS rebinding through
SOCKS5 still reaching only the pinned address; the DNS forwarder over real
UDP and TCP queries (allowed names, CNAME chains cut at an out-of-scope hop,
PTR, REFUSED without or with a forged ticket, intel names only for passive
decisions); the rate limit holding a 12-way parallel portscan to the burst
with a frozen clock and to the rate after one second, the concurrency cap
and 429 on plain HTTP; intel sources reachable only by passive decisions,
recorded as `intel_source`, with `ctlog` end to end; `tlscert` through a
tunnel.

## Limitations

* Standalone (`cmd/egress-proxy`, wired in `services/internal/wire`) the
  proxy has the default port policy, no intel allowlist and no SOCKS5 or DNS
  listener; the dev stack configures all of them.
* The scope service pins addresses only for non-passive checks, so a
  passive decision (tlscert under D3) has no pins and its tunnels are
  refused (`no_pinned_ips`) until the scope check pins on request.
* Tickets and limiter state are per replica (no shared store).
* HTTPS is tunnelled: redirects inside TLS are re-checked by the tool
  runner's client only.
