// Package app is the entry point of the per-service binaries in cmd/:
// Main reads the environment (spec 17.5), builds the named service with
// services/internal/wire and serves it until SIGTERM. Only this package
// and cmd/ read the environment.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/wire"
	"github.com/genuinebnt/flowforge/services/notifier"
)

// Services are the names Main accepts (the cmd/ directory names).
var Services = []string{"scope", "engine", "tool-runner", "egress-proxy", "graph", "similarity", "notifier"}

// Main runs service and exits the process on failure.
func Main(service string) {
	if err := Run(context.Background(), service, os.Getenv); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", service, err)
		os.Exit(1)
	}
}

// Run builds and serves service with the configuration read through getenv
// until SIGTERM or SIGINT.
func Run(ctx context.Context, service string, getenv func(string) string) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()
	cfg, err := platform.ConfigFromEnv(service, getenv)
	if err != nil {
		return err
	}
	log := platform.NewLogger(os.Stdout, service, cfg.Env, platform.ParseLevel(cfg.LogLevel))
	clock := &core.SystemClock{}
	keys, err := platform.LoadKeys(ctx, cfg, clock)
	if err != nil {
		return err
	}
	verifier, err := keys.Verifier(clock)
	if err != nil {
		return err
	}
	tokens, err := keys.Tokens(clock)
	if err != nil {
		return err
	}
	if tokens == nil {
		log.Warn("config.no_signing_key", "detail", "FF_SIGNING_KEY unset: calls to other services will be rejected")
	}
	env := &wire.Env{Service: service, Logger: log, Metrics: platform.NewMetrics(nil), Clock: clock, Verifier: verifier, DSN: cfg.DBDSN}
	if tokens != nil {
		env.Tokens = tokens
	}
	if cfg.DBDSN != "" {
		pool, err := pgxpool.New(ctx, cfg.DBDSN)
		if err != nil {
			return err
		}
		defer pool.Close()
		env.DB = pool
	}
	if cfg.PubSubProject != "" {
		log.Warn("config.no_pubsub", "detail", "the Pub/Sub adapter is not implemented; events stay in the outboxes (see services/README.md)")
	}
	c, err := build(ctx, cfg, env, getenv)
	if err != nil {
		return err
	}
	lis, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	return wire.Serve(ctx, env, lis, c)
}

func build(ctx context.Context, cfg platform.Config, env *wire.Env, getenv func(string) string) (*wire.Component, error) {
	switch cfg.Service {
	case "scope":
		c, _ := wire.Scope(env, wire.ScopeOptions{})
		return c, nil
	case "engine":
		c, _, err := wire.Engine(ctx, env, wire.EngineOptions{ScopeAddr: cfg.ScopeAddr, ToolRunnerAddr: cfg.ToolRunnerAddr,
			GraphAddr: cfg.GraphAddr, RedisAddr: cfg.RedisAddr, RedisAuth: cfg.RedisAuth, Workers: cfg.Workers,
			Engine: engine.Config{FuelLimit: cfg.FuelDefault}})
		return c, err
	case "tool-runner":
		if cfg.EgressProxy == "" && cfg.Env == "prod" {
			return nil, errors.New("FF_EGRESS_PROXY is required in prod")
		}
		return wire.ToolRunner(env, wire.ToolRunnerOptions{ScopeAddr: cfg.ScopeAddr, EgressProxy: cfg.EgressProxy})
	case "egress-proxy":
		c, _, err := wire.EgressProxy(env, wire.EgressOptions{ScopeAddr: cfg.ScopeAddr})
		return c, err
	case "graph":
		c, _, err := wire.Graph(env, wire.GraphOptions{ScopeAddr: cfg.ScopeAddr})
		return c, err
	case "similarity":
		return wire.Similarity(env, wire.SimilarityOptions{GraphAddr: cfg.GraphAddr, ScopeAddr: cfg.ScopeAddr})
	case "notifier":
		c, _ := wire.Notifier(env, wire.NotifierOptions{Secrets: notifier.EnvSecrets{Getenv: getenv},
			Email: notifier.SMTPEmail{Secrets: notifier.EnvSecrets{Getenv: getenv}}})
		return c, nil
	}
	return nil, fmt.Errorf("unknown service %q", cfg.Service)
}

// Migrate applies the database migrations (cmd/migrate, the Cloud Run job).
func Migrate(ctx context.Context, getenv func(string) string) error {
	dsn := getenv("FF_DB_DSN")
	if dsn == "" {
		return errors.New("FF_DB_DSN is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	res, err := migrations.Deploy(ctx, pool)
	for _, r := range res {
		fmt.Printf("%s %s\n", r.Migration.File(), r.Status)
	}
	return err
}

// Logger returns the JSON logger of spec 18.1 writing to stderr.
func Logger(service, env, level string) *slog.Logger {
	return platform.NewLogger(os.Stderr, service, env, platform.ParseLevel(level))
}
