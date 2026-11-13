package fsstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/chunkstore"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

func newDir(t *testing.T, opts Options) *Dir {
	t.Helper()
	d, err := New(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// The Backend contract of chunkstore.Backend, checked against both the
// directory backend and the in-memory reference (differential).
func TestFSStoreBackendContract(t *testing.T) {
	ctx := context.Background()
	for name, b := range map[string]chunkstore.Backend{
		"fs":       newDir(t, Options{}),
		"fsnosync": newDir(t, Options{NoSync: true}),
		"mem":      chunkstore.NewMemBackend(),
	} {
		t.Run(name, func(t *testing.T) {
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
			if _, err := b.GetRange(ctx, "t/a/ref", 1, 5); err == nil {
				t.Fatal("GetRange past the end succeeded")
			}
			got, _ := b.List(ctx, "t/a/")
			if !slices.Equal(got, []string{"t/a/packs/x.pack", "t/a/ref"}) {
				t.Fatalf("List: %v", got)
			}
			all, _ := b.List(ctx, "")
			if !slices.IsSorted(all) || len(all) != 4 {
				t.Fatalf("List all: %v", all)
			}
			if err := b.Delete(ctx, "t/a/ref"); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Get(ctx, "t/a/ref"); !errors.Is(err, chunkstore.ErrObjectNotFound) {
				t.Fatal("deleted object still readable")
			}
			for _, bad := range []string{"", "/abs", "a//b", "../x", "a/../b", "A", "a b", "a\\b", "x.tmp-1"} {
				if err := b.Put(ctx, bad, nil); chunkstore.CodeOf(err) != chunkstore.CodeInvalidName && !(name == "mem" && bad == "x.tmp-1") {
					t.Errorf("Put(%q): %v", bad, err)
				}
			}
		})
	}
}

// Temporary files from interrupted Puts are invisible and Clean removes
// them.
func TestFSStoreTempFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	d, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Put(ctx, "t/a/ref", []byte("x")); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, "t", "a", "ref"+tmpMarker+"99")
	if err := os.WriteFile(stale, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if names, _ := d.List(ctx, ""); !slices.Equal(names, []string{"t/a/ref"}) {
		t.Fatalf("List shows temp files: %v", names)
	}
	if n, err := d.Clean(ctx); err != nil || n != 1 {
		t.Fatalf("Clean: %d %v", n, err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("temp file survived Clean")
	}
}

// A Store over a directory round-trips, survives reopening, collects
// garbage, and detects on-disk corruption.
func TestFSStoreStoreLifecycle(t *testing.T) {
	r := testkit.Rng(t)
	ctx := context.Background()
	root := t.TempDir()
	d, err := New(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := chunkstore.Config{Chunking: chunkstore.ChunkerConfig{MinSize: 256, AvgSize: 1024, MaxSize: 8192}, TargetPackSize: 32 << 10}
	s, err := chunkstore.Open(ctx, d, "acme", cfg)
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
	d2, _ := New(root, Options{})
	s2, err := chunkstore.Open(ctx, d2, "acme", cfg)
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
			t.Fatal("reopened directory store differs")
		}
	}
	live := ids[:4]
	for range 2 {
		if _, err := s2.GC(ctx, live, nil, chunkstore.GCOptions{LiveThresholdPercent: 100}); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := chunkstore.Fsck(ctx, d2, "acme", live)
	if err != nil || len(rep.Problems) != 0 {
		t.Fatalf("fsck: %v %+v", err, rep.Problems)
	}
	// Flip one byte of a pack on disk: fsck reports it.
	packs, _ := d2.List(ctx, "t/acme/packs/")
	var pack string
	for _, p := range packs {
		if filepath.Ext(p) == ".pack" {
			pack = p
		}
	}
	path := filepath.Join(root, filepath.FromSlash(pack))
	raw, _ := os.ReadFile(path)
	raw[len(raw)/2] ^= 0x40
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	rep, _ = chunkstore.Fsck(ctx, d2, "acme", live)
	if rep.OK() || !rep.Has(chunkstore.CodePackChecksum) {
		t.Fatalf("on-disk corruption not reported: %+v", rep.Problems)
	}
}
