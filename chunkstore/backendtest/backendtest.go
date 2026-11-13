// Package backendtest is the contract suite of the chunkstore Backend
// port (chunkstore/backend.go): every adapter (MemBackend, fsstore.Dir,
// gcsstore.Backend) runs Contract and Store, so they agree on atomic
// replace, not-found codes, exact ranges, sorted listing, idempotent
// deletes, name validation, and on a Store's whole life (reopen, GC,
// orphan sweep, fsck) on top. Test support only: imported from _test.go
// files.
package backendtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/genuinebnt/flowforge/chunkstore"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// Contract checks the Backend contract on b, which must be empty.
func Contract(t *testing.T, b chunkstore.Backend) {
	t.Helper()
	ctx := context.Background()
	if _, err := b.Get(ctx, "t/a/ref"); !errors.Is(err, chunkstore.ErrObjectNotFound) {
		t.Fatalf("missing Get: %v", err)
	}
	if _, err := b.GetRange(ctx, "t/a/ref", 0, 1); !errors.Is(err, chunkstore.ErrObjectNotFound) {
		t.Fatalf("missing GetRange: %v", err)
	}
	if err := b.Delete(ctx, "t/a/ref"); err != nil {
		t.Fatalf("delete of a missing object: %v", err)
	}
	for _, n := range []string{"t/a/ref", "t/a/packs/x.pack", "t/b/ref", "z"} {
		if err := b.Put(ctx, n, []byte("v1:"+n)); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Put(ctx, "t/a/ref", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if got, _ := b.Get(ctx, "t/a/ref"); string(got) != "v2" {
		t.Fatalf("replace: %q", got)
	}
	if got, err := b.GetRange(ctx, "t/a/packs/x.pack", 3, 4); err != nil || string(got) != "t/a/" {
		t.Fatalf("GetRange: %q %v", got, err)
	}
	if got, err := b.GetRange(ctx, "t/a/ref", 2, 0); err != nil || len(got) != 0 {
		t.Fatalf("empty range at the end: %q %v", got, err)
	}
	for _, r := range [][2]int{{1, 5}, {2, 1}, {-1, 1}} {
		if _, err := b.GetRange(ctx, "t/a/ref", int64(r[0]), r[1]); err == nil || errors.Is(err, chunkstore.ErrObjectNotFound) {
			t.Fatalf("GetRange(%d, %d) of a 2-byte object: %v", r[0], r[1], err)
		}
	}
	got, _ := b.List(ctx, "t/a/")
	if !slices.Equal(got, []string{"t/a/packs/x.pack", "t/a/ref"}) {
		t.Fatalf("List: %v", got)
	}
	all, _ := b.List(ctx, "")
	if !slices.Equal(all, []string{"t/a/packs/x.pack", "t/a/ref", "t/b/ref", "z"}) {
		t.Fatalf("List all: %v", all)
	}
	if err := b.Delete(ctx, "t/a/ref"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Get(ctx, "t/a/ref"); !errors.Is(err, chunkstore.ErrObjectNotFound) {
		t.Fatal("deleted object still readable")
	}
	if err := b.Delete(ctx, "t/a/ref"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	// Empty objects exist and are empty.
	if err := b.Put(ctx, "t/c/ref", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := b.Get(ctx, "t/c/ref"); err != nil || len(got) != 0 {
		t.Fatalf("empty object: %q %v", got, err)
	}
	for _, bad := range []string{"", "/abs", "a//b", "../x", "a/../b", "A", "a b", "a\\b", "x/", strings.Repeat("a", 256)} {
		if err := b.Put(ctx, bad, nil); chunkstore.CodeOf(err) != chunkstore.CodeInvalidName {
			t.Errorf("Put(%q): %v", bad, err)
		}
		if _, err := b.Get(ctx, bad); chunkstore.CodeOf(err) != chunkstore.CodeInvalidName {
			t.Errorf("Get(%q): %v", bad, err)
		}
	}
	// A large object round-trips and ranges inside it are exact.
	r := testkit.Rng(t)
	big := make([]byte, 3<<20+17)
	for i := range big {
		big[i] = byte(r.Uint64())
	}
	if err := b.Put(ctx, "t/d/packs/big.pack", big); err != nil {
		t.Fatal(err)
	}
	if got, err := b.Get(ctx, "t/d/packs/big.pack"); err != nil || !bytes.Equal(got, big) {
		t.Fatalf("large Get: %d bytes, %v", len(got), err)
	}
	off := int64(core.IntN(r, len(big)-4096))
	if got, err := b.GetRange(ctx, "t/d/packs/big.pack", off, 4096); err != nil || !bytes.Equal(got, big[off:off+4096]) {
		t.Fatalf("large GetRange at %d: %v", off, err)
	}
	// Concurrent Puts of distinct objects all land.
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := range 16 {
		wg.Go(func() {
			errs <- b.Put(ctx, fmt.Sprintf("t/e/packs/%02d.pack", i), []byte{byte(i)})
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if names, err := b.List(ctx, "t/e/"); err != nil || len(names) != 16 || !slices.IsSorted(names) {
		t.Fatalf("after concurrent Puts: %v %v", names, err)
	}
}

// Store runs a chunkstore.Store over the backend through its whole life.
// open returns a new Backend instance over the same (initially empty)
// storage each time it is called, as a restarted process would.
func Store(t *testing.T, open func(t *testing.T) chunkstore.Backend) {
	t.Helper()
	r := testkit.Rng(t)
	ctx := context.Background()
	cfg := chunkstore.Config{Chunking: chunkstore.ChunkerConfig{MinSize: 256, AvgSize: 1024, MaxSize: 8192}, TargetPackSize: 32 << 10}
	b := open(t)
	s, err := chunkstore.Open(ctx, b, "acme", cfg)
	if err != nil {
		t.Fatal(err)
	}
	blobs := map[chunkstore.ManifestID][]byte{}
	var ids []chunkstore.ManifestID
	for range 8 {
		x := make([]byte, core.IntN(r, 60_000))
		for i := range x {
			x[i] = "abcdefgh {}();\n"[core.IntN(r, 15)]
		}
		id, _, err := s.Put(ctx, bytes.NewReader(x))
		if err != nil {
			t.Fatal(err)
		}
		blobs[id] = x
		ids = append(ids, id)
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}

	b2 := open(t)
	s2, err := chunkstore.Open(ctx, b2, "acme", cfg)
	if err != nil {
		t.Fatal(err)
	}
	for id, x := range blobs {
		rc, err := s2.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(rc)
		if err != nil || !bytes.Equal(got, x) {
			t.Fatal("reopened store differs")
		}
	}
	// An orphan left by a crash between pack and ref is swept by GC.
	orphan := "t/acme/packs/" + strings.Repeat("0", 64) + ".pack"
	if err := b2.Put(ctx, orphan, []byte("orphan")); err != nil {
		t.Fatal(err)
	}
	live := ids[:4]
	var orphans int
	for range 2 {
		rep, err := s2.GC(ctx, live, nil, chunkstore.GCOptions{LiveThresholdPercent: 100})
		if err != nil {
			t.Fatal(err)
		}
		orphans += rep.OrphansDeleted
	}
	if orphans != 1 {
		t.Fatalf("GC deleted %d orphans, want 1", orphans)
	}
	rep, err := chunkstore.Fsck(ctx, b2, "acme", live)
	if err != nil || len(rep.Problems) != 0 {
		t.Fatalf("fsck: %v %+v", err, rep.Problems)
	}
	if err := s2.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// After GC every object under packs/ is named by the ref, and the
	// dropped blobs are gone from a third instance.
	s3, err := chunkstore.Open(ctx, open(t), "acme", cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s3.Close(ctx)
	for _, id := range live {
		if _, err := s3.Stat(ctx, id); err != nil {
			t.Fatalf("live blob after GC: %v", err)
		}
	}
	for _, id := range ids[4:] {
		if _, err := s3.Stat(ctx, id); !errors.Is(err, chunkstore.ErrManifestNotFound) {
			t.Fatalf("dropped blob after two GCs: %v", err)
		}
	}
	names, err := b2.List(ctx, "t/acme/packs/")
	if err != nil {
		t.Fatal(err)
	}
	if fr, _ := chunkstore.Fsck(ctx, b2, "acme", live); fr.Has(chunkstore.CodeOrphanObject) {
		t.Fatalf("orphans after GC: %v", names)
	}
}
