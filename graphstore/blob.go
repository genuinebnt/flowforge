package graphstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// BlobStore is the object-storage port snapshot files are written
// through (GCS in production, spec 21.4). Names are slash-separated
// relative paths; Put returns the URI recorded in the catalogue.
type BlobStore interface {
	Put(ctx context.Context, name string, data []byte) (uri string, err error)
	Get(ctx context.Context, uri string) ([]byte, error)
}

// ErrBlobNotFound is returned by Get for an unknown URI.
var ErrBlobNotFound = errors.New("graphstore: blob not found")

func validBlobName(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return fmt.Errorf("graphstore: invalid blob name %q", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("graphstore: invalid blob name %q", name)
		}
	}
	return nil
}

// MemBlobs is an in-memory BlobStore with URIs "mem://<name>". The zero
// value is ready to use and safe for concurrent use.
type MemBlobs struct {
	mu   sync.Mutex
	data map[string][]byte
}

// Put implements BlobStore.
func (m *MemBlobs) Put(ctx context.Context, name string, data []byte) (string, error) {
	if err := validBlobName(name); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data == nil {
		m.data = map[string][]byte{}
	}
	uri := "mem://" + name
	m.data[uri] = append([]byte(nil), data...)
	return uri, nil
}

// Get implements BlobStore.
func (m *MemBlobs) Get(ctx context.Context, uri string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[uri]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrBlobNotFound, uri)
	}
	return append([]byte(nil), b...), nil
}

// DirBlobs is a BlobStore on the local filesystem under Root, with URIs
// "file://<absolute path>". Files are written atomically (temporary file,
// fsync, rename).
type DirBlobs struct {
	Root string
}

func (d DirBlobs) path(name string) (string, error) {
	if err := validBlobName(name); err != nil {
		return "", err
	}
	root, err := filepath.Abs(d.Root)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(name)), nil
}

// Put implements BlobStore.
func (d DirBlobs) Put(ctx context.Context, name string, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p, err := d.path(name)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp, p)
	}
	if werr != nil {
		_ = os.Remove(tmp)
		return "", werr
	}
	return "file://" + filepath.ToSlash(p), nil
}

// Get implements BlobStore. Only URIs under Root are served.
func (d DirBlobs) Get(ctx context.Context, uri string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, ok := strings.CutPrefix(uri, "file://")
	root, err := filepath.Abs(d.Root)
	if err != nil {
		return nil, err
	}
	p = filepath.FromSlash(p)
	if rel, rerr := filepath.Rel(root, p); !ok || rerr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%w: %s", ErrBlobNotFound, uri)
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrBlobNotFound, uri)
	}
	return b, err
}
