package gateway_test

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
)

// switchLimiter is an in-process limiter that fails every call while down
// is set (a Redis outage).
type switchLimiter struct {
	inner limiter.ContextLimiter
	down  *atomic.Bool
}

var errLimiterDown = errors.New("test: limiter store unreachable")

func (s switchLimiter) Acquire(ctx context.Context, key string, cost int, now time.Duration) (limiter.Result, error) {
	if s.down.Load() {
		return limiter.Result{}, errLimiterDown
	}
	return s.inner.Acquire(ctx, key, cost, now)
}

func (s switchLimiter) AcquireAll(ctx context.Context, reqs []limiter.Request, now time.Duration) (limiter.Result, error) {
	if s.down.Load() {
		return limiter.Result{}, errLimiterDown
	}
	return s.inner.AcquireAll(ctx, reqs, now)
}

func switchFactory(down *atomic.Bool) identity.LimiterFactory {
	return func(p limiter.Params) (limiter.ContextLimiter, error) {
		in, err := identity.MemLimiterFactory(p)
		return switchLimiter{inner: in, down: down}, err
	}
}

// TestRegress_G_LIM_01_LimiterFailsClosed: when the rate limiter errors,
// API requests are denied with 503 unavailable and Retry-After (spec 2.4,
// 11.8) and never reach a backend; the auth throttles deny too.
func TestRegress_G_LIM_01_LimiterFailsClosed(t *testing.T) {
	var down atomic.Bool
	h := newHarness(t, harnessOpts{limiterFactory: switchFactory(&down)})
	if code, _, _ := h.doJSON(request{method: http.MethodGet, path: "/v1/programs", auth: "t1:admin"}); code != http.StatusOK {
		t.Fatalf("limiter up: %d", code)
	}
	down.Store(true)
	calls := len(h.be.Calls())
	code, body, hdr := h.doJSON(request{method: http.MethodGet, path: "/v1/programs", auth: "t1:admin"})
	if code != http.StatusServiceUnavailable || errCode(body) != "unavailable" || hdr.Get("Retry-After") == "" {
		t.Fatalf("limiter down: %d %v %v", code, body, hdr)
	}
	if len(h.be.Calls()) != calls {
		t.Fatal("a request reached a backend while the limiter was down")
	}
	// Public auth endpoints: the API limit (keyed by IP) and the identity
	// throttles both fail closed.
	for _, r := range []request{
		{method: http.MethodPost, path: "/v1/auth/signup", body: map[string]any{"email": "x@example.com", "password": "correct horse battery staple!", "tenant_name": "x"}},
		{method: http.MethodPost, path: "/v1/auth/login", body: map[string]any{"email": "x@example.com", "password": "correct horse battery staple!"}},
	} {
		if code, body, _ := h.doJSON(r); code != http.StatusServiceUnavailable {
			t.Fatalf("%s with limiter down: %d %v", r.path, code, body)
		}
	}
}
