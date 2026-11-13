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
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/limiter/redislimiter"
	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/services/gateway"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
	"github.com/genuinebnt/flowforge/services/gateway/identity/keystore"
	"github.com/genuinebnt/flowforge/services/gateway/identity/pgstore"
	"github.com/genuinebnt/flowforge/services/gateway/identity/redisstore"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/bus/busconfig"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/platform/gcpauth"
	"github.com/genuinebnt/flowforge/services/internal/platform/redisconf"
	"github.com/genuinebnt/flowforge/services/internal/platform/smsecrets"
)

// Config is the gateway's environment configuration (spec 17.5).
type Config struct {
	Addr         string
	DBDSN        string // FF_DB_DSN; empty = in-memory identity store
	Migrate      bool   // FF_DB_MIGRATE: apply migrations at start (dev)
	RedisAddr    string // FF_REDIS_ADDR; empty = in-process revocation, lockouts and limits
	RedisAuth    string // FF_REDIS_AUTH
	RedisCA      string // FF_REDIS_CA: PEM (or a PEM file path) of the server CA; set = TLS (G-OPS-13)
	Issuer       string // FF_JWT_ISSUER
	Audience     string // FF_JWT_AUDIENCE (must be flowforge-api)
	PublicURL    string // FF_PUBLIC_URL: console base URL for e-mail links
	CookieDomain string // FF_COOKIE_DOMAIN
	CookieInsec  bool   // FF_COOKIE_INSECURE: drop Secure (local HTTP only)
	TrustXFF     bool   // FF_TRUST_XFF: client IP from X-Forwarded-For
	GRPCTLS      bool   // FF_GRPC_TLS: TLS (system roots) to host:port internal services; https:// addresses always use TLS
	// IDTokens (FF_ID_TOKEN_AUDIENCE_AUTO, or K_SERVICE on Cloud Run) adds
	// a Google ID token for each https:// backend to its calls (spec 17.3;
	// G-SVC-13). IDTokenSource and BackendTLS override the token source and
	// the TLS configuration (tests).
	IDTokens      bool
	IDTokenSource platform.IDTokenSource
	BackendTLS    *tls.Config
	Backends      map[string]string
	ShutdownGrace time.Duration

	// Env is FF_ENV (dev or prod). prod refuses the in-memory key store.
	Env string
	// Signing keys (spec 13.7; G-SVC-03, G-OPS-12). KeyStore is
	// FF_SIGNING_KEY_STORE: "env" (FF_SIGNING_KID + FF_SIGNING_KEY_<KID>,
	// the Terraform layout), "sm" (FF_SIGNING_KID read from Secret Manager
	// secret ff-signing-key-<kid> in SecretProject), "sm-managed" (the
	// gateway generates and rotates keys as Secret Manager secrets), "dir"
	// (key files in FF_SIGNING_KEY_DIR, generated and rotated, shared by
	// local processes) or "memory" (dev: a new key every restart). Empty
	// picks env when FF_SIGNING_KID is set, dir when FF_SIGNING_KEY_DIR
	// is, else memory.
	KeyStore      string
	SigningKID    string // FF_SIGNING_KID
	KeyDir        string // FF_SIGNING_KEY_DIR
	SecretProject string // FF_SECRET_PROJECT, else FF_PUBSUB_PROJECT
	// Getenv reads FF_SIGNING_KEY_<KID> for the env key store (os.Getenv
	// in cmd/gateway; tests pass a map).
	Getenv func(string) string

	// Argon2 is the password-hash parameters (spec 13.1): FF_ARGON2_MEMORY_KIB,
	// FF_ARGON2_TIME, FF_ARGON2_PARALLELISM, each defaulting to the policy
	// floor and never below it. CalibrateTarget (FF_ARGON2_TARGET_MS, 0 =
	// off) raises the iterations at start until one hash takes that long
	// (clamped to 200-500 ms) and logs the result. Hashes made with other
	// parameters are re-hashed at the next successful login (G-LIM-03).
	Argon2          auth.Params
	CalibrateTarget time.Duration

	// Bus selects the event bus the identity outbox (auth.events) is
	// relayed to (G-SVC-02): FF_BUS, FF_PUBSUB_PROJECT, FF_TOPIC_PREFIX and
	// FF_PUBSUB_CREATE, resolved like the internal services
	// (busconfig.Resolve). UseBus overrides it (flowforge dev shares the
	// dev stack's in-process bus).
	BusKind, PubSubProject, TopicPrefix string
	PubSubCreate                        bool
}

// BackendVars are the environment variables naming the internal services.
var BackendVars = []string{"FF_SCOPE_ADDR", "FF_ENGINE_ADDR", "FF_GRAPH_ADDR", "FF_NOTIFIER_ADDR", "FF_TOOL_RUNNER_ADDR", "FF_SIMILARITY_ADDR"}

func boolEnv(v string) bool {
	b, _ := strconv.ParseBool(v)
	return b
}

// LoadConfig reads the configuration through getenv.
func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{
		Addr: getenv("FF_GATEWAY_ADDR"), DBDSN: getenv("FF_DB_DSN"), Migrate: boolEnv(getenv("FF_DB_MIGRATE")),
		RedisAddr: getenv("FF_REDIS_ADDR"), RedisAuth: getenv("FF_REDIS_AUTH"), RedisCA: getenv("FF_REDIS_CA"),
		Issuer: getenv("FF_JWT_ISSUER"), Audience: getenv("FF_JWT_AUDIENCE"), PublicURL: getenv("FF_PUBLIC_URL"),
		CookieDomain: getenv("FF_COOKIE_DOMAIN"), CookieInsec: boolEnv(getenv("FF_COOKIE_INSECURE")),
		TrustXFF: boolEnv(getenv("FF_TRUST_XFF")), GRPCTLS: boolEnv(getenv("FF_GRPC_TLS")), IDTokens: platform.IDTokensFromEnv(getenv),
		Backends: map[string]string{}, ShutdownGrace: 15 * time.Second,
		BusKind: getenv("FF_BUS"), PubSubProject: getenv("FF_PUBSUB_PROJECT"), TopicPrefix: getenv("FF_TOPIC_PREFIX"),
		PubSubCreate: boolEnv(getenv("FF_PUBSUB_CREATE")),
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
	c.Env = getenv("FF_ENV")
	if c.Env == "" {
		c.Env = "dev"
	}
	c.KeyStore, c.SigningKID, c.KeyDir, c.Getenv = getenv("FF_SIGNING_KEY_STORE"), getenv("FF_SIGNING_KID"), getenv("FF_SIGNING_KEY_DIR"), getenv
	c.SecretProject = getenv("FF_SECRET_PROJECT")
	if c.SecretProject == "" {
		c.SecretProject = getenv("FF_PUBSUB_PROJECT")
	}
	floor := auth.PolicyParams()
	c.Argon2 = floor
	for _, f := range []struct {
		name string
		dst  *uint32
	}{{"FF_ARGON2_MEMORY_KIB", &c.Argon2.Memory}, {"FF_ARGON2_TIME", &c.Argon2.Time}} {
		if v := getenv(f.name); v != "" {
			n, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return c, fmt.Errorf("%s: %w", f.name, err)
			}
			*f.dst = uint32(n)
		}
	}
	if v := getenv("FF_ARGON2_PARALLELISM"); v != "" {
		n, err := strconv.ParseUint(v, 10, 8)
		if err != nil {
			return c, fmt.Errorf("FF_ARGON2_PARALLELISM: %w", err)
		}
		c.Argon2.Threads = uint8(n)
	}
	if !c.Argon2.MeetsPolicy() {
		return c, fmt.Errorf("argon2 parameters %+v are below the spec 13.1 floor %+v", c.Argon2, floor)
	}
	if v := getenv("FF_ARGON2_TARGET_MS"); v != "" {
		ms, err := strconv.Atoi(v)
		if err != nil || ms < 0 {
			return c, fmt.Errorf("FF_ARGON2_TARGET_MS: want milliseconds, got %q", v)
		}
		c.CalibrateTarget = time.Duration(ms) * time.Millisecond
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
	pool        *pgxpool.Pool
	bus         bus.Bus
	outbox      bus.OutboxSource
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
		g.store, g.pool = pgstore.New(pool), pool
		g.outbox = bus.PgOutbox{DB: pool, Table: "identity.outbox"}
	} else {
		log.Warn("gateway: FF_DB_DSN not set; identity data is in memory only")
		mem := identity.NewMemStore()
		g.store, g.outbox = mem, &memOutbox{store: mem}
	}

	g.revocations = identity.NewMemRevocations(g.clock)
	var lockouts auth.LockoutStore = auth.NewInMemoryLockoutStore()
	factory := identity.MemLimiterFactory
	if cfg.RedisAddr != "" {
		rc, err := redisconf.NewClient(redisconf.Config{Addr: cfg.RedisAddr, Auth: cfg.RedisAuth, CA: cfg.RedisCA})
		if err != nil {
			g.Close()
			return nil, err
		}
		g.closers = append(g.closers, func() { _ = rc.Close() })
		g.revocations, lockouts = redisstore.NewRevocations(rc), redisstore.NewLockouts(rc)
		factory = func(p limiter.Params) (limiter.ContextLimiter, error) {
			return redislimiter.New(rc, redislimiter.Options{}, limiter.Rule{Pattern: "*", Params: p})
		}
		g.ready["redis"] = func(ctx context.Context) error { return rc.Ping(ctx).Err() }
	} else {
		log.Warn("gateway: FF_REDIS_ADDR not set; revocations, lockouts and rate limits are per process")
	}

	ks, err := g.keyStore(cfg)
	if err != nil {
		g.Close()
		return nil, err
	}
	keys, err := identity.NewKeyManager(ctx, identity.KeyManagerConfig{
		Store: g.store, Secrets: ks, Clock: g.clock, IDs: ids, Random: rand.Reader, Issuer: cfg.Issuer,
	})
	if err != nil {
		g.Close()
		return nil, err
	}
	g.keys = keys
	params := cfg.Argon2
	if params == (auth.Params{}) {
		params = auth.PolicyParams()
	}
	if cfg.CalibrateTarget > 0 {
		p, took, err := auth.Calibrate(params, cfg.CalibrateTarget, auth.Measure(g.clock))
		if err != nil {
			g.Close()
			return nil, err
		}
		log.Info("gateway: argon2 calibrated", "memory_kib", p.Memory, "time", p.Time, "parallelism", p.Threads, "hash_ms", took.Milliseconds())
		params = p
	}
	hasher, err := auth.NewHasher(rand.Reader, params)
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

// keyStore selects the signing-key store (Config.KeyStore).
func (g *Gateway) keyStore(cfg Config) (identity.KeyStore, error) {
	getenv := cfg.Getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	kind := cfg.KeyStore
	if kind == "" {
		switch {
		case cfg.SigningKID != "":
			kind = "env"
		case cfg.KeyDir != "":
			kind = "dir"
		default:
			kind = "memory"
		}
	}
	sm := func() *smsecrets.Store {
		st := &smsecrets.Store{Project: cfg.SecretProject}
		g.closers = append(g.closers, func() { _ = st.Close() })
		return st
	}
	switch kind {
	case "env":
		return keystore.FromEnv(cfg.SigningKID, getenv)
	case "sm":
		return keystore.FromSecretManager(cfg.SigningKID, cfg.SecretProject, sm())
	case "sm-managed":
		if cfg.SecretProject == "" {
			return nil, errors.New("FF_SIGNING_KEY_STORE=sm-managed needs FF_SECRET_PROJECT")
		}
		return keystore.SecretManager{SM: sm()}, nil
	case "dir":
		if cfg.KeyDir == "" {
			return nil, errors.New("FF_SIGNING_KEY_STORE=dir needs FF_SIGNING_KEY_DIR")
		}
		return keystore.Dir{Path: cfg.KeyDir}, nil
	case "memory":
		if cfg.Env == "prod" {
			return nil, errors.New("the in-memory signing-key store is for dev only; set FF_SIGNING_KID and FF_SIGNING_KEY_<KID>")
		}
		g.log.Warn("gateway: signing keys are in memory; every restart rotates to a new key (set FF_SIGNING_KID or FF_SIGNING_KEY_DIR)")
		return identity.NewMemKeyStore(), nil
	}
	return nil, fmt.Errorf("FF_SIGNING_KEY_STORE %q: want env, sm, sm-managed, dir or memory", kind)
}

// UseBus sets the bus the identity outbox is relayed to, instead of the
// one the configuration selects. Call it before Serve.
func (g *Gateway) UseBus(b bus.Bus) { g.bus = b }

// openBus builds the configured bus unless UseBus set one.
func (g *Gateway) openBus(ctx context.Context) error {
	if g.bus != nil {
		return nil
	}
	b, closeFn, err := busconfig.Open(ctx, busconfig.Config{Kind: g.cfg.BusKind, Pool: g.pool, Project: g.cfg.PubSubProject,
		Prefix: g.cfg.TopicPrefix, CreateMissing: g.cfg.PubSubCreate, Logger: g.log})
	if err != nil {
		return err
	}
	g.bus = b
	g.closers = append(g.closers, closeFn)
	return nil
}

// memOutbox relays a MemStore's outbox: the store keeps rows in append
// order and never removes them, so an index is the published watermark.
type memOutbox struct {
	store *identity.MemStore
	mu    sync.Mutex
	next  int
}

// Claim implements bus.OutboxSource.
func (o *memOutbox) Claim(ctx context.Context, limit int, publish func(context.Context, []bus.OutboxRow) error) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	all := o.store.Outbox()
	if o.next >= len(all) {
		return 0, nil
	}
	end := min(len(all), o.next+limit)
	rows := make([]bus.OutboxRow, 0, end-o.next)
	for i, e := range all[o.next:end] {
		rows = append(rows, bus.OutboxRow{ID: int64(o.next + i + 1), EventID: e.EventID.String(), Topic: e.Topic,
			OrderingKey: e.OrderingKey, Payload: e.Payload, CreatedAt: e.CreatedAt})
	}
	if err := publish(ctx, rows); err != nil {
		return 0, err
	}
	o.next = end
	return len(rows), nil
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
	ids := cfg.IDTokenSource
	if ids == nil && cfg.IDTokens {
		ids = gcpauth.New()
	}
	conns := map[string]*grpc.ClientConn{}
	for _, v := range BackendVars {
		t, err := platform.ParseTarget(backends[v])
		if err != nil {
			return fmt.Errorf("%s: %w", v, err)
		}
		creds := t.TransportCredentials(cfg.BackendTLS)
		if cfg.GRPCTLS && !t.TLS {
			creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
		}
		opts := append([]grpc.DialOption{grpc.WithTransportCredentials(creds)}, gateway.DialOptions(g.keys)...)
		cc, err := grpc.NewClient(t.Addr, append(opts, platform.IDTokenDialOptions(t, ids)...)...)
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
		conns["FF_NOTIFIER_ADDR"], conns["FF_TOOL_RUNNER_ADDR"], conns["FF_SIMILARITY_ADDR"])
	// The engine serves its schedule API as HTTP on its gRPC port (h2c
	// locally, TLS on Cloud Run, where the ID token goes in
	// X-Serverless-Authorization as for gRPC).
	et, _ := platform.ParseTarget(backends["FF_ENGINE_ADDR"]) // checked in the loop above
	engineURL := "http://" + et.Addr
	if et.TLS || cfg.GRPCTLS {
		engineURL = "https://" + et.Addr
	}
	be.Schedules = gateway.HTTPSchedules{BaseURL: engineURL, Keys: g.keys, IDTokens: ids, Audience: et.Audience}

	srv, err := gateway.New(gateway.Config{
		Identity: g.svc, Revocations: g.revocations, Limiter: g.lim, Backends: be, Clock: g.clock, IDs: core.SystemRng{},
		Issuer: cfg.Issuer, Cookies: gateway.CookieConfig{Domain: cfg.CookieDomain, Insecure: cfg.CookieInsec},
		TrustForwardedFor: cfg.TrustXFF, Readiness: g.ready, Log: log,
	})
	if err != nil {
		return err
	}

	go maintain(ctx, g.keys, g.store, g.clock, log)
	if err := g.openBus(ctx); err != nil {
		return fmt.Errorf("bus: %w", err)
	}
	relay := &bus.Relay{Source: g.outbox, Bus: g.bus, Logger: log, Interval: 200 * time.Millisecond}
	relayDone := make(chan struct{})
	go func() { defer close(relayDone); _ = relay.Run(ctx) }()
	defer func() { <-relayDone }()
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
