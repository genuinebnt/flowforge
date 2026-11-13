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
	"sort"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/services/internal/blob"
	"github.com/genuinebnt/flowforge/services/internal/bus/busconfig"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/platform/gcpauth"
	"github.com/genuinebnt/flowforge/services/internal/platform/smsecrets"
	"github.com/genuinebnt/flowforge/services/internal/wire"
	"github.com/genuinebnt/flowforge/services/maintenance"
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
		log.Warn("config.no_signing_key", "detail", "FF_SIGNING_KID with FF_SIGNING_KEY_<KID> (or FF_SIGNING_KEY) unset: calls to other services will be rejected")
	}
	env := &wire.Env{Service: service, Logger: log, Metrics: platform.NewMetrics(nil), Clock: clock, Verifier: verifier, DSN: cfg.DBDSN}
	if tokens != nil {
		env.Tokens = tokens
	}
	if cfg.IDTokens {
		// Cloud Run: callee addresses are https:// run.app URLs that admit
		// only callers with an ID token for them (G-SVC-13).
		env.IDTokens = gcpauth.New()
	}
	if cfg.DBDSN != "" {
		pool, err := pgxpool.New(ctx, cfg.DBDSN)
		if err != nil {
			return err
		}
		defer pool.Close()
		env.DB = pool
	}
	// The bus (G-SVC-01): FF_BUS, else Pub/Sub with FF_PUBSUB_PROJECT,
	// else the Postgres bus with FF_DB_DSN, else in process (a lone
	// process: its outbox relay and consumers talk only to themselves).
	b, closeBus, err := busconfig.Open(ctx, busconfig.Config{Kind: cfg.Bus, Pool: env.DB, Project: cfg.PubSubProject,
		Prefix: cfg.TopicPrefix, CreateMissing: cfg.PubSubCreate, Logger: log, Metrics: env.Metrics})
	if err != nil {
		return err
	}
	defer closeBus()
	env.Bus = b
	kind, _ := busconfig.Resolve(busconfig.Config{Kind: cfg.Bus, Pool: env.DB, Project: cfg.PubSubProject})
	blobs, closeBlobs, err := blob.Open(ctx, blob.Config{Bucket: cfg.ArtifactBucket, Dir: cfg.BlobDir})
	if err != nil {
		return err
	}
	defer closeBlobs()
	env.Blobs = blobs
	log.Info("config.adapters", "bus", kind, "blobs", blob.Kind(blob.Config{Bucket: cfg.ArtifactBucket, Dir: cfg.BlobDir}))
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
		drain, err := drainTimeout(getenv)
		if err != nil {
			return nil, err
		}
		maint, err := maintenanceInterval(cfg.MaintenanceInterval)
		if err != nil {
			return nil, err
		}
		c, _, err := wire.Engine(ctx, env, wire.EngineOptions{ScopeAddr: cfg.ScopeAddr, ToolRunnerAddr: cfg.ToolRunnerAddr,
			GraphAddr: cfg.GraphAddr, RedisAddr: cfg.RedisAddr, RedisAuth: cfg.RedisAuth, RedisCA: cfg.RedisCA, Workers: cfg.Workers,
			Engine: engine.Config{FuelLimit: cfg.FuelDefault, DrainTimeout: drain}, MaintenanceInterval: maint,
			TickAudience: cfg.TickAudience, TickServiceAccount: cfg.TickServiceAccount})
		return c, err
	case "tool-runner":
		if cfg.EgressProxy == "" && cfg.Env == "prod" {
			return nil, errors.New("FF_EGRESS_PROXY is required in prod")
		}
		return wire.ToolRunner(env, wire.ToolRunnerOptions{ScopeAddr: cfg.ScopeAddr, EgressProxy: cfg.EgressProxy})
	case "egress-proxy":
		o, err := wire.EgressConfigFromEnv(getenv)
		if err != nil {
			return nil, err
		}
		o.ScopeAddr = cfg.ScopeAddr
		c, _, err := wire.EgressProxy(env, o)
		return c, err
	case "graph":
		c, _, err := wire.Graph(env, wire.GraphOptions{ScopeAddr: cfg.ScopeAddr})
		return c, err
	case "similarity":
		return wire.Similarity(env, wire.SimilarityOptions{GraphAddr: cfg.GraphAddr, ScopeAddr: cfg.ScopeAddr})
	case "notifier":
		// env:// and file:// resolve locally; sm:// through Secret Manager
		// (G-SVC-04). The SM client starts on the first sm:// lookup, so a
		// local notifier without Google credentials runs as long as no
		// channel uses one.
		secrets := platform.LocalSecrets(getenv, &smsecrets.Store{Project: cfg.SecretProject})
		// Webhook and Slack deliveries leave through the egress proxy's
		// notify path (spec 17.1, G-SVC-08); prod has no other way out.
		if cfg.EgressProxy == "" && cfg.Env == "prod" {
			return nil, errors.New("FF_EGRESS_PROXY is required in prod")
		}
		eg, err := notifier.NewNotifyEgress(cfg.EgressProxy, env.Tokens, env.IDTokens)
		if err != nil {
			return nil, err
		}
		c, _ := wire.Notifier(env, wire.NotifierOptions{Secrets: secrets, Email: notifier.SMTPEmail{Secrets: secrets}, Egress: eg})
		return c, nil
	}
	return nil, fmt.Errorf("unknown service %q", cfg.Service)
}

// drainTimeout reads FF_DRAIN_TIMEOUT (a Go duration; "-1s" disables the
// drain), the engine's graceful-shutdown bound (engine.Config.DrainTimeout,
// default 20 s). It must stay below the platform's termination grace
// period (Cloud Run: the time between SIGTERM and SIGKILL), or in-flight
// effects are killed before they are recorded.
func drainTimeout(getenv func(string) string) (time.Duration, error) {
	v := getenv("FF_DRAIN_TIMEOUT")
	if v == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d == 0 {
		return 0, fmt.Errorf("FF_DRAIN_TIMEOUT %q: want a non-zero duration such as 20s", v)
	}
	return d, nil
}

// maintenanceInterval parses FF_MAINTENANCE_INTERVAL: empty is the
// default (hourly), "0" disables the in-process jobs (GCP, where the
// Cloud Scheduler tick runs them).
func maintenanceInterval(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("FF_MAINTENANCE_INTERVAL: %w", err)
	}
	if d == 0 {
		return -1, nil
	}
	return d, nil
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

// Maintain runs one maintenance pass (cmd/migrate maintain; spec 10.7
// retention, partitions, archival) against FF_DB_DSN, archiving runs to
// FF_ARTIFACT_BUCKET or FF_BLOB_DIR. It prints the per-job counts.
func Maintain(ctx context.Context, getenv func(string) string) error {
	dsn := getenv("FF_DB_DSN")
	if dsn == "" {
		return errors.New("FF_DB_DSN is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()
	bc := blob.Config{Bucket: getenv("FF_ARTIFACT_BUCKET"), Dir: getenv("FF_BLOB_DIR")}
	blobs, closeBlobs, err := blob.Open(ctx, bc)
	if err != nil {
		return err
	}
	defer closeBlobs()
	if blob.Kind(bc) == "mem" {
		fmt.Fprintln(os.Stderr, "maintain: no FF_ARTIFACT_BUCKET or FF_BLOB_DIR: run archival is skipped")
	}
	r := &maintenance.Runner{Pool: pool, Blobs: blobs, Logger: Logger("migrate", or(getenv("FF_ENV"), "dev"), getenv("FF_LOG_LEVEL"))}
	rep, err := r.RunOnce(ctx)
	jobs := make([]string, 0, len(rep))
	for j := range rep {
		jobs = append(jobs, j)
	}
	sort.Strings(jobs)
	for _, j := range jobs {
		fmt.Printf("%s %d\n", j, rep[j])
	}
	return err
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Logger returns the JSON logger of spec 18.1 writing to stderr.
func Logger(service, env, level string) *slog.Logger {
	return platform.NewLogger(os.Stderr, service, env, platform.ParseLevel(level))
}
