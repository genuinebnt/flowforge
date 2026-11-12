package csr

import (
	"fmt"
	"sync/atomic"
)

// Holder publishes the current snapshot of one (tenant, program) to
// concurrent readers (spec 21.4). Readers Load a *Graph and keep using it
// until they finish, even if a newer snapshot is published meanwhile. The
// zero value holds nothing and is ready to use.
type Holder struct {
	p atomic.Pointer[Graph]
}

// Load returns the current snapshot, or nil before the first Publish.
func (h *Holder) Load() *Graph { return h.p.Load() }

// Publish atomically replaces the current snapshot with g and returns the
// previous one (nil if none). It refuses a snapshot for another tenant or
// program (ErrInvalidInput) or with a lower watermark (ErrStaleSnapshot),
// so a slow rebuild can never roll readers back.
//
// The Holder never closes snapshots: the caller closes the returned one
// once its readers are done (for example after a grace period).
func (h *Holder) Publish(g *Graph) (*Graph, error) {
	if g == nil {
		return nil, fmt.Errorf("%w: nil snapshot", ErrInvalidInput)
	}
	for {
		old := h.p.Load()
		if old != nil {
			if old.meta.TenantID != g.meta.TenantID || old.meta.ProgramID != g.meta.ProgramID {
				return nil, fmt.Errorf("%w: snapshot for another tenant or program", ErrInvalidInput)
			}
			if g.meta.WatermarkSeq < old.meta.WatermarkSeq {
				return nil, fmt.Errorf("%w: watermark %d < %d", ErrStaleSnapshot, g.meta.WatermarkSeq, old.meta.WatermarkSeq)
			}
		}
		if h.p.CompareAndSwap(old, g) {
			return old, nil
		}
	}
}
