# htmlparse — open questions, known failures, deviations

Spec baseline: whatwg/html `source` at commit `c18a7c5` (2026-11-12),
§13.2–§13.5. Test data: html5lib-tests `c5495c5` (tokenizer) and WPT
`html/syntax/parsing` `6d0a291` (tree construction).

## Known conformance failures (asserted exactly by the tests)

### Q-TOK-1 — 16 tokenizer error positions (test3.test#1167–#1182)

Inputs `<?a a`, `<?A A`, `<?Z Z?`, … (and the `…?` variants). Tokens and
error codes match. The suite expects `eof-in-processing-instruction` at
column 4; the state machine reaches EOF in the processing instruction data
(or questionable) state, i.e. at the end of the input (column 6 or 7), as
every other EOF error in the suite is located. The upstream positions look
like a generator artifact. We keep the standard's behaviour; the list is
`knownTokenizerErrorPositionMismatches`.

### Q-TREE-1 — 6 scripted tree-construction cases

`scripted_adoption01.dat#0`, `scripted_ark.dat#0`,
`scripted_foster01.dat#0/#1`, `scripted_webkit01.dat#0/#1` expect the
effects of executing `<script>` (DOM mutation, `document.write`). This
parser never executes script (V2_DESIGN §4.1). List: `knownTreeFailures`.

## Decisions where the brief and the current standard differ

* **No "in select" / "in select in table" modes.** The brief lists them,
  but the standard removed them with the customizable `<select>` parser
  changes; select/option/optgroup/hr/input are handled in "in body", and
  `select` is a default-scope boundary (there is no select scope any more).
  The `.dat` files test the new behaviour (they are the authority).
* **Processing instructions.** `<?target data>` is now a token and a DOM
  node (`ProcessingInstructionNode`, dumped as `<?target data?>`,
  serialized as `<?target data?>`); `<?xml…` and `<?xml-stylesheet…` and
  invalid targets still become bogus comments. Added `NodeKind`
  beyond the brief's list.
* **selectedcontent.** The expected trees include the DOM side effect
  that copies the selected option's children into `<selectedcontent>`.
  That is element behaviour (option/selectedcontent insertion and
  "children parsed" steps), not §13.2. We reproduce its end state once
  parsing finishes (`selectedcontent.go`) when a selectedcontent element
  was seen; intermediate states (which only scripts could observe) are not
  modelled.
* **Declarative shadow roots, sanitizer, streaming.** The current
  standard's `shadowrootmode` templates, sanitizer actions and insertion
  target redirection only apply with options this parser does not offer
  (`allow declarative shadow roots` is false, as for `DOMParser`); templates
  are always ordinary templates.
* **Tree-construction error codes.** The standard names only tokenizer
  errors. Tree errors use the Flowforge codes in `errors.go`
  (`unexpected-start-tag`, `eof-in-element`, …); `.dat` `#errors` sections
  are not compared (their formats vary by file).
* **Error columns** are UTF-16 code units (what the html5lib positions
  use); offsets are bytes.
* **Lone surrogates** in the input (as WTF-8/CESU-8 bytes) report
  `surrogate-in-input-stream` and become U+FFFD, so every string the
  package produces is valid UTF-8 (portable to Rust `String`). The
  html5lib expectations that contain lone surrogates are compared after the
  same mapping.
* **Default encoding.** After BOM, transport, prescan and XML declaration,
  input that is entirely valid UTF-8 is taken as UTF-8 (the standard's
  optional autodetection step), otherwise windows-1252.
* **Depth limit.** An element that would exceed `MaxDepth` is inserted but
  not pushed; its would-be content goes to the deepest open element.
  Formatting elements that cannot be opened are not added to the active
  formatting list (so reconstruction cannot loop on them), and
  reconstruction stops at the limit.
* **Ignore-LF after `<pre>`/`<listing>`/`<textarea>`** applies only if the
  very next token is a character token starting with LF (also when that LF
  comes from `&#10;`), as specified.

## Serialize → reparse (HTML-INV-03) scope

The standard documents that serialization does not always round-trip.
Excluded from the invariant (`roundTripSafe`): raw text and RCDATA
elements, plaintext, tables (foster parenting), forms, select, template,
foreign content, elements with implied end tags or formatting scope
effects (p, li, dd, dt, headings, a, nobr, button…), processing
instructions, comments containing `--` or starting with `>`/`->`, DOCTYPEs
with public/system ids (the serializer writes only the name), text or
attribute values containing CR (only `&#13;` can produce one; it
serializes raw and reparses as LF), and documents that hit a limit. Every
well-formed generated document is checked.

## Differential oracle (golang.org/x/net/html v0.62) deviation classes

The `.dat` expectation is the authority; in every class below our output
matches the standard (verified by hand on the triggering inputs, and by the
`.dat` cases where they exist). `TestHTMLDiffOracle` fails on any
difference that does not match a class trigger; the triggers are
deliberately narrow input features, not a catch-all.

| Class | x/net behaviour | Evidence |
|---|---|---|
| D1 | `<?…>` is a bogus comment (no processing instructions) | processing-instructions.dat (84), tests1.dat #39/#43/#46, html5test-com.dat #11 |
| D2 | `<search>` is not a special/block element | search-element.dat #3–#7 |
| D3 | form element pointer used while parsing template contents (template context, form in a template table) | template.dat #116/#119/#123 |
| D4 | NUL in CDATA / at integration points kept as U+FFFD and clears frameset-ok | plain-text-unsafe.dat #21/#22/#25 |
| D5 | no selectedcontent mirroring | webkit02.dat #44–#47 |
| D6 | character references decoded inside comments, bogus comments and CDATA sections | soup |
| D7 | the foreign `xmlns` attribute stays in no namespace (we put it in the XMLNS namespace, dumped `xmlns xmlns`); normalized before comparing | soup |
| D8 | the tokenizer enters RAWTEXT/RCDATA/script data by tag name even when the tree builder ignores the start tag (frameset modes) | soup |
| D9 | `ParseFragment` panics (recovered, returned as an error) on some inputs with a MathML `mi` context | fragment soup |
| D10 | a character token starting with whitespace is handled as a unit in "in head noscript" (the whitespace leaves the noscript) | soup |
| D11 | the LF after `<pre>`/`<listing>`/`<textarea>` is dropped even when another token comes first | soup |
| D12 | foster-parented / template-contents text is not merged into an adjacent Text node | soup |
| D13 | a foreign fragment context (svg, mi) is not used as the adjusted current node (HTML rules at top level, CDATA as bogus comment) | fragment soup |
| D14 | `<template>` start tags at SVG/MathML integration points are lost | soup |
| D15 | in "in column group" the whole character token is dropped when its first character is not whitespace | soup |
| D16 | implied end tags / scope checks ignore the namespace (a foreign `<option>`/`<p>`… is closed like an HTML one) | soup |
| D17 | with a table-section fragment context, a cell start tag closing a caption is mishandled | fragment soup |

## Open questions

* Q-OPEN-1: should `Node` shrink (int32 spans, doctype ids out of line)
  for memory? Today bytes/op are ~1.7× x/net/html's (README "Performance
  comparison"); the Rust port can choose `u32` spans independently.
* Q-OPEN-2: `ParseRaw` re-parses from scratch when a `<meta>` changes a
  tentative encoding; a streaming parser could switch decoders when the
  bytes so far decode identically (the standard allows both).
