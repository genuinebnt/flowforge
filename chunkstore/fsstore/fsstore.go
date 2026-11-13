// Package fsstore is the directory adapter of the chunkstore Backend
// port: each object is one file under a root directory, its name mapped
// to a relative path ("t/acme/packs/<hex>.pack" →
// <root>/t/acme/packs/<hex>.pack).
//
// Writes are atomic: Put writes "<name>.tmp-<n>" in the target directory
// (created with O_EXCL), fsyncs it, closes it, renames it over the target
// and fsyncs the directory. A reader therefore sees the old file or the
// new one, never a prefix, and after Put returns the object survives a
// power loss.
//
// Fsync policy: by default (Options.NoSync false) Put fsyncs the file
// before the rename and the directory after it; Delete fsyncs the
// directory after removing; directories created on the way are fsynced
// with their parent. NoSync skips every fsync (tests, throwaway stores):
// atomicity under a process crash still holds because rename is atomic,
// but durability under power loss does not.
//
// Temporary files are never listed. A crash can leave one behind; Clean
// removes them and is safe only while no Put is running.
package fsstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"

	"github.com/genuinebnt/flowforge/chunkstore"
)

// tmpMarker marks temporary files. chunkstore.ValidName allows '.' and '-',
// so path also refuses names containing it: an object can never be
// mistaken for (or Clean'ed as) a temporary file.
const tmpMarker = ".tmp-"

// Options configures a Dir.
type Options struct {
	// NoSync disables fsync (see the package comment).
	NoSync bool
}

// Dir is a chunkstore.Backend over a directory. It is safe for concurrent
// use within one process.
type Dir struct {
	root string
	opts Options
	seq  atomic.Uint64
}

var _ chunkstore.Backend = (*Dir)(nil)

// New returns a backend rooted at root, creating the directory if needed.
func New(root string, opts Options) (*Dir, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	return &Dir{root: abs, opts: opts}, nil
}

// path maps a valid object name to its file path under the root.
func (d *Dir) path(name string) (string, error) {
	if !chunkstore.ValidName(name) || strings.Contains(name, tmpMarker) {
		return "", &chunkstore.Error{Code: chunkstore.CodeInvalidName, Object: name, Detail: "invalid object name"}
	}
	return filepath.Join(d.root, filepath.FromSlash(name)), nil
}

// notFound is the chunkstore.ErrObjectNotFound error for name.
func notFound(name string) error {
	return &chunkstore.Error{Code: chunkstore.CodeObjectNotFound, Object: name, Detail: "no such object"}
}

// syncDir fsyncs a directory so a rename or removal in it is durable.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// mkdirs creates dir and its missing parents up to the root, fsyncing
// each new directory's parent.
func (d *Dir) mkdirs(dir string) error {
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	parent := filepath.Dir(dir)
	if parent != dir && strings.HasPrefix(parent, d.root) {
		if err := d.mkdirs(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	if !d.opts.NoSync {
		return syncDir(parent)
	}
	return nil
}

// Put implements chunkstore.Backend.
func (d *Dir) Put(ctx context.Context, name string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := d.path(name)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := d.mkdirs(dir); err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s%s%d", p, tmpMarker, d.seq.Add(1))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if !d.opts.NoSync {
		if err := f.Sync(); err != nil {
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	ok = true
	if !d.opts.NoSync {
		return syncDir(dir)
	}
	return nil
}

// Get implements chunkstore.Backend.
func (d *Dir) Get(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := d.path(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, notFound(name)
	}
	return b, err
}

// GetRange implements chunkstore.Backend.
func (d *Dir) GetRange(ctx context.Context, name string, off int64, n int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := d.path(name)
	if err != nil {
		return nil, err
	}
	if off < 0 || n < 0 {
		return nil, &chunkstore.Error{Code: chunkstore.CodeBackend, Object: name, Detail: "negative range"}
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, notFound(name)
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, off); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &chunkstore.Error{Code: chunkstore.CodeBackend, Object: name, Detail: fmt.Sprintf("range [%d,+%d) beyond end of object", off, n)}
		}
		return nil, err
	}
	return buf, nil
}

// List implements chunkstore.Backend.
func (d *Dir) List(ctx context.Context, prefix string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var out []string
	err := filepath.WalkDir(d.root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(d.root, p)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if strings.Contains(name, tmpMarker) || !chunkstore.ValidName(name) {
			return nil
		}
		if strings.HasPrefix(name, prefix) {
			out = append(out, name)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}

// Delete implements chunkstore.Backend.
func (d *Dir) Delete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p, err := d.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if !d.opts.NoSync {
		return syncDir(filepath.Dir(p))
	}
	return nil
}

// Clean removes temporary files left by interrupted Puts and returns how
// many it removed. Run it only while no Put is in progress.
func (d *Dir) Clean(ctx context.Context) (int, error) {
	n := 0
	err := filepath.WalkDir(d.root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if !e.IsDir() && strings.Contains(e.Name(), tmpMarker) {
			if err := os.Remove(p); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	return n, err
}
