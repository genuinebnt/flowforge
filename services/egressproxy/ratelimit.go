package egressproxy

import (
	"context"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core/netnorm"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/scope"
)

// DefaultRate applies to programs whose scope document sets no rate.
var DefaultRate = scope.Rate{RequestsPerSecond: 10, Burst: 20, Concurrency: 8}

// hostLimits is the per-target limit at the network edge (V2_DESIGN §9,
// spec 14.3): GCRA per rl:h:<program>:<registrable domain or ip> at the
// program's scope rate, plus a cap on concurrent requests and tunnels per
// key. Every HTTP request, CONNECT tunnel and SOCKS tunnel acquires it, so
// tools with internal parallelism cannot exceed the program's rate.
type hostLimits struct {
	mu         sync.Mutex
	configured map[string]limiter.Params // pattern -> params
	active     map[string]int
}

// limitTarget is the registrable domain of a name, or the IP itself.
func limitTarget(host string) string {
	if _, err := netip.ParseAddr(host); err == nil {
		return host
	}
	if rd := netnorm.RegistrableDomain(host); rd != "" {
		return rd
	}
	return host
}

func params(r scope.Rate) (limiter.Params, int) {
	if r.RequestsPerSecond <= 0 {
		r = DefaultRate
	}
	burst := r.Burst
	if burst <= 0 {
		burst = max(1, int(r.RequestsPerSecond))
	}
	conc := r.Concurrency
	if conc <= 0 {
		conc = DefaultRate.Concurrency
	}
	return limiter.Params{Rate: r.RequestsPerSecond, Burst: burst}, conc
}

// admit charges one request against the target's key. It returns a
// release function (to call when the request or tunnel ends) or a 429
// refusal: rate_limited with the retry delay, or concurrency_limited.
func (p *Proxy) admit(ctx context.Context, rq *request) (func(), *refusal) {
	var rate scope.Rate
	if rq.intel != nil && rq.intel.Rate.RequestsPerSecond > 0 {
		rate = rq.intel.Rate
	} else {
		pol, err := p.Scope.Policy(ctx, rq.id.Tenant, rq.rec.Program, rq.rec.ScopeVersion)
		if err != nil {
			return nil, refuse("policy_unavailable", "cannot read the program rate")
		}
		rate = pol.Document().Activity.Rate
	}
	prm, conc := params(rate)
	program := rq.rec.Program.String()
	key := limiter.HostKey(program, limitTarget(rq.host))
	pattern := limiter.HostKey(program, "") + "*"
	if rq.intel != nil && rq.intel.Rate.RequestsPerSecond > 0 {
		pattern = key
	}
	l := &p.limits
	l.mu.Lock()
	if l.configured == nil {
		l.configured, l.active = map[string]limiter.Params{}, map[string]int{}
	}
	if old, ok := l.configured[pattern]; !ok || old != prm {
		if err := p.Limiter.Configure(pattern, prm); err != nil {
			l.mu.Unlock()
			return nil, refuse("rate_invalid", err.Error())
		}
		l.configured[pattern] = prm
	}
	if l.active[key] >= conc {
		l.mu.Unlock()
		p.Metrics.LimiterDenied.Inc("proxy_host_concurrency")
		return nil, &refusal{reason: "concurrency_limited", detail: "too many concurrent connections to " + limitTarget(rq.host), status: http.StatusTooManyRequests}
	}
	res, err := p.Limiter.Acquire(key, 1, p.Clock.Monotonic())
	if err != nil || !res.Allowed {
		l.mu.Unlock()
		p.Metrics.LimiterDenied.Inc("proxy_host")
		rf := &refusal{reason: "rate_limited", detail: "rate limit for " + limitTarget(rq.host), status: http.StatusTooManyRequests,
			retryAfter: res.RetryAfter}
		if err != nil {
			rf.detail = err.Error()
		}
		return nil, rf
	}
	l.active[key]++
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			if l.active[key]--; l.active[key] <= 0 {
				delete(l.active, key)
			}
			l.mu.Unlock()
		})
	}, nil
}

// setRetry writes the retry hints of a rate-limit refusal.
func setRetry(h http.Header, d time.Duration) {
	if d <= 0 || d == limiter.Never {
		return
	}
	h.Set("Retry-After", strconv.FormatInt(int64((d+time.Second-1)/time.Second), 10))
	h.Set(HeaderRetryAfterMs, strconv.FormatInt(max(1, d.Milliseconds()), 10))
}
