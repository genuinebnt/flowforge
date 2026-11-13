package redislimiter

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/limiter"
	"github.com/redis/go-redis/v9"
)

// Script is the GCRA script of spec 14.4, verbatim. KEYS are limiter keys;
// ARGV holds T_ms, tau_ms and cost for each key. It returns {1, 0} when
// every key allows (and then writes every new TAT) or {0, worst_ms} when
// any key denies (and then writes nothing).
//
// PORT: a Rust limiter sharing this Redis must send this exact text (the
// script SHA is derived from it) with the same ARGV layout and the same
// millisecond rounding as msParams (docs/PORTING_RUST.md 4.4). Redis Lua
// numbers are doubles and SET stores them as "%.14g" text, which is exact
// for millisecond TATs below 1e14 (about the year 5138).
const Script = `-- KEYS: one or more limiter keys; ARGV: per key: T_ms, tau_ms, cost  (GCRA, atomic across keys)
local now = redis.call('TIME'); now = now[1] * 1000 + math.floor(now[2] / 1000)
local worst = 0
local news = {}
for i = 1, #KEYS do
  local T, tau, cost = tonumber(ARGV[3*i-2]), tonumber(ARGV[3*i-1]), tonumber(ARGV[3*i])
  local tat = tonumber(redis.call('GET', KEYS[i]) or '0')
  local new_tat = math.max(tat, now) + T * cost
  local allow_at = new_tat - tau - T
  if now < allow_at then worst = math.max(worst, allow_at - now) end
  news[i] = new_tat
end
if worst > 0 then return { 0, worst } end          -- denied; nothing written
for i = 1, #KEYS do
  local T, tau = tonumber(ARGV[3*i-2]), tonumber(ARGV[3*i-1])
  redis.call('SET', KEYS[i], news[i], 'PX', math.ceil(news[i] - now + tau + T))
end
return { 1, 0 }
`

// timeLine is the first statement of Script, which reads Redis TIME.
const timeLine = "local now = redis.call('TIME'); now = now[1] * 1000 + math.floor(now[2] / 1000)"

// Options tunes a Limiter.
type Options struct {
	// CallerClock makes the script use the now argument of Acquire (in
	// whole milliseconds) instead of Redis TIME. It exists for tests and
	// simulations that drive a deterministic timeline; production keeps
	// the default so that all nodes share Redis's clock.
	CallerClock bool
}

// Limiter is GCRA over Redis. It implements limiter.ContextLimiter. Its
// resolution is one millisecond: T is rounded up to whole milliseconds, so
// it never admits more than the configured rate. Result.Remaining is not
// reported by the script and is always 0; Result.Limit is the smallest
// burst among the keys.
//
// With Redis Cluster every key of one AcquireAll must hash to the same
// slot (use a {hash tag}); the script is atomic only within one node.
type Limiter struct {
	client redis.Scripter
	rules  *limiter.Rules
	script *redis.Script
	caller bool
}

// New returns a Limiter using client and configured with rules.
func New(client redis.Scripter, opts Options, rules ...limiter.Rule) (*Limiter, error) {
	r, err := limiter.NewRules(rules...)
	if err != nil {
		return nil, err
	}
	src := Script
	if opts.CallerClock {
		// Take the clock from the last ARGV element (milliseconds). Only
		// the clock line changes, so tests compare the production script
		// against an exact timeline (lim_010; keep the same trick in Rust).
		src = strings.Replace(Script, timeLine, "local now = tonumber(ARGV[#ARGV])", 1)
	}
	return &Limiter{client: client, rules: r, script: redis.NewScript(src), caller: opts.CallerClock}, nil
}

// Configure sets the parameters for keys matching pattern.
func (l *Limiter) Configure(pattern string, p limiter.Params) error {
	return l.rules.Configure(pattern, p)
}

// Acquire implements limiter.ContextLimiter. now is ignored unless
// Options.CallerClock is set.
func (l *Limiter) Acquire(ctx context.Context, key string, cost int, now time.Duration) (limiter.Result, error) {
	return l.AcquireAll(ctx, []limiter.Request{{Key: key, Cost: cost}}, now)
}

// msParams returns T and tau in whole milliseconds, T rounded up so the
// millisecond script never admits more than the configured rate; tau is
// (Burst-1) times the rounded T.
func msParams(p limiter.Params) (t, tau int64) {
	t = int64((p.EmissionInterval() + time.Millisecond - 1) / time.Millisecond)
	return t, int64(p.Burst-1) * t
}

// AcquireAll implements limiter.ContextLimiter: one script call charges
// every key or none.
func (l *Limiter) AcquireAll(ctx context.Context, reqs []limiter.Request, now time.Duration) (limiter.Result, error) {
	if len(reqs) == 0 {
		return limiter.Result{}, limiter.ErrEmptyRequest
	}
	type entry struct {
		key  string
		cost int
		p    limiter.Params
	}
	merged := make([]entry, 0, len(reqs))
next:
	for _, q := range reqs {
		if q.Key == "" {
			return limiter.Result{}, limiter.ErrInvalidKey
		}
		if q.Cost < 1 {
			return limiter.Result{}, fmt.Errorf("%w: key %q", limiter.ErrInvalidCost, q.Key)
		}
		for i := range merged {
			if merged[i].key == q.Key {
				merged[i].cost += q.Cost
				continue next
			}
		}
		p, err := l.rules.Lookup(q.Key)
		if err != nil {
			return limiter.Result{}, fmt.Errorf("%w: key %q", err, q.Key)
		}
		merged = append(merged, entry{q.Key, q.Cost, p})
	}
	keys := make([]string, 0, len(merged))
	args := make([]any, 0, 3*len(merged)+1)
	limit := 0
	for _, e := range merged {
		if e.cost > e.p.Burst {
			return limiter.Result{RetryAfter: limiter.Never, Limit: e.p.Burst},
				fmt.Errorf("%w: key %q", limiter.ErrCostExceedsCapacity, e.key)
		}
		t, tau := msParams(e.p)
		keys = append(keys, e.key)
		args = append(args, t, tau, e.cost)
		if limit == 0 || e.p.Burst < limit {
			limit = e.p.Burst
		}
	}
	if l.caller {
		args = append(args, int64(now/time.Millisecond))
	}
	vals, err := l.script.Run(ctx, l.client, keys, args...).Int64Slice()
	if err != nil {
		return limiter.Result{}, fmt.Errorf("%w: %w", limiter.ErrUnavailable, err)
	}
	if len(vals) != 2 {
		return limiter.Result{}, fmt.Errorf("%w: unexpected script reply %v", limiter.ErrUnavailable, vals)
	}
	if vals[0] == 1 {
		return limiter.Result{Allowed: true, Limit: limit}, nil
	}
	return limiter.Result{RetryAfter: time.Duration(vals[1]) * time.Millisecond, Limit: limit}, nil
}
