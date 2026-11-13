// Package xmlparse is a non-validating XML 1.0 (Fifth Edition) parser
// with Namespaces in XML 1.0 (Third Edition): a pull Decoder (explicit
// state machine named after the XML productions) and an arena Document
// built from its events, plus a canonical writer modelled on Canonical
// XML 1.0.
//
// It reads elements, attributes (normalized per §3.3.3 as CDATA), text,
// CDATA sections, comments, processing instructions, the XML declaration,
// the five predefined entities and character references, with §2.11
// line-end normalization. Only UTF-8 (with or without BOM) and UTF-16
// with a BOM are accepted. A DOCTYPE is accepted only in the bare form
// <!DOCTYPE name>: an internal subset or an external identifier is
// refused at its first byte (ErrDTDInternalSubset, ErrDTDExternalID), so
// no entity is ever declared, expanded or fetched (XXE and billion-laughs
// safe by construction).
//
// Well-formedness errors are fatal and reported once, as a *SyntaxError
// with a stable code at the first offending byte. Limits (Options.MaxDepth
// 64, MaxInputBytes 8 MiB) stop the read with GradePartial; refused
// features give GradeUnsupported; other errors GradeFailed.
//
// The contract is spec 29.5 (codes 40.1). Parse and the Decoder are pure;
// a Decoder is used by one goroutine, a finished Document may be shared.
//
// Subpackage sitemap reads sitemaps.org documents on top of the Decoder.
package xmlparse
