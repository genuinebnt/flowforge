# sourcemap open questions

1. **Negative zero.** The VLQ `B` (sign bit set, magnitude 0) decodes to
   0, as in mozilla/source-map and the `sourcemap` crate. ECMA-426
   discussion has proposed reading it as -2^31; no generator emits it.
   Our encoder never produces it, so it only matters for non-canonical
   input (`conformance/sourcemap/vlq.json` pins the choice).
2. **Value range.** Every VLQ value and every resolved position or index
   must fit a signed 32-bit integer; larger values are `vlq_overflow` /
   `negative_value`. The crate accepts up to i64.
3. **Strictness choices.** Empty segments (`A,,A`, trailing `,`) and
   2/3/6+-field segments are errors; out-of-range source or name indices
   are errors (some tools tolerate them). A `sourcesContent` shorter than
   `sources` means "no content" for the rest; a longer one is ignored.
4. **sourceRoot and absolute sources.** ECMA-426 prepends sourceRoot
   to every source; like mozilla/source-map we leave absolute URLs
   (with a scheme) alone. A section's own `sourceRoot` is applied to its
   sources when flattening.
5. **Lookups.** `OriginalPositionFor` uses the greatest-lower-bound bias
   on the same generated line only; a 1-field segment is returned with
   `Source = -1` (the range maps to nothing). Equal positions resolve to
   the last one in encoded order.
6. **Limits.** The 20,000-facts bound is applied to sources
   (`MaxSources`): further sources and the mappings into them are
   dropped. There is no cap on mappings beyond the 8 MiB input bound
   (every segment costs at least one byte).
7. **Input over the limit** returns `partial` with a nil map (JSON cannot
   be read partially); callers must not treat it as "no sources".
8. **Duplicate JSON keys.** RFC 8259 leaves them undefined. We keep the
   last value (encoding/json); a serde-based port must do the same
   rather than reject, so both ports accept the same documents.
