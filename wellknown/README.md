# wellknown

robots.txt (RFC 9309) and security.txt (RFC 9116) for Flowforge v2
(`docs/V2_DESIGN.md` §4.3). Layer 1: standard library and `core` only.
Sitemaps are in `xmlparse/sitemap` (layer-1 packages cannot import each
other).

## robots.txt

* `ParseRobots` — line-oriented state machine (`robotsStateStart`,
  `robotsStateAgents`, `robotsStateRules`, the §2.2 group grammar):
  consecutive user-agent lines share a group, the first rule ends the
  run, blank lines and comments do not split groups, rules before any
  user-agent are ignored, sitemap lines are global, `crawl-delay` is
  kept per group as an extension (integer milliseconds).
* `Allowed` / `Match` — product-token group selection (exact, case-
  insensitive, groups combined; otherwise `*`), §2.2.2 percent
  normalization of path and pattern, `*` and end-anchoring `$`, longest
  pattern wins, allow wins ties, `/robots.txt` always allowed.
* Limits: 500 KiB parsed (§2.5; the rest ignored, cut at a line
  boundary), 20,000 rules + sitemaps → `partial`.

## security.txt

* `ParseSecurityTxt(data, now, opts)` — fields (§2.5: Acknowledgments,
  Canonical, Contact, Encryption, Expires, Hiring, Policy,
  Preferred-Languages, plus CSAF), case-insensitive names, unknown fields
  kept, `Expires` required, single and checked against `now` (the clock
  is never read), more than a year ahead is a warning, https required
  for web URIs. OpenPGP cleartext framing (RFC 4880 §7, states
  `pgpStart` … `pgpTrailer`) is recognized and stripped, including
  dash-escaping; the signature is not verified.
* `Valid` (no error-level diagnostic), `CanonicalMatches`.
* Limits: 8 MiB, 20,000 fields → `partial`.

## Conformance

`conformance/wellknown/robots.json` (RFC 9309 §5.1 and §5.2 examples,
the §2.2.2 encoding table, grouping, limits) and `securitytxt.json`
(the RFC 9116 example, signed form, every diagnostic).

## Invariants (port these)

| Id | Test | Statement |
|---|---|---|
| WK-INV-01 | `TestWKInv01_RobotsMatchesReference` | On generated robots.txt files, `Allowed` agrees with a brute-force reference implementation of RFC 9309 semantics for random crawlers and paths. |
| WK-INV-02 | `TestWKInv02_PatternMatcherMatchesBruteForce` | The backtracking pattern matcher equals exhaustive wildcard matching. |
| WK-INV-03 | `TestWKInv03_Totality` | Any byte string parses as both formats; diagnostics carry in-range line numbers; every group has an agent. |
| WK-INV-04 | `TestWKInv04_LimitsGradePartial` | Byte and item limits grade both formats `partial`, never `complete`, and no more items than the limit are kept. |
| WK-INV-05 | `TestWKInv05_ExpiresAgainstArgument` | `expired` holds exactly when Expires ≤ now; results are deterministic for the same input and time. |
| WK-INV-06 | `TestWKInv06_SignedFramingTransparent` | Wrapping a file in a cleartext signature (with dash-escaping) gives the same fields and diagnostics, with `Signed` set. |
| WK-INV-07 | `TestWKInv07_LinearParse` | robots.txt parsing allocations at 4n ≤ 4.4x of n; time per byte ≤ 1.5x. |

Fuzz targets: `FuzzRobots` (INV-01 against the reference on the parsed
rules, INV-04), `FuzzSecurityTxt` (determinism, INV-04, Valid implies
Contact and a live Expires).

## Rust mapping

The robots matcher follows Google's `robotstxt` reference (and the
`robotstxt` Rust port) in pattern length as specificity, ties to allow,
and leading-token user-agent matching. PORT: `time.Parse(time.RFC3339)`
accepts fractional seconds; use `chrono::DateTime::parse_from_rfc3339`.

## Running

```
go test ./wellknown/
go test ./wellknown/ -run XXX -fuzz FuzzRobots -fuzztime 20s
go test ./wellknown/ -run XXX -bench . -benchmem
```

Benchmarks (shared 4 vCPU Xeon): robots.txt 83–129 MB/s, one decision on
a 64 KiB file ~13 µs, security.txt ~106 MB/s.

## Limitations

See `OPEN_QUESTIONS.md`.
