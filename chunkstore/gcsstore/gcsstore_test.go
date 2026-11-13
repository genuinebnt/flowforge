package gcsstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"slices"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/googleapis/gax-go/v2"

	"github.com/genuinebnt/flowforge/chunkstore"
	"github.com/genuinebnt/flowforge/chunkstore/backendtest"
	"github.com/genuinebnt/flowforge/chunkstore/gcsstore"
	"github.com/genuinebnt/flowforge/chunkstore/gcsstore/gcsfake"
	"github.com/genuinebnt/flowforge/textindex"
)

const bucket = "ff-content"

// client returns a storage client on the fake whose retries back off for
// a millisecond, so retry paths run without waiting.
func client(t *testing.T, f *gcsfake.Server) *storage.Client {
	t.Helper()
	c := f.Client(t)
	c.SetRetry(storage.WithBackoff(gax.Backoff{Initial: time.Millisecond, Max: 2 * time.Millisecond}))
	return c
}

func backend(t *testing.T, f *gcsfake.Server, prefix string) *gcsstore.Backend {
	t.Helper()
	b, err := gcsstore.New(client(t, f), bucket, gcsstore.Options{Prefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var cfg = chunkstore.Config{Chunking: chunkstore.ChunkerConfig{MinSize: 256, AvgSize: 1024, MaxSize: 8192}}

func blob(seed byte, n int) []byte {
	x := make([]byte, n)
	s := uint32(seed) + 1
	for i := range x {
		s = s*1664525 + 1013904223
		x[i] = "abcdefgh {}();\n"[s>>28%15]
	}
	return x
}

// The chunkstore Backend contract (chunkstore/backendtest) on GCS, with
// and without an object prefix and with list pages smaller than the
// listings, and the whole Store life over instances reopened on the same
// bucket (G-CORE-04, G-CST-02).
func TestGCSStoreBackendContract(t *testing.T) {
	for _, prefix := range []string{"", "chunks/"} {
		t.Run("prefix="+prefix, func(t *testing.T) {
			f := gcsfake.New(t)
			f.PageSize = 3
			f.Write(bucket, "other/NOT-A-CHUNK", []byte("x")) // foreign objects are not listed
			backendtest.Contract(t, backend(t, f, prefix))
			for _, n := range f.Names(bucket) {
				if n != "other/NOT-A-CHUNK" && !strings.HasPrefix(n, prefix) {
					t.Fatalf("object %q outside prefix %q", n, prefix)
				}
			}
		})
	}
	t.Run("store", func(t *testing.T) {
		f := gcsfake.New(t)
		backendtest.Store(t, func(t *testing.T) chunkstore.Backend { return backend(t, f, "chunks/") })
	})
	if _, err := gcsstore.New(nil, bucket, gcsstore.Options{}); err == nil {
		t.Fatal("New without a client succeeded")
	}
	f := gcsfake.New(t)
	if _, err := gcsstore.New(f.Client(t), bucket, gcsstore.Options{Prefix: "chunks"}); err == nil {
		t.Fatal("a prefix without a trailing / was accepted")
	}
}

// TestRegress_G_CST_02_SecondWriterFailsLoudly: two processes open the
// same tenant namespace (G-CST-03 allows one). The first to commit wins;
// the other's flush and GC fail with ErrConflict instead of replacing the
// ref, and the winner's data stays intact and fsck-clean.
func TestRegress_G_CST_02_SecondWriterFailsLoudly(t *testing.T) {
	ctx := context.Background()
	f := gcsfake.New(t)
	a, err := chunkstore.Open(ctx, backend(t, f, "chunks/"), "acme", cfg)
	if err != nil {
		t.Fatal(err)
	}
	b, err := chunkstore.Open(ctx, backend(t, f, "chunks/"), "acme", cfg)
	if err != nil {
		t.Fatal(err)
	}
	x := blob(1, 40_000)
	idA, _, err := a.Put(ctx, bytes.NewReader(x))
	if err != nil {
		t.Fatal(err)
	}
	refData, refGen, _ := f.Object(bucket, "chunks/t/acme/ref")
	_, _, err = b.Put(ctx, bytes.NewReader(blob(2, 40_000)))
	if !errors.Is(err, gcsstore.ErrConflict) || chunkstore.CodeOf(err) != chunkstore.CodeBackend {
		t.Fatalf("second writer's Put: %v, want BACKEND wrapping ErrConflict", err)
	}
	// It keeps failing (the stale generation is kept), and so does its GC.
	if _, _, err := b.Put(ctx, bytes.NewReader(blob(3, 1000))); !errors.Is(err, gcsstore.ErrConflict) {
		t.Fatalf("second writer's next Put: %v", err)
	}
	if _, err := b.GC(ctx, nil, nil, chunkstore.GCOptions{}); !errors.Is(err, gcsstore.ErrConflict) {
		t.Fatalf("second writer's GC: %v", err)
	}
	if got, gen, _ := f.Object(bucket, "chunks/t/acme/ref"); gen != refGen || !bytes.Equal(got, refData) {
		t.Fatal("the losing writer replaced the ref")
	}
	// The winner still reads its blob; a fresh process sees a consistent
	// store whose only finding is the loser's orphan pack, which the
	// winner's next GC sweeps.
	c, err := chunkstore.Open(ctx, backend(t, f, "chunks/"), "acme", cfg)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := c.Get(ctx, idA)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(rc); !bytes.Equal(got, x) {
		t.Fatal("winner's blob differs")
	}
	if rep, err := c.Fsck(ctx, []chunkstore.ManifestID{idA}); err != nil || !rep.OK() || !rep.Has(chunkstore.CodeOrphanObject) {
		t.Fatalf("fsck after the conflict: %v %+v", err, rep.Problems)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if rep, err := a.GC(ctx, []chunkstore.ManifestID{idA}, nil, chunkstore.GCOptions{}); err != nil || rep.OrphansDeleted == 0 {
		t.Fatalf("winner's GC: %+v %v", rep, err)
	}
}

// The GC sweep re-reads the ref before deleting under packs/: when another
// writer has moved the ref since this backend saw it, the delete is
// refused and the object stays.
func TestGCSStoreSweepChecksRef(t *testing.T) {
	ctx := context.Background()
	f := gcsfake.New(t)
	b := backend(t, f, "")
	pack := "t/acme/packs/" + strings.Repeat("a", 64) + ".pack"
	if err := b.Delete(ctx, pack); !errors.Is(err, gcsstore.ErrConflict) {
		t.Fatalf("sweep in a namespace whose ref was never observed: %v", err)
	}
	if _, err := b.Get(ctx, "t/acme/ref"); !errors.Is(err, chunkstore.ErrObjectNotFound) {
		t.Fatal(err)
	}
	if err := b.Put(ctx, pack, []byte("p")); err != nil {
		t.Fatal(err)
	}
	if err := b.Put(ctx, "t/acme/ref", []byte("r1")); err != nil {
		t.Fatal(err)
	}
	f.Write(bucket, "t/acme/ref", []byte("r2 by another writer"))
	if err := b.Delete(ctx, pack); !errors.Is(err, gcsstore.ErrConflict) {
		t.Fatalf("sweep after the ref moved: %v", err)
	}
	if _, _, ok := f.Object(bucket, pack); !ok {
		t.Fatal("pack deleted although the ref moved")
	}
	// A pack rewritten by someone else after this backend saw it is not
	// deleted either (generation precondition on the delete itself).
	b2 := backend(t, f, "")
	if _, err := b2.Get(ctx, "t/acme/ref"); err != nil {
		t.Fatal(err)
	}
	if _, err := b2.List(ctx, "t/acme/packs/"); err != nil {
		t.Fatal(err)
	}
	f.Write(bucket, pack, []byte("p2"))
	if err := b2.Delete(ctx, pack); !errors.Is(err, gcsstore.ErrConflict) {
		t.Fatalf("delete of a rewritten pack: %v", err)
	}
}

// A write whose acknowledgement is lost is retried by the client; the
// retry's precondition fails because the first attempt landed, and the
// adapter recognises its own bytes instead of reporting a conflict. Other
// failures are BACKEND errors without ErrConflict.
func TestGCSStoreLostAckAndFailures(t *testing.T) {
	ctx := context.Background()
	f := gcsfake.New(t)
	lost := 1
	f.Fault = func(op gcsfake.Op) gcsfake.Fault {
		if op.Method == "upload" && lost > 0 {
			lost--
			return gcsfake.Fault{Status: 503, AfterCommit: true}
		}
		return gcsfake.Fault{}
	}
	b := backend(t, f, "")
	if err := b.Put(ctx, "t/acme/ref", []byte("r1")); err != nil {
		t.Fatalf("Put with a lost acknowledgement: %v", err)
	}
	reqs := f.Requests()
	if !slices.Contains(reqs, "upload t/acme/ref -> 412") {
		t.Fatalf("the retry did not hit the precondition: %v", reqs)
	}
	_, gen, _ := f.Object(bucket, "t/acme/ref")
	if err := b.Put(ctx, "t/acme/ref", []byte("r2")); err != nil {
		t.Fatalf("next Put after the recovered one: %v", err)
	}
	if _, gen2, _ := f.Object(bucket, "t/acme/ref"); gen2 <= gen {
		t.Fatal("generation did not advance")
	}

	f.Fault = func(op gcsfake.Op) gcsfake.Fault {
		if op.Object == "t/acme/packs/x.pack" {
			return gcsfake.Fault{Status: 403}
		}
		return gcsfake.Fault{}
	}
	for name, err := range map[string]error{
		"put":   b.Put(ctx, "t/acme/packs/x.pack", []byte("x")),
		"get":   func() error { _, err := b.Get(ctx, "t/acme/packs/x.pack"); return err }(),
		"range": func() error { _, err := b.GetRange(ctx, "t/acme/packs/x.pack", 0, 1); return err }(),
	} {
		if chunkstore.CodeOf(err) != chunkstore.CodeBackend || errors.Is(err, gcsstore.ErrConflict) || errors.Is(err, chunkstore.ErrObjectNotFound) {
			t.Errorf("%s with 403: %v", name, err)
		}
	}
}

// Bytes changed in the bucket (a faulty writer uploading a corrupted pack)
// are caught by chunkstore's own checksums through this adapter: fsck
// reports PACK_CHECKSUM and Get never returns wrong bytes.
func TestGCSStoreCorruptionDetected(t *testing.T) {
	ctx := context.Background()
	f := gcsfake.New(t)
	s, err := chunkstore.Open(ctx, backend(t, f, ""), "acme", cfg)
	if err != nil {
		t.Fatal(err)
	}
	x := blob(7, 30_000)
	id, _, err := s.Put(ctx, bytes.NewReader(x))
	if err != nil {
		t.Fatal(err)
	}
	var pack string
	for _, n := range f.Names(bucket) {
		if strings.HasSuffix(n, ".pack") {
			pack = n
		}
	}
	data, _, _ := f.Object(bucket, pack)
	data[len(data)/2] ^= 0x40
	f.Write(bucket, pack, data)
	b2 := backend(t, f, "")
	rep, err := chunkstore.Fsck(ctx, b2, "acme", []chunkstore.ManifestID{id})
	if err != nil || rep.OK() || !rep.Has(chunkstore.CodePackChecksum) {
		t.Fatalf("corrupted pack not reported: %v %+v", err, rep.Problems)
	}
	s2, err := chunkstore.Open(ctx, b2, "acme", cfg)
	if err != nil {
		t.Fatal(err)
	}
	if rc, err := s2.Get(ctx, id); err == nil {
		if got, err := io.ReadAll(rc); err == nil && !bytes.Equal(got, x) {
			t.Fatal("Get returned wrong bytes")
		}
	}
}

// Directory satisfies textindex.Directory: missing files wrap
// fs.ErrNotExist, names with separators are refused, List shows only
// direct children, and a second writer's write of a file the first one
// replaced fails with ErrConflict.
func TestGCSStoreDirectory(t *testing.T) {
	f := gcsfake.New(t)
	c := client(t, f)
	d, err := gcsstore.NewDirectory(c, bucket, "index/t1/p1/")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadFile("commit"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	for _, bad := range []string{"", ".", "..", "a/b", `a\b`} {
		if err := d.WriteFile(bad, nil); !errors.Is(err, textindex.ErrInvalidConfig) {
			t.Errorf("WriteFile(%q): %v", bad, err)
		}
	}
	for _, n := range []string{"seg-2", "seg-1", "commit"} {
		if err := d.WriteFile(n, []byte(n)); err != nil {
			t.Fatal(err)
		}
	}
	f.Write(bucket, "index/t1/p1/nested/x", []byte("x"))
	f.Write(bucket, "index/t1/p10/seg-1", []byte("x"))
	if names, err := d.List(); err != nil || !slices.Equal(names, []string{"commit", "seg-1", "seg-2"}) {
		t.Fatalf("List: %v %v", names, err)
	}
	if err := d.WriteFile("commit", []byte("commit 2")); err != nil {
		t.Fatal(err)
	}
	if got, err := d.ReadFile("commit"); err != nil || string(got) != "commit 2" {
		t.Fatalf("ReadFile: %q %v", got, err)
	}
	if err := d.Remove("seg-1"); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove("seg-1"); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
	other, _ := gcsstore.NewDirectory(c, bucket, "index/t1/p1/")
	if _, err := other.ReadFile("commit"); err != nil {
		t.Fatal(err)
	}
	if err := d.WriteFile("commit", []byte("commit 3")); err != nil {
		t.Fatal(err)
	}
	if err := other.WriteFile("commit", []byte("stale commit")); !errors.Is(err, gcsstore.ErrConflict) {
		t.Fatalf("stale writer: %v", err)
	}
	if err := other.WriteFile("seg-2", []byte("clobber")); !errors.Is(err, gcsstore.ErrConflict) {
		t.Fatalf("unobserved overwrite: %v", err)
	}
	if got, _ := d.ReadFile("commit"); string(got) != "commit 3" {
		t.Fatalf("commit after the stale write: %q", got)
	}
	if _, err := gcsstore.NewDirectory(c, bucket, "index"); err == nil {
		t.Fatal("prefix without a trailing / accepted")
	}
}
