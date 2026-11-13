# content: open questions and decisions

Ambiguities in `docs/V2_DESIGN.md` §5/§12 and the choices made. Each choice
is pinned by a test or conformance file named in its entry.

## Classification and scoring

1. **"Removals count at −½ of the positive weight, returns at −¼"**
   (§12.4) is read literally: a removed feature contributes −½ × the
   weight its addition would have, a returning one −¼ × it, so a change
   made only of removals or returns scores 0 after clamping. The one
   exception is a removed security header, which has its own positive
   weight (`removed_security_header`). Pinned by CONTENT-INV-11. If
   Sentinel's model meant "½ of the weight, still positive", only
   `removalFactor`/`returnFactor` change.
2. **Reason aggregation.** oam limits a reason weight to [−1, 1] and
   requires unique names, so contributions are summed per name, clamped
   and rounded to 4 decimals, and the score is the clamped sum of the
   reasons. Five new endpoints therefore weigh 1, not 2.
3. **Module changes are not level 3.** §12.6 makes module identity a noise
   filter ("an unchanged module is no change, wherever it moved"); level 3
   stays the feature-set difference (§5). A bundle whose modules all match
   is `reordered`; changed modules are reported in the summary and the text
   diff, and semantic changes come from features (endpoints, libraries …).
4. **Capture-level level 3.** Feature changes are computed over the body
   plus its derived documents, so an endpoint moving from `#script-1` to
   `#script-2`, or a renumbered inline script, is not a change.
   `#script-n` ordinals shift when a script is inserted; the shifted
   documents show text diffs only.
5. **Catalog class** (a new advisory against an unchanged library) is not
   produced here: advisory matching is deferred (D4). The constant is
   oam's.
6. **Cause** is decided per capture: `target` as soon as one raw body
   differs; otherwise `normalizer` if a normalizer version or rule table
   differs, else `parser` if an extractor set differs. A re-derive job that
   upgrades both reports `normalizer`.
7. **Newness of returns** needs the store's history of feature ids
   (`DiffOptions.History`); without it a returning feature is `added`.

## Coverage and integrity

8. **Degraded captures emit only the source and its redirects**, with
   coverage `failed`. §12.5 says a degraded capture "gets no diff and no
   withdrawal"; adding facts from a login or challenge page would attach
   the wrong page's links and forms to the URL, so none are added.
9. **Which families a page covers.** A complete HTML reading also covers
   the inline-script and inline-style families (the page parse determines
   the whole set of inline documents), so endpoints of a removed inline
   script are withdrawn. Header and redirect families are covered whenever
   the capture is complete.
10. **HTML parse errors** do not lower the grade (the WHATWG algorithm
    recovers from all of them); only the depth, attribute and size limits
    do. Any JavaScript diagnostic lowers the grade to partial.
11. **Thresholds:** catch-all = the same body (SimHash Hamming ≤ 3, the
    exact range of sketch's multi-index) at two other URLs of the host
    (three or more in all, §12.5), configurable through
    `IntegrityInput.CatchAllDistance`; template-heavy sites whose pages
    differ little may need a smaller distance. Mass removal = more than half
    of at least four previous features. Soft errors = a `<title>` naming an
    HTTP error or maintenance on a 2xx page after a 2xx page that did not.
12. **Challenge markers** are a table (`challengeMarkers`). Strong markers
    (Cloudflare challenge platform, Imperva, PerimeterX, DataDome, "verify
    you are human" …) degrade by themselves; weak ones (an embedded
    reCAPTCHA/hCaptcha/Turnstile widget, "access denied", "captcha") only
    with status 403, 429 or 503, so a real login form with a CAPTCHA is not
    a challenge (`TestContentIntegrityFlags`).

## Normalization

13. **HTML runs to a fixed point.** Some trees do not survive
    serialize-then-reparse (misnested lists and formatting elements,
    foreign content; htmlparse INV-3). The output is re-parsed and
    re-emitted until stable, at most 4 rounds (`html_not_converged`
    otherwise; never seen in fuzzing). This costs one extra parse per page.
14. **Charset.** Valid UTF-8 is parsed as UTF-8 whatever the declared
    charset, so the UTF-8 output re-normalizes identically; only invalid
    UTF-8 goes through BOM/charset/prescan decoding. A Shift_JIS page whose
    bytes happen to be valid UTF-8 would be misread.
15. **Inline documents are emptied in the page** (`<script ...></script>`)
    and normalized on their own; event handlers become one `#handlers`
    script of wrapper functions; `javascript:` URLs too.
16. **JS fallback.** Any diagnostic switches to line normalization (no
    masks), which never turns an invalid program into a valid one, so the
    output re-normalizes the same way. Directives are re-quoted with `"`
    by editing the program source (the printer copies directives from it).
17. **CSS invalid tokens** (bad strings, bad URLs) become fixed
    placeholders, strings cut by EOF are closed, an escape cut by EOF is
    dropped and a final hex escape gets its space (found by fuzzing; each
    would otherwise absorb the newline the normalizer appends).
18. **Learned header rules** are accepted but have no effect: the header
    block keeps only security headers, all of which are level-3 paths, so
    no learned rule may mask them.
19. **JSON throughput** (≈ 5 MB/s) is bounded by `diff.ParseJSON`, which
    reads through encoding/json tokens. A dedicated JSON tokenizer state
    machine would be several times faster; not done in this cut.
20. **Text normalization for TS/TSX/JSX** (D2) only normalizes lines; the
    indentation is kept.

## Extraction

21. **JS endpoint method pairing** looks at the next four string literals of
    the same callee for a `method`/`type` property (and the previous two
    for `XMLHttpRequest.open`). A method held in a variable is not
    resolved (default GET for fetch).
22. **js_literal heuristic:** strings starting with "/" and a letter, URL
    characters only, not a static asset; absolute URLs only when API-like
    (`api.` host, `/api`, `/v<n>`, `graphql`). A literal ending in "/"
    followed by `+` gets `{expr}`. Route-table *keys*
    (`{"/users/:id": handler}`) are not read; values (`{path: "/a"}`) are.
23. **Library identification** uses banner comments and a small table of
    version-carrying keys (jQuery). Minified bundles without banners are
    not fingerprinted by code shape (a v3 track).
24. **`includes_library` needs a File source** (oam label rule), so a meta
    generator on a page is a feature only.
25. **GraphQL SDL** roots are found by their conventional names (`type
    Query` …); `schema { query: X }` is not resolved. Schema documents are
    assumed to describe `POST <origin>/graphql`.
26. **Secret features without a hasher** are keyed by rule, preview and
    length (collisions are possible); `Observe` emits exposed_secret tags
    only with a `KeyedHasher`, because the graph requires the keyed hash.
27. **Parameters** become tags only on endpoints present in the same
    observation; GraphQL operations found in JS have no endpoint link.

## Lineage and modules

28. **Linear lineage scan.** sketch's multi-index is exact only to
    Hamming 3 and lineage needs 6; a page's previous scripts are few, so
    `FindPredecessor` scans them all (exact at any threshold).
29. **JS fingerprints erase bound identifiers** (all become `$`), so a
    minifier's renaming does not move the sketch; free identifiers
    (`fetch`, `window`) and property names stay.
30. **Positional renaming** renames identifiers bound anywhere in the
    program, outside member and object-key positions. A method name that is
    also a local binding elsewhere is renamed too, which can make a module
    look changed when only another module's local was renamed.
31. **Vendor modules** are those with a `node_modules/` id, a hash in
    `SplitOptions.VendorHashes`, or a library banner inside; their changes
    are suppressed and the library version change is reported by
    `js.libraries`.

## Algorithms

32. **DiffBySize threshold is 16 KiB**, lower than the §5 4 MiB token cap:
    token-level Myers on a reordered bundle runs into the 10,000-edit cap
    before falling back (`BenchmarkContentAlgo/diff`).
33. **docs/ALGORITHMS.md** needs the "Choosing an algorithm" entries of
    README.md; that file is outside this package's write scope.

## Decoding (codec)

34. **Decoding goes through `codec`** with unchanged behaviour. Gzip
    sitemaps (xml@1) inflate with `codec.Inflate` capped at
    `MaxResponseBytes` and the ratio guard off: the cap already bounds the
    work and the xml@1 rule text ("inflated to 8 MiB") is pinned by
    `normalizers.json`, so a guard would need a version bump. A bad gzip
    header still grades failed, a stream broken later partial
    (`gzip_invalid`) with the inflated prefix, and a stream over the bound
    partial (`input_truncated`) with exactly the bound's prefix. JWT headers
    decode with lenient unpadded base64url, which, like
    `RawURLEncoding`, ignores non-zero trailing bits; the segment scanner
    only passes URL-alphabet characters, so the remaining lenient
    allowances (white space, '+', '/') never apply. Content has no
    percent-decoding of its own: URLs go through `net/url` parsing and
    query parameter names are kept as written, so nothing else moved.
35. **`DecodeBlobs` is new and not wired into `Process`.** V2 §12.14 asks
    for blobs to be decoded before indexing; that is the content store's
    indexing step, so `DecodeBlobs` returns the decoded, redacted text for
    it and leaves `Processed`, the normalized snapshots and their
    conformance hashes alone. Secrets found inside decoded blobs are
    redacted but not reported as findings (a new finding source would
    change scores and observations); pinned by CONTENT-INV-17.
36. **Redact rescans until stable.** Secret rules check the bytes around a
    match (a scheme glued to a letter or digit is not a URL). A
    placeholder ends in `]` where the secret ended in an alphanumeric, so
    redacting one secret can expose a neighbour the first scan rightly
    skipped (`ABIA0000000000000000postgres://:0@`, found by
    contentstore's fuzzer). Redact rescans its output and folds new
    findings back into input offsets until a scan is empty. That makes
    CONTENT-INV-10 idempotence hold by construction, at the cost of
    redacting the exposed neighbour too (the conservative choice).
    `FuzzContentRedactIdempotent` pins it.
37. **Form submission follows the submitter (html.forms@2).** WHATWG HTML
    §4.10.21.3 takes the action, method and enctype from the submitter's
    `formaction`, `formmethod` and `formenctype` when present (§4.10.18.6),
    and only submit buttons are submitters: `button` of type submit or in
    the auto state (missing or invalid type, no `command`/`commandfor`),
    `input type=submit|image`. html.forms@1 took a `formaction` from any
    `button` or `input`, kept the form's method unless `formmethod` was
    post (a `formmethod=get` button in a POST form still extracted POST),
    and ignored `formmethod`/`formenctype` without a `formaction`. Version
    2 extracts the form's own submission exactly as version 1 did (so ids
    and attributes of forms without overrides are unchanged; it is what
    `form.submit()` sends) and one more form feature, with its parameters,
    per further distinct (action, method, enctype) a submit button
    produces; a dialog method never submits, but a POST button in a
    dialog form does. Parameters keep version 1's rules (the submitter's
    own name=value is not a parameter), and only descendants of the form
    are read: controls associated through a `form` attribute from
    elsewhere in the document are not, nor are descendants owned by
    another form through one. TestRegress_FormSubmitterOverrides.
38. **All-hex build hashes need no digit (html@2, css@2).** `isHashLike`
    required a digit in an all-hex file-name part, so an 8-character hex
    hash without one (probability (6/16)^8, about one deploy in 2,600 per
    bundle) was neither masked in normalized text nor stripped from the
    lineage key: the deploy looked like a removed script and a new one.
    CONTENT-INV-02 found it (FF_SEED=1794593622660195640,
    `main.fcbfaeeb.js`; TestRegress_DigitlessHexBuildHash). Any all-hex
    part of 8 to 64 characters is now hash-like (English words of eight
    letters from a-f alone are practically nonexistent). The normalizers
    that mask URLs (html, css) and the extractors whose keys go through
    `LineageKey` (html.scripts, html.styles, js.sourcemap, js.modules,
    css.urls) moved to version 2, which changes the derivation stamp, so
    stored snapshots are re-derived. Still open: vite's 8-character
    base64url hashes have no digit about a quarter of the time and, when
    they are letters only, are not hash-like (a letters-only part is
    indistinguishable from a word such as `analytics`).
