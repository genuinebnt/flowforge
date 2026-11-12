// Package enginetest provides in-memory fakes of every engine port
// (MemRepo, MemScope, MemTools, FakeToolExec, MemIngester, MemPublisher,
// FakeCompiler), a CrashStore that simulates a worker dying after N
// appends, hand-assembled test workflows (Recon, Chain, Multi, Saga,
// Flaky, ParMapEcho, Agent) and a Harness that wires an engine with a fake
// clock and seeded randomness. It is imported only from tests.
package enginetest
