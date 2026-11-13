package chunkstore

import (
	"context"
	"slices"
	"strings"
	"sync"
)

// Backend is the storage port: a flat namespace of named, immutable-ish
// objects. Names are validated by ValidName ("ref", "packs/<hex>.pack",
// "packs/<hex>.idx").
//
// Contract (every implementation, checked by the backend conformance
// tests):
//   - Put is atomic: a concurrent or later Get sees the old object or the
//     new one in full, never a mix or a prefix, even across a crash.
//     Put replaces an existing object.
//   - Get and GetRange of a missing object return an error for which
//     errors.Is(err, ErrObjectNotFound) holds. GetRange returns exactly n
//     bytes or an error.
//   - List returns the names with the prefix in ascending byte order.
//   - Delete of a missing object succeeds (deletes are idempotent).
//
// The Store assumes a single writer per backend (one Store process).
type Backend interface {
	Put(ctx context.Context, name string, data []byte) error
	Get(ctx context.Context, name string) ([]byte, error)
	GetRange(ctx context.Context, name string, off int64, n int) ([]byte, error)
	List(ctx context.Context, prefix string) ([]string, error)
	Delete(ctx context.Context, name string) error
}

// ValidName reports whether name is an acceptable object name: 1 to 255
// bytes of [a-z0-9._-] in '/'-separated segments, none empty, "." or "..".
// Every backend must reject other names with CodeInvalidName so the
// namespace maps safely onto file systems and object stores.
func ValidName(name string) bool {
	if name == "" || len(name) > 255 {
		return false
	}
	for seg := range strings.SplitSeq(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		for i := 0; i < len(seg); i++ {
			c := seg[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
				return false
			}
		}
	}
	return true
}

func checkName(name string) error {
	if !ValidName(name) {
		return errf(CodeInvalidName, name, "invalid object name")
	}
	return nil
}

// MemBackend is the in-memory Backend. It is safe for concurrent use.
type MemBackend struct {
	mu   sync.RWMutex
	objs map[string][]byte
}

// NewMemBackend returns an empty in-memory backend.
func NewMemBackend() *MemBackend { return &MemBackend{objs: map[string][]byte{}} }

// Put implements Backend.
func (m *MemBackend) Put(ctx context.Context, name string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkName(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[name] = slices.Clone(data)
	return nil
}

// Get implements Backend.
func (m *MemBackend) Get(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkName(name); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.objs[name]
	if !ok {
		return nil, errf(CodeObjectNotFound, name, "no such object")
	}
	return slices.Clone(b), nil
}

// GetRange implements Backend.
func (m *MemBackend) GetRange(ctx context.Context, name string, off int64, n int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := checkName(name); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	b, ok := m.objs[name]
	if !ok {
		return nil, errf(CodeObjectNotFound, name, "no such object")
	}
	if off < 0 || n < 0 || off > int64(len(b)) || int64(n) > int64(len(b))-off {
		return nil, errf(CodeBackend, name, "range [%d,+%d) outside object of %d bytes", off, n, len(b))
	}
	return slices.Clone(b[off : off+int64(n)]), nil
}

// List implements Backend.
func (m *MemBackend) List(ctx context.Context, prefix string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []string
	for k := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out, nil
}

// Delete implements Backend.
func (m *MemBackend) Delete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := checkName(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, name)
	return nil
}
