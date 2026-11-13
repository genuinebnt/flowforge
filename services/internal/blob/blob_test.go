package blob_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/chunkstore/gcsstore/gcsfake"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/services/internal/blob"
)

// newGCS opens the GCS adapter on the in-process fake of the JSON API
// (chunkstore/gcsstore/gcsfake), shared with the chunk store's adapter.
func newGCS(t *testing.T) (*blob.GCS, *gcsfake.Server) {
	t.Helper()
	f := gcsfake.New(t)
	st, closeFn, err := blob.Open(context.Background(), blob.Config{Bucket: "ff-artifacts", ClientOptions: f.ClientOptions()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeFn)
	return st.(*blob.GCS), f
}

// TestRegress_GOPS14_BlobAdapters runs the same round trip against every
// adapter Open can select: put, get, a missing URI and a foreign URI.
func TestRegress_GOPS14_BlobAdapters(t *testing.T) {
	ctx := context.Background()
	gcs, fake := newGCS(t)
	dir := t.TempDir()
	dirStore, _, _ := blob.Open(ctx, blob.Config{Dir: dir})
	memStore, _, _ := blob.Open(ctx, blob.Config{})
	for name, st := range map[string]blob.Store{"gcs": gcs, "dir": dirStore, "mem": memStore} {
		t.Run(name, func(t *testing.T) {
			data := []byte(`{"raw":"output"}`)
			uri, err := st.Put(ctx, "raw/ten_1/run_1/obs_1.json", data)
			if err != nil {
				t.Fatal(err)
			}
			got, err := st.Get(ctx, uri)
			if err != nil || !bytes.Equal(got, data) {
				t.Fatalf("get %s = %q, %v", uri, got, err)
			}
			if _, err := st.Put(ctx, "../escape", data); err == nil {
				t.Fatal("put accepted a path escaping the root")
			}
			missing := strings.Replace(uri, "obs_1", "obs_2", 1)
			if _, err := st.Get(ctx, missing); !errors.Is(err, graphstore.ErrBlobNotFound) {
				t.Fatalf("missing blob: %v", err)
			}
		})
	}
	if blob.Kind(blob.Config{Bucket: "b", Dir: "/x"}) != "gcs" || blob.Kind(blob.Config{Dir: "/x"}) != "dir" {
		t.Fatal("selection order: bucket, then directory, then memory")
	}
	if !strings.HasPrefix(gcs.URI("a"), "gs://ff-artifacts/") {
		t.Fatal(gcs.URI("a"))
	}
	if _, err := gcs.Get(ctx, "gs://other-bucket/raw/x.json"); !errors.Is(err, graphstore.ErrBlobNotFound) {
		t.Fatalf("foreign bucket served: %v", err)
	}
	if err := gcs.Delete(ctx, gcs.URI("raw/ten_1/run_1/obs_1.json")); err != nil {
		t.Fatal(err)
	}
	if names := fake.Names("ff-artifacts"); len(names) != 0 {
		t.Fatalf("delete left %v", names)
	}
	if err := gcs.Delete(ctx, gcs.URI("raw/ten_1/run_1/obs_1.json")); err != nil {
		t.Fatalf("delete of a missing object: %v", err)
	}
	// Put replaces (the BlobStore contract: snapshot files are rewritten
	// under the same name), and a refused read is an error, not "not
	// found".
	uri, err := gcs.Put(ctx, "graph/snap.json", []byte("v1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gcs.Put(ctx, "graph/snap.json", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	if got, err := gcs.Get(ctx, uri); err != nil || string(got) != "v2" {
		t.Fatalf("replaced blob: %q %v", got, err)
	}
	fake.Fault = func(op gcsfake.Op) gcsfake.Fault {
		if op.Method == "get" {
			return gcsfake.Fault{Status: 403}
		}
		return gcsfake.Fault{}
	}
	if _, err := gcs.Get(ctx, uri); err == nil || errors.Is(err, graphstore.ErrBlobNotFound) {
		t.Fatalf("server error on read: %v", err)
	}
}

func TestZstdRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 4096, 128 << 10, 128<<10 + 7, 400 << 10} {
		in := bytes.Repeat([]byte(`{"seq":1,"type":"x"}`+"\n"), n/21+1)[:n]
		frame, err := blob.ZstdEncode(in)
		if err != nil {
			t.Fatal(err)
		}
		if binary.LittleEndian.Uint32(frame) != 0xFD2FB528 {
			t.Fatal("magic")
		}
		if n >= 4096 && len(frame) >= n/4 {
			t.Fatalf("n=%d: frame %d bytes, repetitive JSON Lines should compress at least 4x", n, len(frame))
		}
		again, _ := blob.ZstdEncode(in)
		if !bytes.Equal(again, frame) {
			t.Fatalf("n=%d: encoding is not deterministic", n)
		}
		out, err := blob.ZstdDecode(frame, int64(n))
		if err != nil || !bytes.Equal(out, in) {
			t.Fatalf("n=%d: %v", n, err)
		}
		if n > 0 {
			if _, err := blob.ZstdDecode(frame, int64(n-1)); err == nil {
				t.Fatalf("n=%d: size cap not enforced", n)
			}
			bad := bytes.Clone(frame)
			bad[len(bad)-1] ^= 1
			if _, err := blob.ZstdDecode(bad, int64(n)); err == nil {
				t.Fatalf("n=%d: corrupted checksum accepted", n)
			}
		}
	}
}
