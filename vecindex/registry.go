package vecindex

import (
	"fmt"
	"slices"
	"sync"

	"github.com/genuinebnt/flowforge/core"
)

// Key names one index: spec 22.3 keeps one index per (tenant_id,
// program_id, model_id), so tenant and program filtering is structural and
// erasure of a program is deleting its indexes. Model should name the model
// and its version (for example "e5-base@2"): an index never mixes vectors
// of two model versions (spec 22.1), and a model upgrade builds a second
// index under a new key (spec 22.10).
type Key struct {
	Tenant  core.UUID
	Program core.UUID
	Model   string
}

func (k Key) validate() error {
	if k.Tenant.IsZero() || k.Program.IsZero() || k.Model == "" {
		return fmt.Errorf("%w: tenant=%s program=%s model=%q", ErrInvalidKey, k.Tenant, k.Program, k.Model)
	}
	return nil
}

// Registry maps keys to indexes. Every search names its full key, so a
// query in one tenant can never reach another tenant's index, even when
// both hold identical vectors (vec_008). The zero value is an empty
// registry. It is safe for concurrent use.
type Registry struct {
	mu sync.RWMutex
	m  map[Key]Index
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{m: make(map[Key]Index)} }

// Register adds idx under k.
func (r *Registry) Register(k Key, idx Index) error {
	if err := k.validate(); err != nil {
		return err
	}
	if idx == nil {
		return fmt.Errorf("%w: nil index", ErrInvalidConfig)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = make(map[Key]Index)
	}
	if _, ok := r.m[k]; ok {
		return fmt.Errorf("%w: %s/%s/%s", ErrIndexExists, k.Tenant, k.Program, k.Model)
	}
	r.m[k] = idx
	return nil
}

// Lookup returns the index registered under k.
func (r *Registry) Lookup(k Key) (Index, error) {
	if err := k.validate(); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	idx, ok := r.m[k]
	if !ok {
		return nil, fmt.Errorf("%w: %s/%s/%s", ErrNoIndex, k.Tenant, k.Program, k.Model)
	}
	return idx, nil
}

// Remove unregisters k and returns its index so the caller can close or
// destroy it.
func (r *Registry) Remove(k Key) (Index, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	idx, ok := r.m[k]
	delete(r.m, k)
	return idx, ok
}

// Keys returns the keys registered for tenant, sorted by program then
// model.
func (r *Registry) Keys(tenant core.UUID) []Key {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Key
	for k := range r.m {
		if k.Tenant == tenant {
			out = append(out, k)
		}
	}
	slices.SortFunc(out, func(a, b Key) int {
		if c := compareID(a.Program, b.Program); c != 0 {
			return c
		}
		switch {
		case a.Model < b.Model:
			return -1
		case a.Model > b.Model:
			return 1
		}
		return 0
	})
	return out
}

// Upsert writes to the index under k.
func (r *Registry) Upsert(k Key, id core.UUID, v []float32, seq uint64) error {
	idx, err := r.Lookup(k)
	if err != nil {
		return err
	}
	return idx.Upsert(id, v, seq)
}

// Delete deletes from the index under k.
func (r *Registry) Delete(k Key, id core.UUID, seq uint64) error {
	idx, err := r.Lookup(k)
	if err != nil {
		return err
	}
	return idx.Delete(id, seq)
}

// Search searches the index under k.
func (r *Registry) Search(k Key, q []float32, p SearchParams) ([]Hit, error) {
	idx, err := r.Lookup(k)
	if err != nil {
		return nil, err
	}
	return idx.Search(q, p)
}

// SearchPrograms is a cross-program query within one tenant (spec 22.3):
// it fans out over the tenant's indexes for model in the listed programs
// and merges the top k. It never consults another tenant's indexes.
// Programs without an index for model are skipped.
func (r *Registry) SearchPrograms(tenant core.UUID, programs []core.UUID, model string, q []float32, p SearchParams) ([]Hit, error) {
	np, err := p.normalized()
	if err != nil {
		return nil, err
	}
	var lists [][]Hit
	for _, prog := range programs {
		k := Key{Tenant: tenant, Program: prog, Model: model}
		if err := k.validate(); err != nil {
			return nil, err
		}
		r.mu.RLock()
		idx, ok := r.m[k]
		r.mu.RUnlock()
		if !ok {
			continue
		}
		hits, err := idx.Search(q, p)
		if err != nil {
			return nil, fmt.Errorf("vecindex: search program %s: %w", prog, err)
		}
		lists = append(lists, hits)
	}
	return mergeSorted(lists, np.K), nil
}
