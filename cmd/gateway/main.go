// Command gateway runs the Flowforge public REST gateway and identity
// service (spec 12, 13). Configuration comes from the environment
// (spec 17.5); see loadConfig.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
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

// config is the gateway's environment configuration.
type config struct {
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

var backendVars = []string{"FF_SCOPE_ADDR", "FF_ENGINE_ADDR", "FF_GRAPH_ADDR", "FF_NOTIFIER_ADDR", "FF_TOOLRUNNER_ADDR", "FF_SIMILARITY_ADDR"}

func boolEnv(v string) bool {
	b, _ := strconv.ParseBool(v)
	return b
}

// loadConfig reads the configuration through getenv.
func loadConfig(getenv func(string) string) (config, error) {
	c := config{
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
	for _, v := range backendVars {
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

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		log.Error("gateway: configuration", "err", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, log); err != nil {
		log.Error("gateway: exit", "err", err)
		os.Exit(1)
	}
}

// commonBreached is a tiny offline breached-password list; production
// should plug a k-anonymity or bloom-filter checker into auth.PasswordPolicy.
var commonBreached = []string{"password1234", "123456789012", "qwertyuiopas", "passwordpassword", "correct horse battery staple"}

func run(ctx context.Context, cfg config, log *slog.Logger) error {
	clock := &core.SystemClock{}
	ids := core.SystemRng{}
	ready := map[string]func(context.Context) error{}

	var store identity.Store
	if cfg.DBDSN != "" {
		pool, err := pgxpool.New(ctx, cfg.DBDSN)
		if err != nil {
			return fmt.Errorf("postgres: %w", err)
		}
		defer pool.Close()
		if cfg.Migrate {
			if err := migrations.Apply(ctx, pool); err != nil {
				return fmt.Errorf("migrate: %w", err)
			}
		}
		store = pgstore.New(pool)
	} else {
		log.Warn("gateway: FF_DB_DSN not set; identity data is in memory only")
		store = identity.NewMemStore()
	}

	var revocations identity.Revocations = identity.NewMemRevocations(clock)
	var lockouts auth.LockoutStore = auth.NewInMemoryLockoutStore()
	factory := identity.MemLimiterFactory
	if cfg.RedisAddr != "" {
		rc := redis.NewClient(&redis.Options{Addr: cfg.RedisAddr, Password: cfg.RedisAuth})
		defer rc.Close()
		revocations, lockouts = redisstore.NewRevocations(rc), redisstore.NewLockouts(rc)
		factory = func(p limiter.Params) (limiter.ContextLimiter, error) {
			return redislimiter.New(rc, redislimiter.Options{}, limiter.Rule{Pattern: "*", Params: p})
		}
		ready["redis"] = func(ctx context.Context) error { return rc.Ping(ctx).Err() }
	} else {
		log.Warn("gateway: FF_REDIS_ADDR not set; revocations, lockouts and rate limits are per process")
	}

	keys, err := identity.NewKeyManager(ctx, identity.KeyManagerConfig{
		Store: store, Secrets: identity.NewMemKeyStore(), Clock: clock, IDs: ids, Random: rand.Reader, Issuer: cfg.Issuer,
	})
	if err != nil {
		return err
	}
	hasher, err := auth.NewHasher(rand.Reader, auth.PolicyParams())
	if err != nil {
		return err
	}
	lim := identity.NewRateLimiter(factory, clock)
	svc, err := identity.New(identity.Config{
		Store: store, Keys: keys, Revocations: revocations, Limiter: lim, Lockouts: lockouts, Passwords: hasher,
		Policy: auth.PasswordPolicy{Breached: auth.NewBreachedSet(commonBreached...)}, Email: identity.LogEmailSender{Log: log},
		Clock: clock, IDs: ids, Random: rand.Reader, PublicURL: cfg.PublicURL, Log: log,
	})
	if err != nil {
		return err
	}

	creds := insecure.NewCredentials()
	if cfg.GRPCTLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conns := map[string]*grpc.ClientConn{}
	for _, v := range backendVars {
		cc, err := grpc.NewClient(cfg.Backends[v], append([]grpc.DialOption{grpc.WithTransportCredentials(creds)}, gateway.DialOptions(keys)...)...)
		if err != nil {
			return fmt.Errorf("%s: %w", v, err)
		}
		defer cc.Close()
		conns[v] = cc
		name := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(v, "FF_"), "_ADDR"))
		ready[name] = func(context.Context) error {
			cc.Connect()
			if s := cc.GetState(); s == connectivity.TransientFailure || s == connectivity.Shutdown {
				return fmt.Errorf("connection %s", s)
			}
			return nil
		}
	}
	backends := gateway.BackendsFromConns(conns["FF_SCOPE_ADDR"], conns["FF_ENGINE_ADDR"], conns["FF_GRAPH_ADDR"],
		conns["FF_NOTIFIER_ADDR"], conns["FF_TOOLRUNNER_ADDR"], conns["FF_SIMILARITY_ADDR"])

	srv, err := gateway.New(gateway.Config{
		Identity: svc, Revocations: revocations, Limiter: lim, Backends: backends, Clock: clock, IDs: ids,
		Issuer: cfg.Issuer, Cookies: gateway.CookieConfig{Domain: cfg.CookieDomain, Insecure: cfg.CookieInsec},
		TrustForwardedFor: cfg.TrustXFF, Readiness: ready, Log: log,
	})
	if err != nil {
		return err
	}

	go maintain(ctx, keys, store, clock, log)
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
