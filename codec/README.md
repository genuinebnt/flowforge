# codec

Pure encoders and decoders for content extraction and search: base64,
base32, hex, percent- and form-encoding, quoted-printable, JavaScript string
escapes, bounded decompression (gzip, zlib, raw DEFLATE, zstd), blob
detection and bounded recursive decoding. Layer 1: imports only the
standard library, `core` and `github.com/klauspost/compress` (zstd). No
I/O, clock, randomness or goroutines. HTML character references belong to
`htmlparse`.

**Spec:** `docs/V2_DESIGN.md` §1 (portability rules) and §12.14 ("gzip and
base64 blobs found in content are decoded (bounded) before indexing"); each
codec follows the RFC or standard named below.

## API

| Codec | Functions | Governing text | Modes |
|---|---|---|---|
| base64 | `EncodeBase64`, `DecodeBase64`, `Append*`, `Base64Options{Alphabet, NoPadding, Mode, Algo}` | RFC 4648 §4 (std), §5 (URL), §3.2 padding, §3.5 canonical | strict: canonical only (exact padding, zero trailing bits, no white space, one alphabet). lenient: ASCII white space skipped, padding full/partial/absent, trailing bits ignored, both alphabets |
| base32 | `EncodeBase32`, `DecodeBase32`, `Base32Options{Alphabet: Base32Std\|Base32Hex, NoPadding, Mode}` | RFC 4648 §6, §7 | case-insensitive in both; strict canonical structure; lenient as base64 plus surplus `=` |
| hex | `EncodeHex`, `DecodeHex`, `HexOptions{Upper, Mode}` | RFC 4648 §8 | case-insensitive; lenient skips white space between pairs |
| percent | `EncodePercent`, `DecodePercent`, `PercentOptions{Set, Mode, RejectInvalidEscapes}`, `PercentSet` (`SetComponent` default, `SetUnreserved`, `SetC0Control`, `SetFragment`, `SetQuery`, `SetSpecialQuery`, `SetPath`, `SetUserinfo`, `SetForm`) | RFC 3986 §2.1; WHATWG URL §1.3 | strict: exactly `EncodePercent(x, Set)` (upper-case hex, every byte of the set escaped, nothing else). lenient: WHATWG percent-decode (`%` not starting a valid escape is literal); `RejectInvalidEscapes` turns that into an error (RFC 3986 syntax, = `net/url.PathUnescape`) |
| form | `ParseForm`, `EncodeForm`, `FormPair`, `AppendEncodeFormComponent`, `AppendDecodeFormComponent` | WHATWG URL §5.1, §5.2; Encoding Standard UTF-8 decode | strict: exactly serializer output. lenient: WHATWG parser with U+FFFD replacement per maximal subpart |
| quoted-printable | `EncodeQP`, `DecodeQP`, `QPMaxLine` | RFC 2045 §6.7 rules 1-5 | strict: the RFC's MUSTs (CRLF only, upper-case escapes, no trailing white space, lines ≤ 76). lenient: rule 3 robust decoder ⊇ `mime/quotedprintable` |
| JS strings | `EncodeJSString`, `DecodeJSString`, `JSOptions{Mode, Quote, Surrogates}` | ECMA-262 §12.9.4, Annex B.1.2 | strict: strict mode code. lenient: legacy octal, `\8 \9`, malformed escapes kept |
| inflate | `Inflate`, `AppendInflate`, `InflateOptions{MaxOutput, MaxRatio, RatioFloor}`, `DetectCompression` | RFC 1950, 1951, 1952, 8878 | cap = min(MaxOutput, max(RatioFloor, MaxRatio × len(src))); defaults 8 MiB, 100×, 1 MiB |
| detection | `Sniff`, `SniffOptions`, `Candidate`, `LooksLikeText` | this package | base64 runs ≥ 24 data chars (mixed case, a digit or symbol, ≥ 16 distinct, one alphabet, MIME/PEM line wrapping joined), hex runs ≥ 32 with digits and letters, gzip/zlib/zstd magic at offset 0 |
| layers | `DecodeLayers`, `LayersOptions{MaxDepth, MaxTotal, MaxNodes, Sniff, Inflate}`, `Layers`, `Layer` | V2 §12.14 | arena tree (`Parent` indexes), pre-order; depth 3, 8 MiB total, 256 nodes by default; keeps only text or compressed layers |

Common: `Mode` (`Strict` is the zero value, `Lenient`), `Kind`
(`KindBase64`, `KindBase64URL`, `KindHex`, `KindGzip`, `KindZlib`,
`KindDeflate`, `KindZstd`), `Base64Algo` (`ParseBase64Algo`, `Base64Algos`).

**Errors** are `*Error{Code, Offset}` wrapping a sentinel: `ErrInvalid`
(`invalid_byte`, `bad_padding`, `trailing_bits`, `truncated`,
`invalid_escape`, `non_canonical`, `line_too_long`), `ErrCorrupt`
(`bad_header`, `corrupt`), `ErrOutputLimit` (`output_limit`,
`ratio_limit`; the text is `codec: output limit`), `ErrUnsupported`
(`unsupported`); `ErrUnknownAlgorithm` for configuration names. For text
codecs `Offset` is a byte offset in the input; for streams it is the number
of output bytes returned with the error. A failing `Append*` text decoder
returns `dst` unchanged; `Inflate` returns the prefix it produced.

## Invariants (port these)

| Id | Test | Statement |
|---|---|---|
| CODEC-INV-01 | `TestCodecInv01_RoundTrip` | `decode(encode(x)) == x` in both modes for every codec and variant (JS over UTF-8/WTF-8, forms over UTF-8 pairs), and `Inflate` inverts the reference gzip, zlib, DEFLATE and zstd encoders. |
| CODEC-INV-02 | `TestCodecInv02_StrictCanonical` | For base64, base32, hex, percent (every set), form components and forms, strict decoding accepts `s` iff `s == encode(decode(s))` (base32 and hex up to ASCII case). |
| CODEC-INV-03 | `TestCodecInv03_StrictConformsSpec` | Quoted-printable and JS strict modes accept every encoder output (QP lines ≤ 76) and reject each planted violation of RFC 2045 §6.7 / ECMA-262 strict mode with its code and offset; lenient QP and JS decoding are total. |
| CODEC-INV-04 | `TestCodecInv04_LenientSuperset` | Whatever strict accepts, lenient accepts with the same output. |
| CODEC-INV-05 | `TestCodecInv05_StdlibAgreement` | Differential against `encoding/base64` (strict both ways, lenient ⊇ stdlib), `encoding/base32` (lenient ⊇), `encoding/hex` (equal), `net/url` (`PathUnescape` equal to the RFC 3986 check, `QueryUnescape`/`ParseQuery` ⊆ form, both invert our encoders), `mime/quotedprintable` (lenient ⊇; it inverts `EncodeQP`; strict accepts its binary writer), `encoding/json` string escapes, and `compress/*` + klauspost zstd on intact and corrupted streams. |
| CODEC-INV-06 | `TestCodecInv06_Base64AlgosAgree` | Every `Base64Algo` candidate returns identical bytes, or the identical error code and offset, for every option set and input. |
| CODEC-INV-07 | `TestCodecInv07_OutputLimits` | `Inflate` never returns more than its cap; at the cap it returns exactly that prefix with `ErrOutputLimit` (bombs: 12 MiB of zeros through every format, a 1 GiB zstd RLE frame); below it the output is complete. Text decoders never output more bytes than they read. |
| CODEC-INV-08 | `TestCodecInv08_LayersBounded` | `DecodeLayers` stays within `MaxDepth`, `MaxTotal` and `MaxNodes` with a consistent arena; a payload wrapped k ≤ MaxDepth times (base64, base64url, hex, gzip, zlib, zstd in random order) is found as text at depth k with that path; a gzip-in-gzip bomb stays within the total. |
| CODEC-INV-09 | `TestCodecInv09_SniffSound` | `Sniff` returns exactly the planted base64/hex blobs in filler text with exact spans; on any input candidates are sorted, disjoint, in bounds and decode with their kind. |
| CODEC-INV-10 | `TestCodecInv10_Determinism` | Same input, same bytes: encoders, decoders, `Sniff`, `DecodeLayers`; `Append*` appends exactly what the plain function returns, whatever `dst` holds. |
| CODEC-INV-11 | `TestCodecInv11_StableErrors` | Every error is an `*Error` with a code from the closed list unwrapping to its sentinel, the offset is within the input (text) or equals the returned length (streams), and failing text decoders leave `dst` unchanged. |

Fuzz targets assert the same invariants on arbitrary input:
`FuzzCodecBase64` (02, 04, 06, 07, 11), `FuzzCodecBase32`, `FuzzCodecHex`,
`FuzzCodecPercent` (also `ParseForm`), `FuzzCodecQP` (agreement with
`mime/quotedprintable`), `FuzzCodecJSString`, `FuzzCodecInflate` (cap,
reference agreement), `FuzzCodecSniff` (09), `FuzzCodecDecodeLayers` (08).

## Conformance data (`conformance/codec/`)

One schema for every file (`confFile` in `conformance_test.go`): `codec`,
`op` (`decode`, `encode`, `both`), `mode`, `encoded`/`encoded_hex`,
`decoded`/`decoded_hex`, `error` (code name) and `offset`, plus `pairs`,
`quote`, `reject_invalid`, `max_output`/`max_ratio`/`ratio_floor`,
`candidates`, `layers`, `max_depth`/`max_total`/`truncated`.

| File | Content |
|---|---|
| `rfc4648.json` | RFC 4648 §10 vectors (base64, base32, base32hex, base16) |
| `base64.json`, `base32_hex.json` | padding, trailing bits, truncation, white space, alphabets; base64 cases run on every candidate |
| `percent.json`, `form.json` | every percent-encode set, canonical checks, WHATWG invalid-escape and UTF-8 replacement behaviour |
| `qp.json` | RFC 2045 §6.7 examples and each rule |
| `jsstring.json` | ECMA-262 §12.9.4 escapes, surrogates, Annex B.1.2, strict errors |
| `inflate.json` | gzip (one and two members), zlib, DEFLATE (stored block), zstd vectors; bad header, checksum, truncation, dictionary, limits |
| `sniff.json`, `layers.json` | detection and recursive decoding results |

## Choosing an algorithm

`docs/ALGORITHMS.md`, "Base64 decoding (`codec`)". Base64 decoding has three
candidates behind `Base64Options.Algo` (`ParseBase64Algo`: `statemachine`,
`swar`, `stdlib`); `go test -run '^$' -bench CodecAlgo -benchmem ./codec`
compares them (`BenchmarkCodecAlgo/<candidate>/<size>[-mime]`). Measured
2026-11-13 on the 4-vCPU Xeon development machine (Go 1.26), two runs,
decoded MB/s:

| Candidate | 64 B | 4 KiB | 1 MiB | 1 MiB MIME-wrapped, lenient |
|---|---|---|---|---|
| `swar` (**default**) | 1,303-1,306 | 1,804-1,958 | 1,853-2,035 | 963-1,066 |
| `statemachine` | 595-598 | 753-760 | 579-753 | 480-493 |
| `stdlib` | 562-639 | 817-864 | 749-836 | 491-494 |

## Benchmarks

`go test -run '^$' -bench . -benchmem ./codec` (same machine, MB/s of
input; decoders on their own encoding of 1 MiB):

| Benchmark | Result |
|---|---|
| encode base64 / base32 / hex | 1,751 / 347 / 1,634 |
| encode percent / QP / JS (text) | 857 / 335 / 390 |
| decode base32 strict / lenient | 707 / 627 |
| decode hex strict / lenient | 1,262 / 1,266 |
| decode percent strict / lenient | 509 / 589 |
| decode QP strict / lenient | 380 / 458 |
| decode JS strict / lenient | 321 / 343 |
| inflate gzip / zlib / deflate / zstd (MB/s of output) | 371 / 296 / 343 / 636 |
| 1 GiB zstd RLE bomb stopped at the 3.3 MB ratio cap | 3.7 ms |
| `Sniff` / `DecodeLayers` over a 1 MiB bundle with blobs | 239 / 175 |

Baselines: `go run ./cmd/xtask bench-module codec` →
`bench/results/codec/`.

## Running

```sh
go test ./codec/...
FF_SEED=1 FF_PROPTEST_CASES=100000 go test ./codec -run Inv
go test ./codec -run '^$' -fuzz FuzzCodecBase64 -fuzztime 60s   # and the other Fuzz targets
go test ./codec -run '^$' -bench . -benchmem
```

## Known limitations

See `OPEN_QUESTIONS.md`: base32 and hex strictness is up to case, the
stdlib's base32 quirks are not reproduced, compressed-stream error offsets
are implementation-defined, Sniff heuristics miss short or digit-free
blobs, and DEFLATE/gzip/zlib come from `compress/flate` rather than an
in-package state machine.
