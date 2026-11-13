# codec: open questions and decisions

Each choice is pinned by a test or a conformance case.

## Canonical forms

1. **Strict base32 and hex ignore letter case.** RFC 4648 §8 calls base16
   case-insensitive and the task asks for case-insensitive decoding, so
   "canonical" for these two means canonical up to ASCII case: the encoders
   write upper-case base32 and (by default) lower-case hex, and CODEC-INV-02
   compares after folding. Everything else (padding, trailing bits, white
   space) is strict.
2. **Strict percent-decoding is relative to a set.** RFC 3986 allows many
   spellings of one string; strict mode accepts exactly
   `EncodePercent(decoded, Set)`: upper-case hex (§2.1), every byte of the
   set escaped, no other byte escaped (§6.2.2.2). A "valid but not
   canonical" check is `Lenient` with `RejectInvalidEscapes`, which accepts
   exactly what `net/url.PathUnescape` accepts (CODEC-INV-05).
3. **Every percent-encode set contains `%`.** The WHATWG sets (URL §1.3) do
   not, because the URL parser keeps existing escapes; an encoder that
   leaves `%` raw does not round-trip, so it is added to every set (the
   component and form sets already contain it). The path set follows the
   2024 text with U+005E (`^`).
4. **Quoted-printable and JS strict modes are conformance, not
   canonicality** (CODEC-INV-03). Both formats allow many encodings of one
   value (`=41` and `A`, `\x41` and `A`), so strict means the MUSTs of RFC
   2045 §6.7 and ECMA-262 strict mode code. `EncodeQP` is still a fixed
   canonical form: CRLF pairs are hard breaks, lone CR/LF are `=0D`/`=0A`
   (so decoding restores the input exactly), white space before a break or
   the end is escaped, lines are filled greedily to 76.
5. **Lenient QP accepts what `mime/quotedprintable` accepts** with the same
   output, plus more (controls, a soft break with junk after it read as
   literal `=`, `=` at the end of the data). A bare LF decodes to LF and CR
   LF to CR LF, as Go does; RFC 2045 rule 4 would canonicalise to CRLF.

## Base64 and base32 details

6. **Lenient base64 accepts both alphabets in one input** (`+/-_`), full,
   partial (`Zg=`) or no padding, and ASCII white space anywhere. It rejects
   data after the padding and `=` at the start of a quantum, as the stdlib
   does.
7. **Go's base32 quirks are not reproduced.** Unpadded `encoding/base32`
   returns no bytes and no error for data lengths no encoder emits (1, 3 or
   6 mod 8), silently dropping the whole final group, and padded decoding
   ignores anything after the padding. Ours reports `truncated` and
   `bad_padding`; lenient mode does accept surplus `=` (which Go accepts
   too). The differential test skips exactly those inputs.
8. **Non-zero trailing bits** are `trailing_bits` in strict mode (RFC 4648
   §3.5) and ignored in lenient mode, like `encoding/base64` without
   `Strict()`.

## JavaScript strings

9. **jsparse is not reused.** It decodes escapes inside its lexer
   (`Lexer.scanEscape`, unexported) and is a layer-1 peer, which layer-1
   packages may not import. The decoder here follows the same productions;
   it differs in one place: an escaped surrogate pair in any escape form
   (`\u{D83D}\u{DE00}` too) combines into one code point, as ECMAScript's
   UTF-16 string value implies, while jsparse combines only `\uXXXX\uXXXX`.
10. **Unpaired surrogates** are WTF-8 by default (matching jsparse's cooked
    values, and round-tripping through `EncodeJSString`); `SurrogateReplace`
    writes U+FFFD for consumers that need valid UTF-8.
11. **Lenient keeps malformed escapes verbatim** (`\x4G` stays `\x4G`):
    ECMAScript has no recovery (they are SyntaxErrors), and verbatim text is
    the least surprising thing to index.

## Decompression

12. **DEFLATE, gzip and zlib use `compress/flate`, `compress/gzip` and
    `compress/zlib`**, zstd uses klauspost's decoder with concurrency 1 (a
    synchronous stream decoder: no goroutines). The bounded read loop, the
    caps and the error mapping are this package's; the inflate state
    machines are not rewritten. PORT: Rust uses `flate2` (miniz_oxide) and
    the `zstd` crate with the same caps; outputs agree because decoding is
    deterministic.
13. **Error offsets of corrupt streams are implementation-defined**: they
    count the output produced before the decompressor noticed, which
    depends on its buffering. Only the code is portable; conformance cases
    for `corrupt` give no offset.
14. **Default bomb guard: 100× once past 1 MiB.** Real text compresses
    3-30× and DEFLATE peaks near 1032×; a 1 MiB floor lets tiny, very
    repetitive inputs decode. Content's gzip sitemaps run with the guard off
    (content OPEN_QUESTIONS 34).
15. **zstd windows** up to max(cap, 8 MiB) are accepted; a frame declaring
    a larger window stops with `output_limit`, since its output could exceed
    the cap anyway.
16. **Trailing data** after the last gzip member is `corrupt` (RFC 1952
    §2.2: a file is a sequence of members); after a zlib trailer or a final
    DEFLATE block it is ignored, as the stdlib does.

## Detection and layers

17. **Sniff heuristics** (integer-only, so the Rust port gets identical
    answers): a base64 run needs ≥ 24 data characters, upper- and lower-case
    letters, a digit or one of `+/-_`, ≥ 16 distinct characters and one
    alphabet; a hex run needs ≥ 32 characters, even, with digits and
    letters. Camel-case identifiers, paths and numbers are rejected; random
    18-byte blobs are missed about 1 % of the time, longer ones almost
    never. Hex of hex (all digits) is not detected. Percent- and
    QP-encoded runs are not sniffed.
18. **Compression is only sniffed at offset 0** of a buffer: binary data
    cannot sit inside text, so a compressed blob is found as the decoding of
    a base64 or hex layer, or as the whole input.
19. **DecodeLayers keeps a layer only if it is text or compressed.** Binary
    payloads (images in data URIs, SRI digests, hashes) are dropped and not
    counted towards `MaxTotal`; a decode cut by a limit sets
    `Layers.Truncated` even when its prefix is dropped.
