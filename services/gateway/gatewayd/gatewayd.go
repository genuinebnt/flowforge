// Package gatewayd assembles a runnable gateway process from configuration:
// identity store (memory or Postgres), revocations, lockouts and limits
// (process-local or Redis), signing keys, backend connections and the HTTP
// server. cmd/gateway and cmd/flowforge dev both use it.
package gatewayd

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/limiter/redislimiter"
	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/services/gateway"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
	"github.com/genuinebnt/flowforge/services/gateway/identity/pgstore"
	"github.com/genuinebnt/flowforge/services/gateway/identity/redisstore"
)

// Config is the gateway's environment configuration (spec 17.5).
type Config struct {
	Addr          string
	DBDSN         string // FF_DB_DSN; empty = in-memory identity store
	Migrate       bool   // FF_DB_MIGRATE: apply migrations at start (dev)
	RedisAddr     string // FF_REDIS_ADDR; empty = in-process revocation, lockouts and limits
	RedisAuth     string // FF_REDIS_AUTH
	Issuer        string // FF_JWT_ISSUER
	Audience      string // FF_JWT_AUDIENCE (must be flowforge-api)
	PublicURL     string // FF_PUBLIC_URL: console base URL for e-mail links
	CookieDomain  string // FF_COOKIE_DOMAIN
	CookieInsec   bool   // FF_COOKIE_INSECURE: drop Secure (local HTTP only)
	TrustXFF      bool   // FF_TRUST_XFF: client IP from X-Forwarded-For
	GRPCTLS       bool   // FF_GRPC_TLS: TLS (system roots) to internal services
	Backends      map[string]string
	ShutdownGrace time.Duration
}

// BackendVars are the environment variables naming the internal services.
var BackendVars = []string{"FF_SCOPE_ADDR", "FF_ENGINE_ADDR", "FF_GRAPH_ADDR", "FF_NOTIFIER_ADDR", "FF_TOOLRUNNER_ADDR", "FF_SIMILARITY_ADDR"}

func boolEnv(v string) bool {
	b, _ := strconv.ParseBool(v)
	return b
}

// LoadConfig reads the configuration through getenv.
func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		Addr: getenv("FF_GATEWAY_ADDR"), DBDSN: getenv("FF_DB_DSN"), Migrate: boolEnv(getenv("FF_DB_MIGRATE")),
		RedisAddr: getenv("FF_REDIS_ADDR"), RedisAuth: getenv("FF_REDIS_AUTH"),
		Issuer: getenv("FF_JWT_ISSUER"), Audience: getenv("FF_JWT_AUDIENCE"), PublicURL: getenv("FF_PUBLIC_URL"),
		CookieDomain: getenv("FF_COOKIE_DOMAIN"), CookieInsec: boolEnv(getenv("FF_COOKIE_INSECURE")),
		TrustXFF: boolEnv(getenv("FF_TRUST_XFF")), GRPCTLS: boolEnv(getenv("FF_GRPC_TLS")),
		Backends: map[string]string{}, ShutdownGrace: 15 * time.Second,
	}
	if c.Addr == "" {
		if p := getenv("PORT"); p != "" {
			c.Addr = ":" + p
		} else {
			c.Addr = ":8080"
		}
	}
	if c.Issuer == "" {
		return c, errors.New("FF_JWT_ISSUER is required")
	}
	if c.Audience != "" && c.Audience != auth.AudienceAPI {
		return c, fmt.Errorf("FF_JWT_AUDIENCE must be %q (got %q)", auth.AudienceAPI, c.Audience)
	}
	if c.PublicURL == "" {
		c.PublicURL = strings.TrimSuffix(c.Issuer, "/")
	}
	var missing []string
	for _, v := range BackendVars {
		addr := getenv(v)
		if addr == "" {
			missing = append(missing, v)
		}
		c.Backends[v] = addr
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing internal service addresses: %s", strings.Join(missing, ", "))
	}
	return c, nil
}

// commonBreached is a tiny offline breached-password list; production
// should plug a k-anonymity or bloom-filter checker into auth.PasswordPolicy.
var commonBreached = []string{"password1234", "123456789012", "qwertyuiopas", "passwordpassword", "correct horse battery staple"}

// Gateway is an assembled gateway whose identity side (stores, signing
// keys) is ready. Call Serve to connect the backends and listen.
type Gateway struct {
	cfg         Config
	log         *slog.Logger
	clock       core.Clock
	store       identity.Store
	keys        *identity.KeyManager
	svc         *identity.Service
	lim         *identity.RateLimiter
	revocations identity.Revocations
	ready       map[string]func(context.Context) error
	closers     []func()
}

// New builds the identity side of the gateway from cfg. Backends are not
// dialled yet, so a caller can start the internal services with Keys
// trusted first (as cmd/flowforge dev does).
func New(ctx context.Context, cfg Config, log *slog.Logger) (*Gateway, error) {
	g := &Gateway{cfg: cfg, log: log, clock: &core.SystemClock{}, ready: map[string]func(context.Context) error{}}
	ids := core.SystemRng{}
	if cfg.DBDSN != "" {
		pool, err := pgxpool.New(ctx, cfg.DBDSN)
		if err != nil {
			return nil, fmt.Errorf("postgres: %w", err)
		}
		g.closers = append(g.closers, pool.Close)
		if cfg.Migrate {
			if err := migrations.Apply(ctx, pool); err != nil {
				g.Close()
				return nil, fmt.Errorf("migrate: %w", err)
			}
		}
		g.store = pgstore.New(pool)
	} else {
		log.Warn("gateway: FF_DB_DSN not set; identity data is in memory only")
		g.store = identity.NewMemStore()
	}

	g.revocations = identity.NewMemRevocations(g.clock)
	var lockouts auth.LockoutStore = auth.NewInMemoryLockoutStore()
	factory := identity.MemLimiterFactory
	if cfg.RedisAddr != "" {
		rc := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisAuth})
		g.closers = append(g.closers, func() { _ = rc.Close() })
		g.revocations, lockouts = redisstore.NewRevocations(rc), redisstore.NewLockouts(rc)
		factory = func(p limiter.Params) (limiter.ContextLimiter, error) {
			return redislimiter.New(rc, redislimiter.Options{}, limiter.Rule{Pattern: "*", Params: p})
		}
		g.ready["redis"] = func(ctx context.Context) error { return rc.Ping(ctx).Err() }
	} else {
		log.Warn("gateway: FF_REDIS_ADDR not set; revocations, lockouts and rate limits are per process")
	}

	keys, err := identity.NewKeyManager(ctx, identity.KeyManagerConfig{
		Store: g.store, Secrets: identity.NewMemKeyStore(), Clock: g.clock, IDs: ids, Random: rand.Reader, Issuer: cfg.Issuer,
	})
	if err != nil {
		g.Close()
		return nil, err
	}
	g.keys = keys
	hasher, err := auth.NewHasher(rand.Reader, auth.PolicyParams())
	if err != nil {
		g.Close()
		return nil, err
	}
	g.lim = identity.NewRateLimiter(factory, g.clock)
	g.svc, err = identity.New(identity.Config{
		Store: g.store, Keys: keys, Revocations: g.revocations, Limiter: g.lim, Lockouts: lockouts, Passwords: hasher,
		Policy: auth.PasswordPolicy{Breached: auth.NewBreachedSet(commonBreached...)}, Email: identity.LogEmailSender{Log: log},
		Clock: g.clock, IDs: ids, Random: rand.Reader, PublicURL: cfg.PublicURL, Log: log,
	})
	if err != nil {
		g.Close()
		return nil, err
	}
	return g, nil
}

// Keys returns the signing-key manager. Internal services must trust it
// (it signs the internal tokens the gateway sends them).
func (g *Gateway) Keys() *identity.KeyManager { return g.keys }

// Close releases the database and Redis clients.
func (g *Gateway) Close() {
	for i := len(g.closers) - 1; i >= 0; i-- {
		g.closers[i]()
	}
	g.closers = nil
}

// Serve dials the backends named in backends (keyed by BackendVars),
// serves HTTP on cfg.Addr until ctx is cancelled, then shuts down
// gracefully.
func (g *Gateway) Serve(ctx context.Context, backends map[string]string) error {
	cfg, log := g.cfg, g.log
	creds := insecure.NewCredentials()
	if cfg.GRPCTLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conns := map[string]*grpc.ClientConn{}
	for _, v := range BackendVars {
		cc, err := grpc.NewClient(backends[v], append([]grpc.DialOption{grpc.WithTransportCredentials(creds)}, gateway.DialOptions(g.keys)...)...)
		if err != nil {
			return fmt.Errorf("%s: %w", v, err)
		}
		defer cc.Close()
		conns[v] = cc
		name := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(v, "FF_"), "_ADDR"))
		g.ready[name] = func(context.Context) error {
			cc.Connect()
			if s := cc.GetState(); s == connectivity.TransientFailure || s == connectivity.Shutdown {
				return fmt.Errorf("connection %s", s)
			}
			return nil
		}
	}
	be := gateway.BackendsFromConns(conns["FF_SCOPE_ADDR"], conns["FF_ENGINE_ADDR"], conns["FF_GRAPH_ADDR"],
		conns["FF_NOTIFIER_ADDR"], conns["FF_TOOLRUNNER_ADDR"], conns["FF_SIMILARITY_ADDR"])

	srv, err := gateway.New(gateway.Config{
		Identity: g.svc, Revocations: g.revocations, Limiter: g.lim, Backends: be, Clock: g.clock, IDs: core.SystemRng{},
		Issuer: cfg.Issuer, Cookies: gateway.CookieConfig{Domain: cfg.CookieDomain, Insecure: cfg.CookieInsec},
		TrustForwardedFor: cfg.TrustXFF, Readiness: g.ready, Log: log,
	})
	if err != nil {
		return err
	}

	go maintain(ctx, g.keys, g.store, g.clock, log)
	hs := srv.HTTPServer(cfg.Addr)
	errc := make(chan error, 1)
	go func() {
		log.Info("gateway: listening", "addr", cfg.Addr)
		errc <- hs.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownGrace)
	defer cancel()
	if err := hs.Shutdown(sctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// maintain rotates and retires signing keys and purges expired
// idempotency records.
func maintain(ctx context.Context, keys *identity.KeyManager, store identity.Store, clock core.Clock, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := keys.Maintain(ctx); err != nil {
				log.Warn("gateway: key maintenance", "err", err)
			}
			if n, err := store.IdemPurge(ctx, clock.Now()); err != nil {
				log.Warn("gateway: idempotency purge", "err", err)
			} else if n > 0 {
				log.Info("gateway: idempotency purge", "deleted", n)
			}
		}
	}
}
