// Package codec is the pure encoder/decoder library used by content
// extraction and search (docs/V2_DESIGN.md §12.14: gzip and base64 blobs
// found in content are decoded, bounded, before indexing). Layer 1: it
// imports only the standard library, core and
// github.com/klauspost/compress (zstd).
//
// Codecs and their governing specifications:
//
//   - base64, standard and URL alphabets, padded and unpadded (RFC 4648
//     §4, §5): EncodeBase64, DecodeBase64; decoder candidates
//     Base64AlgoStateMachine, Base64AlgoSWAR, Base64AlgoStdlib;
//   - base32 and base32hex (RFC 4648 §6, §7), base16 (§8):
//     EncodeBase32/DecodeBase32, EncodeHex/DecodeHex, case-insensitive;
//   - percent-encoding (RFC 3986 §2.1, WHATWG URL §1.3 percent-encode
//     sets and percent-decode) and application/x-www-form-urlencoded
//     (WHATWG URL §5): EncodePercent/DecodePercent, EncodeForm/ParseForm;
//   - quoted-printable (RFC 2045 §6.7): EncodeQP/DecodeQP;
//   - JavaScript string-literal escapes (ECMA-262 §12.9.4 and Annex
//     B.1.2): EncodeJSString/DecodeJSString;
//   - bounded decompression of gzip (RFC 1952), zlib (RFC 1950), raw
//     DEFLATE (RFC 1951) and zstd (RFC 8878): Inflate, with an output
//     cap and a decompression-ratio guard;
//   - blob detection (Sniff) and bounded recursive decoding
//     (DecodeLayers).
//
// HTML character references are not here: htmlparse owns them.
//
// Decoders are explicit state machines whose state names follow the
// governing specification. Text decoders have a Strict mode, which
// accepts exactly the canonical encodings (or, for quoted-printable and
// JavaScript, exactly the inputs the specification's MUSTs allow), and a
// Lenient mode for real-world input that accepts a superset with the
// same output. Errors are *Error values with a stable Code from a closed
// list, wrapping one of the sentinel errors (ErrInvalid, ErrCorrupt,
// ErrOutputLimit, ErrUnsupported). Every function is deterministic and
// allocation-bounded by its input or by its stated limits; no function
// starts a goroutine or reads a clock.
//
// The numbered invariants CODEC-INV-01..11 (README.md) are what the Rust
// port reimplements.
package codec
