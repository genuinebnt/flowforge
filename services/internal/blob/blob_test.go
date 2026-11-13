package blob_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"google.golang.org/api/option"

	"cloud.google.com/go/storage"

	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/services/internal/blob"
)

// fakeGCS implements the subset of the Cloud Storage JSON API the GCS
// adapter uses, and nothing more: multipart upload
// (POST /upload/storage/v1/b/{bucket}/o?uploadType=multipart), media
// download (GET /storage/v1/b/{bucket}/o/{object}?alt=media, with
// storage.WithJSONReads) and delete (DELETE /storage/v1/b/{bucket}/o/{object}).
// It returns the CRC32C the client verifies after an upload. Not covered:
// resumable uploads, preconditions, ACLs, retries on 5xx, the XML read
// path and authentication.
type fakeGCS struct {
	mu      sync.Mutex
	objects map[string][]byte // bucket/name
	reqs    []string
}

func (f *fakeGCS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
	f.mu.Unlock()
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/upload/storage/v1/b/"):
		bucket := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/upload/storage/v1/b/"), "/o")
		if r.URL.Query().Get("uploadType") != "multipart" {
			http.Error(w, "only multipart uploads", http.StatusNotImplemented)
			return
		}
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		meta, err := mr.NextPart()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var obj struct {
			Name        string `json:"name"`
			ContentType string `json:"contentType"`
		}
		if err := json.NewDecoder(meta).Decode(&obj); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		media, err := mr.NextPart()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(media)
		f.mu.Lock()
		f.objects[bucket+"/"+obj.Name] = data
		f.mu.Unlock()
		var crc [4]byte
		binary.BigEndian.PutUint32(crc[:], crc32.Checksum(data, crc32.MakeTable(crc32.Castagnoli)))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"bucket": bucket, "name": obj.Name, "size": itoa(len(data)),
			"contentType": obj.ContentType, "generation": "1", "crc32c": base64.StdEncoding.EncodeToString(crc[:])})
	case strings.HasPrefix(r.URL.Path, "/storage/v1/b/"):
		rest := strings.TrimPrefix(r.URL.Path, "/storage/v1/b/")
		bucket, name, ok := strings.Cut(rest, "/o/")
		if !ok {
			http.NotFound(w, r)
			return
		}
		name, _ = url.PathUnescape(name)
		key := bucket + "/" + name
		f.mu.Lock()
		data, found := f.objects[key]
		if found && r.Method == http.MethodDelete {
			delete(f.objects, key)
		}
		f.mu.Unlock()
		if !found {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"No such object"}}`))
			return
		}
		switch {
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Query().Get("alt") == "media":
			w.Header().Set("Content-Length", itoa(len(data)))
			w.Header().Set("X-Goog-Generation", "1")
			_, _ = w.Write(data)
		default:
			http.Error(w, "unsupported", http.StatusNotImplemented)
		}
	default:
		http.NotFound(w, r)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func newGCS(t *testing.T) (*blob.GCS, *fakeGCS) {
	t.Helper()
	f := &fakeGCS{objects: map[string][]byte{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	st, closeFn, err := blob.Open(context.Background(), blob.Config{Bucket: "ff-artifacts", ClientOptions: []option.ClientOption{
		option.WithEndpoint(srv.URL + "/storage/v1/"), option.WithoutAuthentication(), storage.WithJSONReads()}})
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
	if len(fake.objects) != 0 {
		t.Fatalf("delete left %v", fake.objects)
	}
}

func TestZstdStoredRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 4096, 128 << 10, 128<<10 + 7, 400 << 10} {
		in := bytes.Repeat([]byte(`{"seq":1,"type":"x"}`+"\n"), n/21+1)[:n]
		frame := blob.ZstdStored(in)
		if binary.LittleEndian.Uint32(frame) != 0xFD2FB528 {
			t.Fatal("magic")
		}
		out, err := blob.ZstdDecodeStored(frame)
		if err != nil || !bytes.Equal(out, in) {
			t.Fatalf("n=%d: %v", n, err)
		}
		frame[len(frame)-1] ^= 1
		if _, err := blob.ZstdDecodeStored(frame); err == nil {
			t.Fatalf("n=%d: corrupted checksum accepted", n)
		}
	}
}
