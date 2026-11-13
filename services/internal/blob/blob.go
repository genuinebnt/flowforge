// Package blob is the object storage of the services (G-OPS-14): raw tool
// output (observation raw_ref, spec 10.5), graph snapshot files (spec
// 21.4) and run archives (spec 10.7). The port is graphstore.BlobStore, so
// the graph store writes snapshots through the same adapters:
//
//   - memory (graphstore.MemBlobs, "mem://" URIs): one-process runs;
//   - a local directory (graphstore.DirBlobs, "file://" URIs): FF_BLOB_DIR;
//   - Cloud Storage (GCS, "gs://bucket/name" URIs): FF_ARTIFACT_BUCKET.
//
// Open picks one from configuration; the bucket wins over the directory.
package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"

	"github.com/genuinebnt/flowforge/graphstore"
)

// Store is the blob port (Put returns the URI to record; Get reads one).
type Store = graphstore.BlobStore

// Config selects the adapter.
type Config struct {
	Bucket string // FF_ARTIFACT_BUCKET
	Dir    string // FF_BLOB_DIR
	// ClientOptions are passed to storage.NewClient (tests point it at a
	// fake; production uses Application Default Credentials).
	ClientOptions []option.ClientOption
}

// Kind names the adapter Open picks for c: "gcs", "dir" or "mem".
func Kind(c Config) string {
	switch {
	case c.Bucket != "":
		return "gcs"
	case c.Dir != "":
		return "dir"
	}
	return "mem"
}

// Open returns the configured store and a function that releases it.
func Open(ctx context.Context, c Config) (Store, func(), error) {
	switch Kind(c) {
	case "gcs":
		cl, err := storage.NewClient(ctx, c.ClientOptions...)
		if err != nil {
			return nil, nil, fmt.Errorf("blob: storage client: %w", err)
		}
		return &GCS{Client: cl, Bucket: c.Bucket}, func() { _ = cl.Close() }, nil
	case "dir":
		return graphstore.DirBlobs{Root: c.Dir}, func() {}, nil
	}
	return &graphstore.MemBlobs{}, func() {}, nil
}

// GCS is a Store on one Cloud Storage bucket. Objects are written in one
// request (no resumable upload: every blob here is built in memory first)
// and the client library checks the server's CRC32C of each upload. Get
// serves only URIs of its own bucket.
type GCS struct {
	Client *storage.Client
	Bucket string
}

// URI returns the gs:// URI of name.
func (g *GCS) URI(name string) string { return "gs://" + g.Bucket + "/" + name }

// Put implements Store.
func (g *GCS) Put(ctx context.Context, name string, data []byte) (string, error) {
	if err := validName(name); err != nil {
		return "", err
	}
	w := g.Client.Bucket(g.Bucket).Object(name).NewWriter(ctx)
	w.ChunkSize = 0 // single-shot multipart upload
	w.ContentType = contentType(name)
	if _, err := io.Copy(w, bytes.NewReader(data)); err != nil {
		_ = w.Close()
		return "", fmt.Errorf("blob: write %s: %w", name, err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("blob: write %s: %w", name, err)
	}
	return g.URI(name), nil
}

// Get implements Store.
func (g *GCS) Get(ctx context.Context, uri string) ([]byte, error) {
	name, ok := strings.CutPrefix(uri, "gs://"+g.Bucket+"/")
	if !ok || validName(name) != nil {
		return nil, fmt.Errorf("%w: %s", graphstore.ErrBlobNotFound, uri)
	}
	r, err := g.Client.Bucket(g.Bucket).Object(name).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil, fmt.Errorf("%w: %s", graphstore.ErrBlobNotFound, uri)
	}
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

// Delete removes the object behind uri; a missing object is not an error.
func (g *GCS) Delete(ctx context.Context, uri string) error {
	name, ok := strings.CutPrefix(uri, "gs://"+g.Bucket+"/")
	if !ok {
		return fmt.Errorf("%w: %s", graphstore.ErrBlobNotFound, uri)
	}
	err := g.Client.Bucket(g.Bucket).Object(name).Delete(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		return nil
	}
	return err
}

func validName(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("blob: invalid name %q", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("blob: invalid name %q", name)
		}
	}
	return nil
}

func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	case strings.HasSuffix(name, ".jsonl.zst"):
		return "application/zstd"
	}
	return "application/octet-stream"
}

var _ Store = (*GCS)(nil)
