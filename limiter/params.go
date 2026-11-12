package limiter

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// MaxBurst bounds Params.Burst.
const MaxBurst = 1 << 30

// maxSpan bounds every parameter-derived duration (burst*T, windows) so
// that sums of a time and a span never overflow int64.
const maxSpan = int64(1) << 61

// unit is the fixed-point scale for token counts: one token is 1e9 units,
// so a rate in tokens per second is also a rate in units per nanosecond.
const unit = int64(1e9)

// Params configures one class of keys. Every algorithm reads Rate and
// Burst; the window algorithms also read Window.
type Params struct {
	// Rate is the sustained rate in tokens per second (> 0, at most 1e9).
	Rate float64
	// Burst is the capacity: the token bucket size, the GCRA burst, the
	// sliding-window limit L or the leaky-bucket queue length (1..MaxBurst).
	Burst int
	// Window is the window length W for the sliding-window algorithms.
	// Zero means Burst/Rate, which makes the window algorithms admit the
	// same long-run rate as the others.
	Window time.Duration
}

// EmissionInterval returns the GCRA emission interval T = 1/Rate, rounded
// up to a whole nanosecond so rounding never admits more than Rate.
func (p Params) EmissionInterval() time.Duration {
	return time.Duration(math.Ceil(1e9 / p.Rate))
}

// Tolerance returns the GCRA tolerance tau = (Burst - 1) * T.
func (p Params) Tolerance() time.Duration {
	return time.Duration(p.Burst-1) * p.EmissionInterval()
}

// Validate reports whether p is usable; the error wraps ErrInvalidParams.
func (p Params) Validate() error {
	_, err := compile(p)
	return err
}

// compiled holds the derived, integer forms of a Params.
type compiled struct {
	p        Params
	burst    int64
	t        int64   // emission interval, ns
	tau      int64   // GCRA tolerance, ns
	window   int64   // sliding window length, ns
	rate     float64 // tokens per second == units per nanosecond
	capUnits int64   // burst in fixed-point units
}

func compile(p Params) (*compiled, error) {
	if math.IsNaN(p.Rate) || math.IsInf(p.Rate, 0) || p.Rate <= 0 || p.Rate > 1e9 {
		return nil, fmt.Errorf("%w: rate %v not in (0, 1e9]", ErrInvalidParams, p.Rate)
	}
	if p.Burst < 1 || p.Burst > MaxBurst {
		return nil, fmt.Errorf("%w: burst %d not in [1, %d]", ErrInvalidParams, p.Burst, MaxBurst)
	}
	tf := math.Ceil(1e9 / p.Rate)
	if tf*float64(p.Burst) > float64(maxSpan) {
		return nil, fmt.Errorf("%w: burst/rate span too long", ErrInvalidParams)
	}
	c := &compiled{p: p, burst: int64(p.Burst), t: int64(tf), rate: p.Rate, capUnits: int64(p.Burst) * unit}
	c.tau = (c.burst - 1) * c.t
	switch {
	case p.Window < 0 || int64(p.Window) > maxSpan:
		return nil, fmt.Errorf("%w: window %v out of range", ErrInvalidParams, p.Window)
	case p.Window == 0:
		c.window = c.burst * c.t
	default:
		c.window = int64(p.Window)
	}
	return c, nil
}

// Rule binds a key pattern to parameters.
//
// Patterns: "*" matches every key (the default); "prefix*" matches keys
// starting with prefix; anything else matches one key exactly. An exact
// match beats any prefix, and the longest prefix wins.
type Rule struct {
	Pattern string
	Params  Params
}

// Rules is a concurrency-safe table of key patterns. Lookups are lock-free;
// Configure copies the table, so it is meant for configuration changes,
// not per-request use.
type Rules struct {
	mu  sync.Mutex
	cur atomic.Pointer[ruleSet]
}

type prefixRule struct {
	prefix string
	c      *compiled
}

type ruleSet struct {
	exact    map[string]*compiled
	prefixes []prefixRule // longest first
	def      *compiled
}

// NewRules builds a table from rules, applied in order.
func NewRules(rules ...Rule) (*Rules, error) {
	r := &Rules{}
	r.cur.Store(&ruleSet{exact: map[string]*compiled{}})
	for _, rule := range rules {
		if err := r.Configure(rule.Pattern, rule.Params); err != nil {
			return nil, err
		}
	}
	return r, nil
}

// Configure sets the parameters for keys matching pattern, replacing any
// earlier rule with the same pattern. Existing per-key state is kept and
// evaluated under the new parameters from the next request on.
func (r *Rules) Configure(pattern string, p Params) error {
	c, err := compile(p)
	if err != nil {
		return err
	}
	prefix, isPrefix := strings.CutSuffix(pattern, "*")
	if pattern == "" || strings.Contains(prefix, "*") {
		return fmt.Errorf("%w: %q", ErrInvalidPattern, pattern)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	old := r.cur.Load()
	if old == nil { // zero Rules
		old = &ruleSet{}
	}
	next := &ruleSet{exact: make(map[string]*compiled, len(old.exact)+1), prefixes: slices.Clone(old.prefixes), def: old.def}
	for k, v := range old.exact {
		next.exact[k] = v
	}
	switch {
	case pattern == "*":
		next.def = c
	case isPrefix:
		i := slices.IndexFunc(next.prefixes, func(pr prefixRule) bool { return pr.prefix == prefix })
		if i >= 0 {
			next.prefixes[i].c = c
		} else {
			next.prefixes = append(next.prefixes, prefixRule{prefix, c})
			slices.SortStableFunc(next.prefixes, func(a, b prefixRule) int { return len(b.prefix) - len(a.prefix) })
		}
	default:
		next.exact[pattern] = c
	}
	r.cur.Store(next)
	return nil
}

// Lookup returns the parameters that apply to key.
func (r *Rules) Lookup(key string) (Params, error) {
	c, err := r.lookup(key)
	if err != nil {
		return Params{}, err
	}
	return c.p, nil
}

func (r *Rules) lookup(key string) (*compiled, error) {
	rs := r.cur.Load()
	if rs == nil {
		return nil, ErrNoRule
	}
	if c, ok := rs.exact[key]; ok {
		return c, nil
	}
	for _, pr := range rs.prefixes {
		if strings.HasPrefix(key, pr.prefix) {
			return pr.c, nil
		}
	}
	if rs.def != nil {
		return rs.def, nil
	}
	return nil, ErrNoRule
}

// prepare validates one request and returns its compiled parameters.
func (r *Rules) prepare(key string, cost int, now time.Duration) (*compiled, error) {
	if key == "" {
		return nil, ErrInvalidKey
	}
	if now > MaxTime || now < -MaxTime {
		return nil, ErrInvalidTime
	}
	c, err := r.lookup(key)
	if err != nil {
		return nil, err
	}
	if cost < 1 {
		return nil, ErrInvalidCost
	}
	if int64(cost) > c.burst {
		return c, ErrCostExceedsCapacity
	}
	return c, nil
}

// prepared is one validated, de-duplicated request of an AcquireAll.
type prepared struct {
	key  string
	cost int64
	c    *compiled
}

// prepareAll validates reqs and merges duplicate keys by summing costs.
// buf is reused when large enough so small calls do not allocate.
func (r *Rules) prepareAll(reqs []Request, now time.Duration, buf []prepared) ([]prepared, Result, error) {
	if len(reqs) == 0 {
		return nil, Result{}, ErrEmptyRequest
	}
	out := buf[:0]
next:
	for _, q := range reqs {
		if q.Cost < 1 {
			return nil, Result{}, fmt.Errorf("%w: key %q", ErrInvalidCost, q.Key)
		}
		for i := range out {
			if out[i].key == q.Key {
				out[i].cost += int64(q.Cost)
				if out[i].cost > out[i].c.burst {
					return nil, cannot(out[i].c.burst), fmt.Errorf("%w: key %q", ErrCostExceedsCapacity, q.Key)
				}
				continue next
			}
		}
		c, err := r.prepare(q.Key, q.Cost, now)
		if err != nil {
			if c != nil {
				return nil, cannot(c.burst), fmt.Errorf("%w: key %q", err, q.Key)
			}
			return nil, Result{}, fmt.Errorf("%w: key %q", err, q.Key)
		}
		out = append(out, prepared{key: q.Key, cost: int64(q.Cost), c: c})
	}
	return out, Result{}, nil
}
