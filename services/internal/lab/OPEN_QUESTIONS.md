# services/internal/lab: open questions and findings

Findings the lab made about the packages it exercises, and the choices it
makes because of them.

1. **Fixed in `content` (TestRegress_RedirectBase):** references now
   resolve against the final URL. The tool workarounds below are kept
   because they are also correct behaviour. Original finding:
   **References of a redirected capture resolve against the requested
   URL.** `content.Process` resolves a capture's links, scripts and forms
   against `ProcessOptions.PageURL` or `Capture.URL`, never the final URL
   of the redirect chain (RFC 3986 §5.1.3: the base is the retrieval URI,
   the final one). For `http://h/` → `https://h/` every relative link of
   the page points at the plain-HTTP origin. The content tools work around
   it: crawl expands a redirected capture's target instead of its links;
   collect's subresources of such a page are fetched through the original
   origin (and its redirect). The fix belongs in `content`.
2. **Fixed in `content` (TestRegress_HopHeaderDocIDs):** hop header
   blocks are now `<url>#hop/<n>/headers`. Original finding:
   **A redirected capture's final headers and its first hop share a doc
   id.** `content.Process` names the final response's header block
   `Capture.URL + "#headers"` and each hop's `Hop.URL + "#headers"`; the
   first hop's URL is the requested URL, so both artifacts are
   `<url>#headers`, and the next version's diff pairs the final block with
   the hop's (a `changed` artifact inside a `structural` change set, with
   no level-3 change). Every new version of a redirected URL whose final
   response changes (any volatile value) is then text-only noise. The
   shop's redirects therefore land on a static page (`/legacy` →
   `/catalog`), and the acceptance test fails on any `structural` version.
   The fix belongs in `content` (a distinct suffix for hop blocks).
3. **Catch-all 404s are degraded and stored every time.** Three or more
   URLs of a host answering the same not-found body make each such capture
   degraded (V2 §12.5), and degraded captures are stored closed on every
   fetch. wellknown's probes therefore store only 2xx answers (a missing
   resource is an answer, not content); robots.txt is stored whatever it
   answers. Without that the shop's six deploys stored 60 degraded 404
   versions.
4. **Storage depends on realistic sizes.** With the first, tiny site (a
   1.7 KB page, pages under 500 bytes) the raw snapshots took 37 % of the
   raw bytes and all records 110 %: a version costs a capture record and
   two manifests (about 300 bytes) whatever its size. The acceptance site
   has a 39 KB product page and 5-48 KB secondary pages (generated, no
   links), which is still small for a real shop; §10 item 7 holds with
   room (6.8 %, all records 18.4 %). contentstore OPEN_QUESTIONS 1
   discusses the same effect on the SPA fixture.
5. **Scope checks resolve names before matching allow rules.** Spec 4.3
   step 7 resolves an active name target before step 8 matches the allow
   rules, so an active check of an out-of-scope link would ask the lab DNS
   (in the real world: the link owner's DNS) about it. The content tools
   check every discovered URL passively first and only then actively
   (services/toolrunner README, "Scope"); the acceptance test asserts that
   no out-of-scope name reaches the DNS server.
6. **Lineage after several renames** used to pick the oldest generation of
   a bundle (contentstore OPEN_QUESTIONS 7, fixed there): the lab's d4
   bundle was diffed against d1's and reported d3's endpoint again.
