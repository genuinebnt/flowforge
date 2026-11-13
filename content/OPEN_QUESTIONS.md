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
