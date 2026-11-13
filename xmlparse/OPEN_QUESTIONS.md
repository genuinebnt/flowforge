# xmlparse open questions

1. **Offset definition.** "First offending byte" is defined at character
   granularity (see `SyntaxError`): a multi-byte character that can only
   be wrong once complete is reported at its first byte, while invalid
   UTF-8 is reported at the first byte that makes the sequence invalid.
   A strictly byte-granular definition would put some errors on a lead
   byte (e.g. `\xC3` can never start a prolog character) and others on
   the last byte, which is hard to keep consistent.
2. **DOCTYPE policy.** Only `<!DOCTYPE name S? >` is accepted. A document
   that is well-formed XML with an internal subset is graded
   `unsupported`, not `failed`. Because no DTD is read, every attribute
   is CDATA and no default attributes exist; undefined entity references
   are well-formedness errors (WFC: Entity Declared).
3. **Encodings.** Policy refusal of anything but UTF-8 / UTF-16 (BOM
   required). UTF-16 without a BOM is detected by a NUL at offset 0 or 1
   and reported as `unsupported_encoding` there. US-ASCII and
   ISO-8859-1 declarations are refused even when the bytes are ASCII.
   UTF-16 input that ends inside a character (an odd final byte or a high
   surrogate without its pair) is an EOF condition, as a UTF-8 sequence
   cut by the end is: `unexpected_eof` (or `no_root_element` in the
   prolog) at the end of the transcoded buffer, never a complete document
   (`TestRegress_XMLUTF16PartialCharAtEOF`).
4. **XML 1.1.** `version="1.x"` is accepted (XML 1.0 §2.8 lets 1.0
   processors do so) but parsed with 1.0 rules.
5. **Text events.** A text event is emitted at EOF before the EOF error
   when character data was read; the tree then holds it.
6. **PI targets** may not contain a colon (Namespaces §7); an element or
   attribute named with the `xmlns` prefix is `reserved_prefix`.
7. **Sitemaps.** A root without any namespace is accepted with
   `missing_namespace` unless `RequireNamespace`; entries with a missing
   or invalid `<loc>` are dropped with a diagnostic but do not make the
   read partial (no valid entry was lost). Default limits (20,000 / 8
   MiB) are below the protocol's 50,000 / 50 MiB by the V2 assurance
   bounds; options can raise them only up to the protocol limits.
