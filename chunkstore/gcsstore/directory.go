package gcsstore

import (
	"context"
	"fmt"
	"io/fs"
	"strings"

	"cloud.google.com/go/storage"

	"github.com/genuinebnt/flowforge/textindex"
)

// Directory is a textindex.Directory on the objects "<prefix><file>" of
// one bucket. WriteFile is atomic (one object write) and conditional on
// the observed generation, like Backend.Put; textindex replaces only its
// commit file and removes leftovers it has listed, so one writer works
// unchanged and a second one fails with ErrConflict. textindex.Directory
// takes no context; Ctx (default context.Background()) bounds every call.
type Directory struct {
	o      *objects
	prefix string
	// Ctx is the context of every request.
	Ctx context.Context
}

var _ textindex.Directory = (*Directory)(nil)

// NewDirectory returns the directory at prefix ("index/<tenant>/<program>/":
// relative, ending in "/").
func NewDirectory(c *storage.Client, bucket, prefix string) (*Directory, error) {
	if prefix == "" || !strings.HasSuffix(prefix, "/") || strings.HasPrefix(prefix, "/") {
		return nil, fmt.Errorf("gcsstore: directory prefix %q must be relative and end in /", prefix)
	}
	o, err := newObjects(c, bucket)
	if err != nil {
		return nil, err
	}
	return &Directory{o: o, prefix: prefix, Ctx: context.Background()}, nil
}

// name checks a file name the way textindex.FSDirectory does (no
// separators, not "." or "..").
func (d *Directory) name(file string) (string, error) {
	if file == "" || strings.ContainsAny(file, `/\`) || file == "." || file == ".." {
		return "", fmt.Errorf("%w: bad file name %q", textindex.ErrInvalidConfig, file)
	}
	return d.prefix + file, nil
}

// ReadFile implements textindex.Directory; a missing file wraps
// fs.ErrNotExist.
func (d *Directory) ReadFile(file string) ([]byte, error) {
	n, err := d.name(file)
	if err != nil {
		return nil, err
	}
	data, found, err := d.o.get(d.Ctx, n)
	if err != nil {
		return nil, fmt.Errorf("gcsstore: read %s: %w", n, err)
	}
	if !found {
		return nil, fmt.Errorf("gcsstore: %s: %w", n, fs.ErrNotExist)
	}
	return data, nil
}

// WriteFile implements textindex.Directory.
func (d *Directory) WriteFile(file string, data []byte) error {
	n, err := d.name(file)
	if err != nil {
		return err
	}
	if err := d.o.put(d.Ctx, n, data); err != nil {
		return fmt.Errorf("gcsstore: write %s: %w", n, err)
	}
	return nil
}

// Remove implements textindex.Directory; removing a missing file
// succeeds.
func (d *Directory) Remove(file string) error {
	n, err := d.name(file)
	if err != nil {
		return err
	}
	if err := d.o.del(d.Ctx, n); err != nil {
		return fmt.Errorf("gcsstore: remove %s: %w", n, err)
	}
	return nil
}

// List implements textindex.Directory: the files directly under the
// prefix, sorted.
func (d *Directory) List() ([]string, error) {
	names, err := d.o.list(d.Ctx, d.prefix)
	if err != nil {
		return nil, fmt.Errorf("gcsstore: list %s: %w", d.prefix, err)
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if f := strings.TrimPrefix(n, d.prefix); f != "" && !strings.Contains(f, "/") {
			out = append(out, f)
		}
	}
	return out, nil
}
