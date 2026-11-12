# netnorm

Target normalization of spec 4.2, shared by `scope` (decisions), `oam`
(de-duplication keys) and the tool adapters, so every component agrees on
what a target is. Layer 0; depends on `golang.org/x/net` for IDNA and the
public suffix list.

* `Hostname`: trim, drop one trailing dot, IDNA A-labels (UTS 46 lookup
  mapping, non-transitional), labels ≤ 63 and names ≤ 253 octets,
  characters `[a-z0-9-_]`, and names whose last label is numeric are
  rejected (`2130706433`, `0x7f.1`, `0177.0.0.1` are IPv4 tricks).
* `IP`: strict dotted-quad or IPv6, no zones, IPv4-mapped IPv6 unmapped.
* `CIDR`: host bits zeroed; mapped prefixes converted to IPv4.
* `ParseHost`: name or IP literal (bracketed IPv6 allowed).
* `ParseURL`: http/https only, userinfo rejected, scheme default port
  dropped, fragment dropped, unreserved escapes decoded, dot segments
  removed. RFC 3986 semantics, deliberately not WHATWG.
* `RegistrableDomain`: public suffix + 1, used for per-target rate-limit
  keys.

Every error means `invalid_target` to the scope algorithm.

Tests: `go test ./core/netnorm` (table tests, an idempotence property and
`FuzzNetnormURL`); scope's conformance data in `conformance/scope/normalize`
exercises the same rules end to end.
