package engine

import (
	"context"
	"slices"
	"sync"

	"github.com/genuinebnt/flowforge/engine"
)

// MemTools is the in-memory engine.ToolRegistry. It keeps every
// registered version; ListTools returns the latest of each name.
type MemTools struct {
	mu    sync.Mutex
	specs map[string][]engine.ToolSpec
}

var _ engine.ToolRegistry = (*MemTools)(nil)

// NewMemTools returns a registry holding specs.
func NewMemTools(specs ...engine.ToolSpec) *MemTools {
	t := &MemTools{specs: map[string][]engine.ToolSpec{}}
	for _, s := range specs {
		t.Register(s)
	}
	return t
}

// Register adds a tool version and returns it with its SpecSHA256.
func (t *MemTools) Register(s engine.ToolSpec) engine.ToolSpec {
	s.SpecSHA256 = s.ComputeSHA256()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.specs[s.Name] = append(t.specs[s.Name], s)
	return s
}

// Spec returns the latest version of a tool.
func (t *MemTools) Spec(name string) (engine.ToolSpec, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	vs := t.specs[name]
	if len(vs) == 0 {
		return engine.ToolSpec{}, false
	}
	return vs[len(vs)-1], true
}

// ListTools implements engine.ToolRegistry.
func (t *MemTools) ListTools(ctx context.Context) ([]engine.ToolSpec, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	names := make([]string, 0, len(t.specs))
	for n := range t.specs {
		names = append(names, n)
	}
	slices.Sort(names)
	out := make([]engine.ToolSpec, 0, len(names))
	for _, n := range names {
		vs := t.specs[n]
		out = append(out, vs[len(vs)-1])
	}
	return out, nil
}

// ResolveTool implements engine.ToolRegistry.
func (t *MemTools) ResolveTool(ctx context.Context, name string, sha [32]byte) (engine.ToolSpec, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.specs[name] {
		if s.SpecSHA256 == sha {
			return s, nil
		}
	}
	return engine.ToolSpec{}, engine.ErrNotFound
}
