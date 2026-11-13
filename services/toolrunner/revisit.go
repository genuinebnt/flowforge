package toolrunner

import (
	"fmt"
	"sync"
	"time"
)

// RevisitPolicy decides how long crawl waits before fetching a URL again
// (V2_DESIGN §11 item 8). It is a pluggable algorithm (docs/ALGORITHMS.md,
// "Crawl revisit policy"): AdaptiveRevisit is the default, FixedRevisit the
// baseline it is compared with (BenchmarkToolAlgo/revisit).
type RevisitPolicy interface {
	// Name identifies the policy in configuration and benchmarks.
	Name() string
	// Next returns the interval to wait after a visit. prev is the
	// interval that led to this visit (0 on the first visit) and changed
	// reports whether the visit found a change (a structural or level-3
	// change; masked-only and reordered versions are not changes).
	Next(prev time.Duration, changed bool) time.Duration
}

// Revisit bounds of V2_DESIGN §11 item 8.
const (
	RevisitMin     = time.Hour
	RevisitMax     = 7 * 24 * time.Hour
	RevisitInitial = 24 * time.Hour
)

// AdaptiveRevisit halves the interval when a visit finds a change and
// doubles it when it finds none, clamped to [Min, Max] (V2_DESIGN §11
// item 8: [1 h, 7 d]); the first visit starts at Initial. Pages that
// change often converge to Min, stable pages to Max, so fetches follow
// the change rate (a multiplicative-increase / multiplicative-decrease
// estimator of the change interval).
type AdaptiveRevisit struct {
	// Initial, Min and Max default to RevisitInitial, RevisitMin and
	// RevisitMax.
	Initial, Min, Max time.Duration
}

// Name implements RevisitPolicy.
func (AdaptiveRevisit) Name() string { return "adaptive" }

// Next implements RevisitPolicy.
func (p AdaptiveRevisit) Next(prev time.Duration, changed bool) time.Duration {
	lo, hi, init := p.Min, p.Max, p.Initial
	if lo <= 0 {
		lo = RevisitMin
	}
	if hi <= 0 {
		hi = RevisitMax
	}
	if init <= 0 {
		init = RevisitInitial
	}
	next := init
	if prev > 0 {
		if changed {
			next = prev / 2
		} else {
			next = prev * 2
		}
	}
	return min(max(next, lo), hi)
}

// FixedRevisit revisits every Interval (default RevisitInitial) whatever
// a visit finds: the baseline the adaptive policy is measured against.
type FixedRevisit struct{ Interval time.Duration }

// Name implements RevisitPolicy.
func (FixedRevisit) Name() string { return "fixed" }

// Next implements RevisitPolicy.
func (p FixedRevisit) Next(time.Duration, bool) time.Duration {
	if p.Interval <= 0 {
		return RevisitInitial
	}
	return p.Interval
}

// ParseRevisitPolicy returns the policy named by s ("" or "adaptive":
// AdaptiveRevisit; "fixed": FixedRevisit), the FF_CRAWL_REVISIT setting.
func ParseRevisitPolicy(s string) (RevisitPolicy, error) {
	switch s {
	case "", "adaptive":
		return AdaptiveRevisit{}, nil
	case "fixed":
		return FixedRevisit{}, nil
	}
	return nil, fmt.Errorf("toolrunner: unknown revisit policy %q (adaptive, fixed)", s)
}

// RevisitState is what crawl remembers about one (program, URL, profile).
type RevisitState struct {
	// Interval is the policy's current interval; Due is when the URL may
	// be fetched again.
	Interval time.Duration
	Due      time.Time
	// Visits and Changes count fetches and the fetches that found a change.
	Visits, Changes int
}

// Visit returns the state after a fetch at now that did or did not find
// a change.
func (s RevisitState) Visit(p RevisitPolicy, now time.Time, changed bool) RevisitState {
	s.Interval = p.Next(s.Interval, changed)
	s.Due = now.Add(s.Interval)
	s.Visits++
	if changed {
		s.Changes++
	}
	return s
}

// RevisitStore keeps revisit states. Implementations are safe for
// concurrent use.
type RevisitStore interface {
	Get(key string) (RevisitState, bool)
	Put(key string, s RevisitState)
}

// MemRevisitStore is the in-memory RevisitStore: per tool-runner process,
// lost on restart (every URL is then due, which errs on fetching).
type MemRevisitStore struct {
	mu sync.Mutex
	m  map[string]RevisitState
}

// NewMemRevisitStore returns an empty store.
func NewMemRevisitStore() *MemRevisitStore { return &MemRevisitStore{m: map[string]RevisitState{}} }

// Get implements RevisitStore.
func (s *MemRevisitStore) Get(key string) (RevisitState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[key]
	return v, ok
}

// Put implements RevisitStore.
func (s *MemRevisitStore) Put(key string, v RevisitState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[key] = v
}
