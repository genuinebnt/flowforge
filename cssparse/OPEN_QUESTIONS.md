# cssparse open questions

1. **Declaration whitespace.** CSS Syntax 3 (2021 CR and the current ED)
   trims whitespace after the colon and at the end of a declaration's
   value. The vendored css-parsing-tests (used by rust-cssparser and
   tinycss2) expect it kept (`"foo: 9000  !Important"` → `[" ", 9000,
   " "]`). We follow the suite: the tree is lossless and normalizers can
   trim. Pinned by `one_declaration.json`.
2. **Legacy tokens.** The suite still expects `<unicode-range-token>` and
   the match/column tokens of the 2014 CR. The current spec removed them
   (unicode ranges are re-parsed from ident/number/dimension tokens).
   They are produced only with `Options.LegacyTokens`; the default follows
   the current spec, pinned by `conformance/cssparse/tokens.json`.
3. **Ident code points.** We use the 2021 CR definition (every code point
   ≥ U+0080 is an ident code point), which the suite expects
   (`"\u007F\u0080\u0081"` → delim, ident). The 2024 ED narrowed the
   range; adopting it would change the conformance data.
4. **EOF markers.** The suite emits `["error", "eof-in-string"]` /
   `["error", "eof-in-url"]` after unterminated tokens; we model this as
   `FlagUnterminated` plus a diagnostic, and the test renderer emits the
   marker. The token itself is the spec's string/url token.
5. **Not vendored:** `stylesheet_bytes.json` (needs WHATWG encoding
   labels; decoding to UTF-8 is the caller's job here), `An+B.json` and
   `color_*.json` (value-level grammars, not syntax).
6. **Depth limit recovery.** A block that would nest deeper than
   `MaxDepth` (64) is consumed to its matching close (mirror-aware, as the
   real algorithm would) and represented by one `DiagDepthLimit` error
   node; its tokens stay in the token list (tiling is unaffected).
7. **Stray `}` in block contents.** Only reachable when parsing a source
   text with `ParseBlockContentsSource`; it is discarded with
   `unexpected_close` so the loop always advances (the ED returns
   there).
8. **Numbers out of range.** "Convert a string to a number" is
   unspecified for magnitudes beyond binary64; we return ±Inf or 0 from
   correctly rounded parsing (PORT: Rust `f64::from_str` agrees).
9. **URL extraction scope.** `@font-face` and other at-rule blocks are
   re-parsed with the ED block-contents algorithm; style-rule blocks are
   walked as raw component values (no `@font-face` can nest in them).
   `bad-url` tokens are not references. Strings count as URLs only as the
   first `@import` component, in `@namespace`, and directly inside
   `image-set()`.
