// Package testkit provides deterministic test fixtures shared by every
// Flowforge package: a fake clock, seeded randomness that can be replayed
// through FF_SEED, and a loader for the language-neutral conformance data
// under conformance/ (spec 24.2, 25.2, 25.9).
//
// testkit is imported only from _test.go files and from the test-support
// packages built on it (eventstore/storetest, engine/enginetest), which in
// turn are imported only from tests.
package testkit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// FakeClock is a manually driven core.Clock. The zero value starts at the
// Unix epoch with a monotonic reading of zero. It is safe for concurrent use.
type FakeClock struct {
	mu   sync.Mutex
	wall time.Time
	mono time.Duration
}

// NewFakeClock returns a clock whose wall time starts at start.
func NewFakeClock(start time.Time) *FakeClock { return &FakeClock{wall: start} }

// Now implements core.Clock.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wall.IsZero() {
		return time.Unix(0, 0).UTC().Add(c.mono)
	}
	return c.wall
}

// Monotonic implements core.Clock.
func (c *FakeClock) Monotonic() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mono
}

// Advance moves both readings forward by d (d may be zero, never negative).
func (c *FakeClock) Advance(d time.Duration) {
	if d < 0 {
		panic("testkit: FakeClock.Advance with negative duration")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wall.IsZero() {
		c.wall = time.Unix(0, 0).UTC()
	}
	c.wall = c.wall.Add(d)
	c.mono += d
}

// SetMonotonic sets the monotonic reading. Tests use it to feed specific
// timelines, including (deliberately) backwards ones.
func (c *FakeClock) SetMonotonic(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.mono = d
}

// SetWall sets the wall time.
func (c *FakeClock) SetWall(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.wall = t
}

// Seed returns the seed for a randomized test: FF_SEED (decimal uint64)
// when set, otherwise the wall clock in nanoseconds. It logs the seed when
// the test fails so the run can be reproduced (spec 25.2). A malformed
// FF_SEED fails the test.
func Seed(t testing.TB) uint64 {
	t.Helper()
	seed := uint64(time.Now().UnixNano())
	if s := os.Getenv("FF_SEED"); s != "" {
		v, err := strconv.ParseUint(s, 10, 64)
		if err != nil {
			t.Fatalf("testkit: bad FF_SEED %q: %v", s, err)
		}
		seed = v
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("reproduce with FF_SEED=%d", seed)
		}
	})
	return seed
}

// Rng returns a seeded core.Rng for t (see Seed).
func Rng(t testing.TB) *core.SplitMix64 { return core.NewSplitMix64(Seed(t)) }

// Cases returns the number of property-test iterations: 1,000 by default,
// 100 under go test -short, or FF_PROPTEST_CASES when it is a positive
// integer (the nightly job uses 100,000); FF_PROPTEST_CASES wins over
// -short.
func Cases() int {
	if s := os.Getenv("FF_PROPTEST_CASES"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	if testing.Short() {
		return 100
	}
	return 1000
}

// RepoRoot walks up from the working directory to the directory holding
// go.mod.
func RepoRoot(t testing.TB) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("testkit: go.mod not found above working directory")
		}
		dir = parent
	}
}

// ConformanceDir returns the absolute path of conformance/<suite>.
func ConformanceDir(t testing.TB, suite string) string {
	t.Helper()
	return filepath.Join(RepoRoot(t), "conformance", filepath.FromSlash(suite))
}

// CaseFile is one decoded conformance file.
type CaseFile[T any] struct {
	Name string // file name relative to the suite directory
	Data T
}

// LoadCases decodes every *.json file in conformance/<suite> into T, in
// file-name order (byte order of the paths). It fails the test if the
// suite is missing or empty, so a deleted data directory cannot make a
// suite pass vacuously. Decoding is plain json.Unmarshal: unknown keys in
// the data are ignored.
func LoadCases[T any](t testing.TB, suite string) []CaseFile[T] {
	t.Helper()
	dir := ConformanceDir(t, suite)
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		t.Fatalf("testkit: conformance suite %q has no *.json files in %s", suite, dir)
	}
	sort.Strings(names)
	out := make([]CaseFile[T], 0, len(names))
	for _, n := range names {
		b, err := os.ReadFile(n)
		if err != nil {
			t.Fatal(err)
		}
		var v T
		if err := json.Unmarshal(b, &v); err != nil {
			t.Fatalf("testkit: decode %s: %v", n, err)
		}
		out = append(out, CaseFile[T]{Name: filepath.Base(n), Data: v})
	}
	return out
}

// ReadFile reads conformance/<suite>/<name>.
func ReadFile(t testing.TB, suite, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(ConformanceDir(t, suite), name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}
