// Package storetest is the conformance suite for eventstore.Store
// implementations (spec 23.4 properties (1)-(4), 23.8, test ids es_001 to
// es_006, the random-interleavings property of 25.4, and the scripted
// scenarios in conformance/eventstore/*.json).
//
// An implementation's test calls Run with a factory that returns a fresh
// Harness; es_007 is that call succeeding for every implementation with no
// per-implementation special cases. The suite never sleeps: lease expiry
// and wake times are driven through Harness.Elapse, which a MemStore
// implements by advancing its FakeClock and Postgres by shifting the
// stored lease and wake timestamps back (equivalent for every comparison
// against now()).
//
// Like testkit, which it uses, storetest is test support code: import it
// only from _test.go files.
package storetest
