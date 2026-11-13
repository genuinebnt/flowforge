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
	"github.com/genuinebnt/flowforge/chunkstore/backendtest"
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

// The Backend contract of chunkstore.Backend (chunkstore/backendtest),
// checked against both the directory backend and the in-memory reference,
// plus the directory backend's own rule: names containing its temporary
// marker are refused.
func TestFSStoreBackendContract(t *testing.T) {
	for name, mk := range map[string]func(t *testing.T) chunkstore.Backend{
		"fs":       func(t *testing.T) chunkstore.Backend { return newDir(t, Options{}) },
		"fsnosync": func(t *testing.T) chunkstore.Backend { return newDir(t, Options{NoSync: true}) },
		"mem":      func(t *testing.T) chunkstore.Backend { return chunkstore.NewMemBackend() },
	} {
		t.Run(name, func(t *testing.T) { backendtest.Contract(t, mk(t)) })
	}
	if err := newDir(t, Options{}).Put(context.Background(), "x.tmp-1", nil); chunkstore.CodeOf(err) != chunkstore.CodeInvalidName {
		t.Errorf("Put of a temporary-file name: %v", err)
	}
}

// The Store lifecycle suite over directories reopened from the same root.
func TestFSStoreBackendStoreSuite(t *testing.T) {
	root := t.TempDir()
	backendtest.Store(t, func(t *testing.T) chunkstore.Backend {
		d, err := New(root, Options{NoSync: true})
		if err != nil {
			t.Fatal(err)
		}
		return d
	})
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
