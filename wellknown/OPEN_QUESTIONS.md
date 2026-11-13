# wellknown open questions

1. **Specificity.** RFC 9309 §2.2.2: "the most octets". We count the
   octets of the normalized pattern, wildcards included (as Google's
   parser does), so `/*.php` and `/x.php` tie and allow wins.
2. **`$` inside a pattern** is a literal; only a final `$` anchors.
3. **Percent-encoding.** Both sides are normalized: `%XX` of an
   unreserved character is decoded, other `%XX` get upper-case hex, and
   octets outside printable ASCII are encoded. The §2.2.2 table row that
   matches `https://` in a robots.txt path against `https%3A%2F%2F` in the
   URL would need reserved characters encoded too; we do not, since that
   would also encode `/`.
4. **Grouping of non-rule lines.** `crawl-delay`, `sitemap` and unknown
   keys do not end a user-agent run; only allow/disallow (even empty)
   do. Typos such as `Disalow` are reported and ignored (Google accepts
   some).
5. **User-agent values** are reduced to their leading product token
   (`Googlebot/2.1` → `googlebot`); the crawler's own string is reduced
   the same way.
6. **Patterns not starting with `/` or `*`** are kept with
   `invalid_pattern` and can never match a path.
7. **security.txt**: web-only fields (Canonical, Policy, Hiring,
   Acknowledgments, CSAF) must be https URIs; Contact and Encryption may
   use any scheme except http. The PGP signature is not verified and
   content after it is ignored. The 8 MiB / 20,000-field bounds are the
   V2 defaults; the RFC sets none.
8. **Grades.** Both formats are read line by line, so the only grades
   produced are `complete` and `partial`; `Valid` reports RFC
   conformance separately.
