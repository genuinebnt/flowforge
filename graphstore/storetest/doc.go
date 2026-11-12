// Package storetest is the shared behaviour suite of graphstore.Store
// implementations (spec 5.7, 5.9, 6.1, 10.5, 11.6, 21.4, 23.5). Every
// implementation (graphstore.Mem, graphstore/pg) runs the same functions
// from its own tests, so the two cannot drift apart; where an oracle
// factory is given, results are also compared byte for byte with it.
//
// The package does not import testkit (it is not a test-only package for
// the architecture rules): callers pass the conformance cases and the
// seeded random source in.
package storetest
