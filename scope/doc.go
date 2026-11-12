// Package scope implements the Flowforge scope model (spec section 4): the
// program scope document (4.1), target normalization (4.2, delegated to
// core/netnorm), the decision algorithm (4.3), the passive scope status
// used by graph ingest (4.4, 5.7) and the DSL's scope.allows pre-filter
// (7.6), and re-checking of redirect and CNAME chains (4.5). It upholds
// the scope and "no silent success" invariants of section 2.4: nothing is
// allowed unless an allow rule matches the exact normalized target, and
// every failure, including a failed name resolution, is a deny.
//
// A Document is validated and compiled once into an immutable Policy:
//
//	p, err := scope.Compile(doc, scope.WithResolver(dns))
//	d := p.Check(ctx, scope.Request{Target: "https://api.acme.com/v2/x",
//		Kind: scope.TargetURL, Class: core.ClassActive, Now: now})
//
// Matching is indexed so that decision cost grows with the depth of the
// target (labels, prefix bits, path segments), not with the number of
// rules: a reversed-label trie for domain and wildcard_domain rules, a
// binary prefix trie per address family for cidr rules, and a per-origin
// path map for url_prefix rules. A linear reference matcher, a direct
// transcription of the spec, is kept for differential tests and as the
// benchmark baseline (spec 25.8, b_scope_01).
//
// The package reads no clock: the decision instant arrives in
// Request.Now. Name resolution goes through the Resolver port; the
// StaticResolver fake serves tests.
package scope
