// Package wire builds each internal service from an Env (logger, metrics,
// keys, optional Postgres pool, optional bus, a dialer) into a Component
// (gRPC registrations, extra HTTP routes, background loops) and serves it.
// Both the per-service mains in cmd/ and services/devstack use it, so a
// service is wired the same way standalone and in the dev stack.
package wire

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// Env is what every service is built from.
type Env struct {
	Service  string
	Logger   *slog.Logger
	Metrics  *platform.Metrics
	Clock    core.Clock
	Tokens   platform.TokenSource
	Verifier platform.TokenVerifier
	// DB is nil for the in-memory stores; DSN is its connection string
	// (LISTEN connections).
	DB  *pgxpool.Pool
	DSN string
	// Bus is nil when no event bus is available (standalone binaries: no
	// Pub/Sub adapter yet); outboxes then keep their rows unpublished.
	Bus bus.Bus
	// Blobs is the object store for raw tool output, snapshot files and
	// run archives (services/internal/blob); nil keeps raw output out of
	// storage and snapshots in memory.
	Blobs graphstore.BlobStore
}

// Dial opens a client connection with env's token source.
func (e *Env) Dial(target string) (*grpc.ClientConn, error) {
	if target == "" {
		return nil, errors.New("wire: empty service address")
	}
	return platform.Dial(target, platform.ClientOptions{Tokens: e.Tokens})
}

// Component is a built service.
type Component struct {
	// Register adds the gRPC services.
	Register func(*grpc.Server)
	// Routes are extra HTTP routes on the same port (pattern -> handler).
	Routes map[string]http.Handler
	// Handler, when set, replaces the gRPC/HTTP handler entirely (the
	// egress proxy is a plain HTTP proxy).
	Handler http.Handler
	// Run are background loops (workers, relays, consumers).
	Run []func(context.Context) error
	// Ready is an optional readiness check.
	Ready func(context.Context) error
	// Close releases resources after shutdown.
	Close func()
}

// Serve serves c on lis until ctx is done, running its background loops.
func Serve(ctx context.Context, env *Env, lis net.Listener, c *Component) error {
	health := platform.NewHealth()
	if c.Ready != nil {
		health.AddCheck(env.Service, c.Ready)
	}
	h := c.Handler
	if h == nil {
		g := platform.NewGRPCServer(platform.ServerOptions{Service: env.Service, Verifier: env.Verifier, Logger: env.Logger, Metrics: env.Metrics})
		platform.RegisterHealth(g, health)
		if c.Register != nil {
			c.Register(g)
		}
		mux := health.Mux(env.Metrics.Registry)
		for p, rh := range c.Routes {
			mux.Handle(p, rh)
		}
		h = platform.Handler(g, mux)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for _, run := range c.Run {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := run(ctx); err != nil && ctx.Err() == nil {
				env.Logger.ErrorContext(ctx, "service.loop_failed", "error", err.Error())
			}
		}()
	}
	env.Logger.InfoContext(ctx, "service.listening", "addr", lis.Addr().String())
	err := platform.Serve(ctx, lis, h, health, env.Logger)
	cancel()
	wg.Wait()
	if c.Close != nil {
		c.Close()
	}
	return err
}

type lockedRng struct {
	mu sync.Mutex
	r  core.Rng
}

func (l *lockedRng) Uint64() uint64 { l.mu.Lock(); defer l.mu.Unlock(); return l.r.Uint64() }
