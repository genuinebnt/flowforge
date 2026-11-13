# xmlparse

Non-validating XML 1.0 (Fifth Edition) + Namespaces in XML 1.0 parser
for Flowforge v2 (`docs/V2_DESIGN.md` §4.3). Layer 1: standard library
and `core` only. Subpackage `xmlparse/sitemap` reads sitemaps.

## What it implements

* `Decoder` — pull parser; `state` enum named after the productions
  (`stateMisc` [27], `stateContent` [43], `stateComment` [15],
  `stateCDSect` [18], `statePI` [16]/[23], `stateDoctype` [28],
  `stateSTag` [40]/[44], `stateETag` [42]). Events: XML declaration,
  doctype, start/end element (namespace-resolved names, normalized
  attributes), text, CDATA, comment, PI.
* `Parse` — arena `Document` (`Nodes []Node`, `NodeID int32`) built from
  the events; on error the partial tree plus the error.
* `(*Document).Canonical` — canonical writer modelled on C14N 1.0.
* Encodings: UTF-8 (BOM optional) and UTF-16 with BOM (transcoded; then
  offsets index the transcoded buffer). Declared encodings must match
  (`utf-8`, or `utf-16`/`utf-16le`/`utf-16be` for UTF-16 input).
* Security: only `<!DOCTYPE name>`; an internal subset or external ID is
  refused with `dtd_internal_subset` / `dtd_external_id` at its first
  byte. Only the five predefined entities and character references exist,
  so no reference expands beyond its own length.
* Limits: `MaxDepth` 64, `MaxInputBytes` 8 MiB → `depth_limit` /
  `input_too_large`, grade `partial`. Grades: `complete`, `partial`,
  `unsupported` (refused DOCTYPE or encoding), `failed` (not well-formed).

Error offsets: the first offending byte at character granularity —
`input[:Offset]` is still a prefix of some well-formed document,
`input[:Offset+w]` (w = width of the character there) is not, and later
bytes cannot change the error. Checks are therefore made incrementally
(end-tag names byte by byte, encoding names by prefix, namespace bindings
at the `>` or `/` that closes the tag).

`xmlparse/sitemap`: urlset and sitemapindex (loc, lastmod as W3C
Datetime, changefreq, priority), plain-text sitemaps, gzip input with
decompression capped at MaxBytes+1 bytes; defaults 20,000 entries / 8 MiB
(raisable up to the protocol's 50,000 / 50 MiB).

## Conformance

`conformance/xmlparse/{wellformed,errors,security,limits}.json` (spec
examples, error codes with exact offsets, XXE/billion-laughs/encoding
refusals, limits) and `conformance/xmlparse/sitemap/{protocol,limits}.json`
(the protocol's samples, field validation, extensions, gzip, zip bomb,
limits). No upstream XML suite was vendored (the W3C XML conformance
suite relies on DTDs and external entities, which this parser refuses by
design).

## Invariants (port these)

| Id | Test | Statement |
|---|---|---|
| XML-INV-01 | `TestXMLInv01_Totality` | Any byte string yields a tree and nil or a `*SyntaxError` with an in-range offset; EOF-class errors sit at the input length; the grade matches the error. |
| XML-INV-02 | `TestXMLInv02_FirstOffendingByte` | Every prefix of a well-formed document is error-free or ends in an EOF-class error; for an error at k, `[:k]` has no non-EOF error and `[:k+w]` (alone or followed by anything) gives the same code at k. |
| XML-INV-03 | `TestXMLInv03_DepthCap` | Nesting beyond MaxDepth stops with `depth_limit` (partial) and no tree path exceeds MaxDepth. |
| XML-INV-04 | `TestXMLInv04_CanonicalRoundTrip` | Generated documents (random escaping, quoting, CDATA, whitespace, namespaces) parse to the model's canonical form, and canonical form is a fixed point of parse-then-canonicalize. |
| XML-INV-05 | `TestXMLInv05_NoExpansionGrowth` | Decoded text, names and attribute values never exceed the input length; any internal subset is refused (`unsupported`). |
| XML-INV-06 | `TestXMLInv06_LinearSteps`, `TestXMLInv06_LinearTime` | Decoder steps ≤ 3·n + 16 on generated and adversarial input; tree allocations at 4n ≤ 4.4x of n; decoder time per byte at 4n ≤ 1.5x of n. |
| XML-INV-07 | `TestXMLInv07_LimitsGradePartial` | A result is `partial` exactly when a limit stopped it, never `complete` then. |
| XML-INV-08 | `TestXMLInv08_EventsNestAndStick` | Events nest with matching names, the tree has one node per content event, and the error is sticky. |
| SITEMAP-INV-01 | `TestSitemapInv01_ExactEntries` | Generated sitemaps (plain or gzip, with invalid entries and look-alike `<loc>` in extensions) yield exactly the valid entries in order. |
| SITEMAP-INV-02 | `TestSitemapInv02_LimitsGradePartial` | Entry/byte limits grade the read partial, entries never exceed the limit, decompression never inflates past MaxBytes+1. |

Fuzz targets: `FuzzXMLParse` (INV-01, 02, 04, 05), `FuzzSitemap`.

## Rust mapping

The event API mirrors `quick-xml`'s reader (pull events, borrowed
attribute buffer reused between calls), and the tree mirrors `roxmltree`
(read-only arena, namespace-resolved names, attribute ranges into one
`Vec`). Differences: no DTD support at all (roxmltree expands internal
entities; we refuse the subset), errors carry a first-offending-byte
offset, and depth/input limits are part of the contract. PORT: Go
strings are byte slices; Rust names should be `&str` into the input (or
`String` after reference decoding).

## Running

```
go test ./xmlparse/...
go test ./xmlparse/ -run XXX -fuzz FuzzXMLParse -fuzztime 20s
go test ./xmlparse/... -run XXX -bench . -benchmem
```

Benchmarks (shared 4 vCPU Xeon): decoder ~91–94 MB/s, `Parse` 24–26
MB/s, sitemap 26–31 MB/s, linear between n and 4n.

## Limitations

See `OPEN_QUESTIONS.md`.
