# Third-party conformance suites

Vendored, unmodified, so the Go and Rust parsers are checked against the
same upstream test data. Each directory keeps the upstream licence.

| Directory | Upstream | Commit | Licence | Used by |
|---|---|---|---|---|
| `html5lib-tests/tokenizer` | github.com/html5lib/html5lib-tests | c5495c539ca8a79c3c5107f5f14010bcff9a35d5 | MIT (`html5lib-tests/LICENSE`) | `htmlparse` tokenizer |
| `wpt-html-parsing/*.dat` | github.com/web-platform-tests/wpt, `html/syntax/parsing` (the html5lib tree-construction tests now live there) | 6d0a291020e5ac3cc55ba53b28a6e556822ace39 | BSD-3-Clause (`wpt-html-parsing/LICENSE.md`) | `htmlparse` tree construction |
| `test262-parser-tests/{pass,pass-explicit,fail,early}` | github.com/tc39/test262-parser-tests | 0e808c74fbec780646434cad17bb22dc52461003 | see `test262-parser-tests/licenses.md` | `jsparse` |

To update: replace the files from the new upstream commit, update this
table, and record the expected-failure list changes in the parser's
OPEN_QUESTIONS.md.
