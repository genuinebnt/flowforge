package textindex

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// Directory is the storage port of an index: a flat namespace of
// immutable files. WriteFile must be atomic (readers and a crash see the
// old content or the new, never a mix); that is how the commit point is
// swapped. ReadFile of a missing file returns an error wrapping
// fs.ErrNotExist.
type Directory interface {
	ReadFile(name string) ([]byte, error)
	WriteFile(name string, data []byte) error
	Remove(name string) error
	List() ([]string, error) // sorted
}

// MemDirectory is the in-memory Directory (tests, ephemeral indexes).
type MemDirectory struct {
	mu    sync.Mutex
	files map[string][]byte
}

// NewMemDirectory returns an empty in-memory directory.
func NewMemDirectory() *MemDirectory { return &MemDirectory{files: map[string][]byte{}} }

// ReadFile implements Directory.
func (m *MemDirectory) ReadFile(name string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.files[name]
	if !ok {
		return nil, fmt.Errorf("textindex: %s: %w", name, fs.ErrNotExist)
	}
	return b, nil
}

// WriteFile implements Directory. The data is copied.
func (m *MemDirectory) WriteFile(name string, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.files[name] = slices.Clone(data)
	return nil
}

// Remove implements Directory.
func (m *MemDirectory) Remove(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.files, name)
	return nil
}

// List implements Directory.
func (m *MemDirectory) List() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.files))
	for n := range m.files {
		out = append(out, n)
	}
	slices.Sort(out)
	return out, nil
}

// FSDirectory stores files in an operating-system directory. WriteFile
// writes a temporary file, syncs it and renames it into place.
type FSDirectory struct{ path string }

// NewFSDirectory creates path if needed and returns a directory over it.
func NewFSDirectory(path string) (*FSDirectory, error) {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, err
	}
	return &FSDirectory{path: path}, nil
}

func (d *FSDirectory) full(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, `/\`) || name == "." || name == ".." {
		return "", fmt.Errorf("%w: bad file name %q", ErrInvalidConfig, name)
	}
	return filepath.Join(d.path, name), nil
}

// ReadFile implements Directory.
func (d *FSDirectory) ReadFile(name string) ([]byte, error) {
	p, err := d.full(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// WriteFile implements Directory.
func (d *FSDirectory) WriteFile(name string, data []byte) error {
	p, err := d.full(name)
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		return err
	}
	if dir, err := os.Open(d.path); err == nil {
		_ = dir.Sync()
		dir.Close()
	}
	return nil
}

// Remove implements Directory.
func (d *FSDirectory) Remove(name string) error {
	p, err := d.full(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// List implements Directory (temporary files are not listed).
func (d *FSDirectory) List() ([]string, error) {
	es, err := os.ReadDir(d.path)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range es {
		if !e.IsDir() && !strings.HasSuffix(e.Name(), ".tmp") {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out, nil
}
