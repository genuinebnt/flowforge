package gateway

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	graphv1 "github.com/genuinebnt/flowforge/gen/flowforge/graph/v1"
	similarityv1 "github.com/genuinebnt/flowforge/gen/flowforge/similarity/v1"
	flowforgev1 "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
)

// Backends are the generated gRPC clients of the internal services. Their
// connections must carry the interceptors from DialOptions, which attach
// the internal token and the spec 11.1 metadata.
type Backends struct {
	Scope      flowforgev1.ScopeServiceClient
	Engine     flowforgev1.EngineServiceClient
	Graph      flowforgev1.GraphServiceClient
	Notifier   flowforgev1.NotifierServiceClient
	ToolRunner flowforgev1.ToolRunnerServiceClient
	Analytics  graphv1.GraphAnalyticsClient
	Similarity similarityv1.SimilarityClient
	// Schedules is the engine's schedule API (nil: /v1/schedules answers
	// 503).
	Schedules Schedules
}

// CookieConfig controls the console cookies (spec 13.5).
type CookieConfig struct {
	Domain string
	// Insecure drops the Secure attribute (local development over HTTP).
	Insecure bool
}

// Route classes for API rate limits (rl:api:<tenant>:<class>).
const (
	ClassAuth   = "auth"   // public identity endpoints (keyed by client IP)
	ClassRead   = "read"   // GET
	ClassWrite  = "write"  // POST, PUT, PATCH, DELETE
	ClassStream = "stream" // SSE watches
)

// PlanLimits maps plan name -> route class -> GCRA parameters. The plan
// "anonymous" applies to unauthenticated requests (keyed by client IP).
type PlanLimits map[string]map[string]limiter.Params

// DefaultPlanLimits returns conservative defaults.
func DefaultPlanLimits() PlanLimits {
	std := map[string]limiter.Params{
		ClassRead:   {Rate: 20, Burst: 100},
		ClassWrite:  {Rate: 5, Burst: 30},
		ClassStream: {Rate: 1, Burst: 10},
		ClassAuth:   {Rate: 5, Burst: 30},
	}
	return PlanLimits{
		"free":      std,
		"pro":       {ClassRead: {Rate: 100, Burst: 500}, ClassWrite: {Rate: 25, Burst: 100}, ClassStream: {Rate: 5, Burst: 50}, ClassAuth: {Rate: 10, Burst: 50}},
		"anonymous": {ClassAuth: {Rate: 5, Burst: 30}, ClassRead: {Rate: 10, Burst: 50}, ClassWrite: {Rate: 5, Burst: 30}, ClassStream: {Rate: 1, Burst: 5}},
	}
}

// Config configures a Server.
type Config struct {
	Identity    *identity.Service
	Revocations identity.Revocations // access-token revocation (deny:jti, sv)
	Limiter     *identity.RateLimiter
	Backends    Backends
	Clock       core.Clock
	IDs         core.Rng
	// Issuer is the iss of access tokens (FF_JWT_ISSUER).
	Issuer  string
	Cookies CookieConfig
	Plans   PlanLimits
	// BodyLimit caps request bodies (default 1 MiB).
	BodyLimit int64
	// KeepAlive is the SSE keep-alive comment interval (default 15 s).
	KeepAlive time.Duration
	// WriteTimeout bounds writing a non-streaming response (default 30 s).
	WriteTimeout time.Duration
	// TrustForwardedFor takes the client IP from the first X-Forwarded-For
	// entry (set behind a load balancer that overwrites it).
	TrustForwardedFor bool
	// Readiness checks run by /readyz.
	Readiness map[string]func(ctx context.Context) error
	Log       *slog.Logger
}

// Server is the public REST gateway (spec 12).
type Server struct {
	cfg      Config
	verifier *auth.Verifier
	handler  http.Handler
	ids      *lockedIDs
	plans    *planCache
}

// New returns a Server.
func New(cfg Config) (*Server, error) {
	if cfg.Identity == nil || cfg.Revocations == nil || cfg.Limiter == nil || cfg.Clock == nil || cfg.IDs == nil || cfg.Issuer == "" {
		return nil, fmt.Errorf("gateway: config needs identity, revocations, limiter, clock, ids and issuer")
	}
	b := cfg.Backends
	if b.Scope == nil || b.Engine == nil || b.Graph == nil || b.Notifier == nil || b.ToolRunner == nil || b.Analytics == nil || b.Similarity == nil {
		return nil, fmt.Errorf("gateway: every backend client is required")
	}
	if cfg.Plans == nil {
		cfg.Plans = DefaultPlanLimits()
	}
	if cfg.BodyLimit <= 0 {
		cfg.BodyLimit = 1 << 20
	}
	if cfg.KeepAlive <= 0 {
		cfg.KeepAlive = 15 * time.Second
	}
	if cfg.WriteTimeout <= 0 {
		cfg.WriteTimeout = 30 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	v, err := auth.NewVerifier(auth.VerifierConfig{
		Keys: cfg.Identity.Keys(), Clock: cfg.Clock, Issuer: cfg.Issuer, Audience: auth.AudienceAPI,
		Revocation: cfg.Revocations,
	})
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, verifier: v, ids: &lockedIDs{clock: cfg.Clock, rng: cfg.IDs}}
	s.plans = &planCache{ttl: time.Minute, clock: cfg.Clock, lookup: cfg.Identity.TenantPlan, m: map[core.UUID]planEntry{}}
	mux := http.NewServeMux()
	for _, rt := range s.routes() {
		rt := rt
		mux.Handle(rt.method+" "+rt.pattern, s.wrap(&rt))
	}
	mux.Handle("/", s.wrap(&route{method: "", pattern: "/", public: true, class: ClassAuth, h: func(c *call) error { return errNotFound() }}))
	s.handler = s.outer(mux)
	return s, nil
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler { return s.handler }

// HTTPServer returns an http.Server for addr with conservative timeouts.
// WriteTimeout is left unset because SSE streams are long-lived; each
// non-streaming response gets its own write deadline instead.
func (s *Server) HTTPServer(addr string) *http.Server {
	return &http.Server{
		Addr: addr, Handler: s.handler,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 120 * time.Second,
		MaxHeaderBytes: 64 << 10,
	}
}

type lockedIDs struct {
	mu    sync.Mutex
	clock core.Clock
	rng   core.Rng
}

func (l *lockedIDs) new() core.UUID {
	l.mu.Lock()
	defer l.mu.Unlock()
	return core.NewUUIDv7(l.clock, l.rng)
}

func (l *lockedIDs) hex(n int) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	const digits = "0123456789abcdef"
	b := make([]byte, n)
	var x uint64
	for i := range b {
		if i%16 == 0 {
			x = l.rng.Uint64()
		}
		b[i] = digits[x&0xf]
		x >>= 4
	}
	return string(b)
}

// planCache caches tenant plans for rate limiting.
type planCache struct {
	ttl    time.Duration
	clock  core.Clock
	lookup func(context.Context, core.UUID) string
	mu     sync.Mutex
	m      map[core.UUID]planEntry
}

type planEntry struct {
	plan string
	exp  time.Time
}

func (p *planCache) get(ctx context.Context, t core.UUID) string {
	now := p.clock.Now()
	p.mu.Lock()
	e, ok := p.m[t]
	p.mu.Unlock()
	if ok && now.Before(e.exp) {
		return e.plan
	}
	plan := p.lookup(ctx, t)
	p.mu.Lock()
	if len(p.m) > 100_000 {
		p.m = map[core.UUID]planEntry{}
	}
	p.m[t] = planEntry{plan: plan, exp: now.Add(p.ttl)}
	p.mu.Unlock()
	return plan
}
