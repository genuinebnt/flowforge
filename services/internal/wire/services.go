package wire

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"time"

	"google.golang.org/grpc"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/engine/pgrepo"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/eventstore/pgstore"
	graphv1 "github.com/genuinebnt/flowforge/gen/flowforge/graph/v1"
	simv1 "github.com/genuinebnt/flowforge/gen/flowforge/similarity/v1"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/graphstore"
	graphpg "github.com/genuinebnt/flowforge/graphstore/pg"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/limiter/redislimiter"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/egressproxy"
	enginesvc "github.com/genuinebnt/flowforge/services/engine"
	graphsvc "github.com/genuinebnt/flowforge/services/graph"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/platform/redisconf"
	"github.com/genuinebnt/flowforge/services/maintenance"
	"github.com/genuinebnt/flowforge/services/notifier"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
	"github.com/genuinebnt/flowforge/services/similarity"
	"github.com/genuinebnt/flowforge/services/toolrunner"
)

func ping(env *Env) func(context.Context) error {
	if env.DB == nil {
		return nil
	}
	return func(ctx context.Context) error { return env.DB.Ping(ctx) }
}

func relayLoop(env *Env, src bus.OutboxSource) func(context.Context) error {
	r := &bus.Relay{Source: src, Bus: env.Bus, Logger: env.Logger, Metrics: env.Metrics, Interval: 100 * time.Millisecond}
	return r.Run
}

// ScopeOptions configure the scope service.
type ScopeOptions struct {
	// Resolver resolves names for active checks (default net.Resolver).
	Resolver scope.Resolver
}

// Scope builds the scope service. It returns the repository so an
// in-process egress proxy can read the decision log.
func Scope(env *Env, o ScopeOptions) (*Component, scopesvc.Repo) {
	var repo scopesvc.Repo = scopesvc.NewMemRepo()
	if env.DB != nil {
		repo = &scopesvc.PgRepo{DB: env.DB}
	}
	srv := scopesvc.NewServer(scopesvc.Options{Repo: repo, Resolver: o.Resolver, Clock: env.Clock, Logger: env.Logger, Metrics: env.Metrics})
	c := &Component{Register: func(g *grpc.Server) { pb.RegisterScopeServiceServer(g, srv) }, Ready: repo.Ping,
		Run: []func(context.Context) error{srv.ActivationLoop(0)}}
	if env.Bus != nil {
		c.Run = append(c.Run, relayLoop(env, repo.Outbox()))
	}
	return c, repo
}

// EngineOptions configure the engine service.
type EngineOptions struct {
	// ScopeAddr and ToolRunnerAddr are required; GraphAddr is optional
	// (without it observations are not forwarded to the graph).
	ScopeAddr, ToolRunnerAddr, GraphAddr string
	// RedisAddr selects the Redis rate limiter (FF_REDIS_ADDR, with the
	// local failover of spec 23.8); empty uses an in-process GCRA.
	RedisAddr, RedisAuth string
	// RedisCA is FF_REDIS_CA (redisconf.Config.CA): set enables TLS.
	RedisCA string
	// Workers is the number of scheduler workers (enginesvc.Options).
	Workers int
	Engine  engine.Config
	// ScheduleInterval is how often the in-process scheduler fires due
	// workflow schedules (default 15 s; G-OPS-10).
	ScheduleInterval time.Duration
	// MaintenanceInterval is how often the retention and partition jobs
	// run in process with Postgres (default 1 h; negative disables, e.g.
	// when Cloud Scheduler's tick drives them; G-OPS-03).
	MaintenanceInterval time.Duration
	// TickAudience and TickServiceAccount accept Cloud Scheduler's Google
	// ID token on POST /internal/schedules:tick.
	TickAudience, TickServiceAccount string
}

// Engine builds the engine service: Postgres stores (event store, run
// repository, outbox, inbox, schedules, maintenance) when env.DB is set,
// in-memory ones otherwise. It returns the component and the service, whose
// Engine the dev stack drives directly.
func Engine(ctx context.Context, env *Env, o EngineOptions) (*Component, *enginesvc.Service, error) {
	scopeConn, err := env.Dial(o.ScopeAddr)
	if err != nil {
		return nil, nil, err
	}
	runnerConn, err := env.Dial(o.ToolRunnerAddr)
	if err != nil {
		return nil, nil, err
	}
	d := enginesvc.Deps{Scope: scopesvc.NewClient(pb.NewScopeServiceClient(scopeConn), "engine"),
		Runner: pb.NewToolRunnerServiceClient(runnerConn), Bus: env.Bus, Logger: env.Logger, Metrics: env.Metrics,
		Clock: env.Clock, Verifier: env.Verifier}
	closers := []func(){func() { scopeConn.Close() }, func() { runnerConn.Close() }}
	if o.GraphAddr != "" {
		gc, err := env.Dial(o.GraphAddr)
		if err != nil {
			return nil, nil, err
		}
		closers = append(closers, func() { gc.Close() })
		d.Graph = pb.NewGraphServiceClient(gc)
	}
	var wake chan struct{}
	if env.DB != nil {
		st, err := pgstore.New(env.DB, pgstore.Config{DSN: env.DSN})
		if err != nil {
			return nil, nil, err
		}
		if err := pgrepo.EnsureSchema(ctx, env.DB); err != nil {
			return nil, nil, err
		}
		closers = append(closers, st.Close)
		d.Store, d.Repo, d.Tools = st, pgrepo.New(env.DB), enginesvc.PgToolStore{Tools: pgrepo.NewTools(env.DB)}
		d.Publisher, d.Outbox = pgrepo.NewOutbox(env.DB), bus.PgOutbox{DB: env.DB, Table: "engine.outbox"}
		d.Inbox = bus.PgInbox{DB: env.DB, Table: "engine.processed_events"}
		wake = make(chan struct{}, 1)
	} else {
		out := bus.NewMemOutbox()
		d.Store = eventstore.NewMemStore(env.Clock)
		// SystemRng is safe for concurrent use (crypto/rand), which the
		// workers sharing the repository need.
		d.Repo = enginesvc.NewMemRepo(core.IDGen{Clock: env.Clock, Rng: core.SystemRng{}})
		d.Tools = enginesvc.MemToolStore{MemTools: enginesvc.NewMemTools()}
		d.Publisher, d.Outbox, d.Inbox = enginesvc.MemPublisher{Outbox: out}, out, bus.NewMemInbox()
	}
	if o.RedisAddr != "" {
		// FF_REDIS_CA set means Memorystore TLS (G-OPS-13).
		rc, err := redisconf.NewClient(redisconf.Config{Addr: o.RedisAddr, Auth: o.RedisAuth, CA: o.RedisCA})
		if err != nil {
			return nil, nil, err
		}
		rl, err := redislimiter.New(rc, redislimiter.Options{}, enginesvc.DefaultLimitRules()...)
		if err != nil {
			return nil, nil, err
		}
		closers = append(closers, func() { rc.Close() })
		// Spec 23.8: on a Redis outage passive work continues on local
		// budgets and active work waits (G-LIM-02).
		if d.Limiter, err = enginesvc.NewFailoverLimiter(rl); err != nil {
			return nil, nil, err
		}
	} else {
		g, err := limiter.NewAtomicGCRA(enginesvc.DefaultLimitRules()...)
		if err != nil {
			return nil, nil, err
		}
		d.Limiter = g
	}
	svc, err := enginesvc.New(d, enginesvc.Options{Engine: o.Engine, Workers: o.Workers, Wakeups: wake})
	if err != nil {
		return nil, nil, err
	}
	var store enginesvc.ScheduleStore = enginesvc.NewMemSchedules()
	var maint func(context.Context) error
	var runner *maintenance.Runner
	if env.DB != nil {
		store = enginesvc.PgSchedules{DB: env.DB}
		runner = &maintenance.Runner{Pool: env.DB, Blobs: env.Blobs, Logger: env.Logger}
		maint = func(ctx context.Context) error {
			_, err := runner.RunOnce(ctx)
			if errors.Is(err, maintenance.ErrBusy) {
				return nil
			}
			return err
		}
	}
	sched := &enginesvc.Scheduler{Store: store, Engine: svc.Engine, Clock: env.Clock, Logger: env.Logger}
	sh := svc.SchedulesHandler(sched, enginesvc.TickAuth{Verifier: env.Verifier, Audience: o.TickAudience, ServiceAccount: o.TickServiceAccount}, maint)
	if o.ScheduleInterval <= 0 {
		o.ScheduleInterval = 15 * time.Second
	}
	c := &Component{
		Register: func(g *grpc.Server) { pb.RegisterEngineServiceServer(g, svc.Server) },
		Routes:   map[string]http.Handler{"/internal/schedules": sh, "/internal/schedules/": sh, "/internal/schedules:tick": sh},
		Run: []func(context.Context) error{svc.Run, func(ctx context.Context) error {
			sched.Run(ctx, o.ScheduleInterval)
			return nil
		}},
		Ready: ping(env),
		Close: func() {
			for _, f := range closers {
				f()
			}
		},
	}
	if wake != nil {
		c.Run = append(c.Run, func(ctx context.Context) error { enginesvc.ListenWake(ctx, env.DSN, wake, env.Logger); return nil })
	}
	if runner != nil && o.MaintenanceInterval >= 0 {
		iv := o.MaintenanceInterval
		if iv == 0 {
			iv = time.Hour
		}
		c.Run = append(c.Run, func(ctx context.Context) error { runner.Loop(ctx, iv); return nil })
	}
	return c, svc, nil
}

// ToolRunnerOptions configure the tool runner.
type ToolRunnerOptions struct {
	ScopeAddr   string
	EgressProxy string
	ProbePorts  []toolrunner.ProbePort
	// Adapters replaces the default adapters.
	Adapters []toolrunner.Adapter
	// ContentAddr is the content service; set adds the content tools
	// (collect, crawl, wellknown, extract, unpack_sourcemap, diff_latest,
	// search_changes) configured by Content (its Content client is
	// dialled here; Ports default to ProbePorts).
	ContentAddr string
	Content     toolrunner.ContentConfig
	// TargetTLS verifies program targets reached through an http://
	// egress proxy (nil: the system roots; tests pass a lab CA).
	TargetTLS *tls.Config
	// Clock is the tool runner's clock: capture times and crawl's revisit
	// schedule (default env.Clock).
	Clock core.Clock
}

// ToolRunner builds the tool runner.
func ToolRunner(env *Env, o ToolRunnerOptions) (*Component, error) {
	conn, err := env.Dial(o.ScopeAddr)
	if err != nil {
		return nil, err
	}
	eg, err := toolrunner.NewEgress(o.EgressProxy, env.Tokens)
	if err != nil {
		return nil, err
	}
	if eg != nil {
		eg.IDTokens = env.IDTokens
		eg.TargetTLS = o.TargetTLS
	}
	ads := o.Adapters
	if ads == nil {
		ads = toolrunner.DefaultAdapters(toolrunner.HTTPProbe{Ports: o.ProbePorts})
	}
	closers := []func(){func() { conn.Close() }}
	if o.ContentAddr != "" {
		cc, err := env.Dial(o.ContentAddr)
		if err != nil {
			conn.Close()
			return nil, err
		}
		closers = append(closers, func() { cc.Close() })
		cfg := o.Content
		cfg.Content = pb.NewContentServiceClient(cc)
		if cfg.Ports == nil {
			cfg.Ports = o.ProbePorts
		}
		cts, err := toolrunner.ContentAdapters(cfg)
		if err != nil {
			for _, c := range closers {
				c()
			}
			return nil, err
		}
		ads = append(append([]toolrunner.Adapter{}, ads...), cts...)
	}
	clock := o.Clock
	if clock == nil {
		clock = env.Clock
	}
	raw, err := toolrunner.RawStoreFor(env.Blobs, clock)
	if err != nil {
		for _, c := range closers {
			c()
		}
		return nil, err
	}
	srv := toolrunner.NewServer(toolrunner.Options{Adapters: ads, Scope: scopesvc.NewClient(pb.NewScopeServiceClient(conn), "tool-runner"),
		Egress: eg, Clock: clock, Logger: env.Logger, Raw: raw})
	return &Component{Register: func(g *grpc.Server) { pb.RegisterToolRunnerServiceServer(g, srv) }, Close: func() {
		for _, c := range closers {
			c()
		}
	}}, nil
}

// EgressOptions configure the egress proxy.
type EgressOptions struct {
	ScopeAddr string
	// Decisions is the decision log (in process: the scope service's
	// repository; standalone: the scope schema through env.DB).
	Decisions egressproxy.Decisions
	// Recorder records intel_source decisions (default: the scope schema
	// through env.DB when Decisions is not given; else none).
	Recorder egressproxy.Recorder

	// Ports is the port policy for host-level decisions (nil: 80, 443).
	Ports *egressproxy.PortPolicy
	// Intel is the intelligence-source allowlist (nil: none);
	// IntelResolver resolves sources without pinned addresses (default
	// the system resolver).
	Intel         []egressproxy.IntelSource
	IntelResolver scope.Resolver
	// SOCKSListen and DNSListen are the listen addresses of the SOCKS5
	// listener and the DNS forwarder (UDP and TCP); "" disables them.
	// SOCKSAdvertise and DNSAdvertise are the addresses tickets carry
	// (default: the listener's address, an unspecified host replaced by
	// 127.0.0.1, the sandbox-sidecar case).
	SOCKSListen, DNSListen       string
	SOCKSAdvertise, DNSAdvertise string
	// Upstream answers forwarded DNS queries (default the system resolver).
	Upstream egressproxy.Upstream
	// RedisAddr, when set, keeps the per-target limiter in Redis so every
	// replica shares it (Redis TIME is the clock).
	RedisAddr, RedisAuth string
	// RedisCA is FF_REDIS_CA (redisconf.Config.CA): set enables TLS.
	RedisCA string

	// NotifyAllowlist are the notifier destinations the proxy's notify path
	// admits (FF_NOTIFY_ALLOWLIST; egressproxy.Proxy.NotifyAllowlist).
	NotifyAllowlist []string
	// Resolver resolves notify destinations (default net.Resolver).
	Resolver scope.Resolver
}

// EgressProxy builds the egress proxy (a plain HTTP proxy, not gRPC).
func EgressProxy(env *Env, o EgressOptions) (*Component, *egressproxy.Proxy, error) {
	conn, err := env.Dial(o.ScopeAddr)
	if err != nil {
		return nil, nil, err
	}
	if o.Decisions == nil {
		if env.DB == nil {
			return nil, nil, errors.New("egress proxy: needs the scope decision log (FF_DB_DSN) when not in process")
		}
		o.Decisions = scopesvc.DecisionLog{Repo: &scopesvc.PgRepo{DB: env.DB}}
	}
	if o.Recorder == nil && env.DB != nil {
		o.Recorder = &scopesvc.PgRepo{DB: env.DB}
	}
	p := &egressproxy.Proxy{Decisions: o.Decisions, Scope: scopesvc.NewClient(pb.NewScopeServiceClient(conn), "egress-proxy"),
		Verifier: env.Verifier, Logger: env.Logger, Metrics: env.Metrics, Recorder: o.Recorder, Ports: o.Ports, Intel: o.Intel,
		IntelResolver: o.IntelResolver, Upstream: o.Upstream, NotifyAllowlist: o.NotifyAllowlist, Resolver: o.Resolver}
	if p.Resolver == nil {
		p.Resolver = scopesvc.NetResolver{}
	}
	if p.IntelResolver == nil {
		p.IntelResolver = scopesvc.NetResolver{}
	}
	if p.Upstream == nil {
		p.Upstream = egressproxy.NetUpstream{}
	}
	closers := []func(){func() { conn.Close() }}
	closeAll := func() {
		for _, c := range closers {
			c()
		}
	}
	if o.RedisAddr != "" {
		rc, err := redisconf.NewClient(redisconf.Config{Addr: o.RedisAddr, Auth: o.RedisAuth, CA: o.RedisCA})
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		rl, err := redislimiter.New(rc, redislimiter.Options{})
		if err != nil {
			closeAll()
			return nil, nil, err
		}
		closers = append(closers, func() { rc.Close() })
		p.Limiter = egressproxy.ContextLimiter{L: rl}
	}
	var runs []func(context.Context) error
	if o.SOCKSListen != "" {
		l, err := net.Listen("tcp", o.SOCKSListen)
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("egress proxy: SOCKS5 listener: %w", err)
		}
		closers = append(closers, func() { l.Close() })
		p.SOCKSAddr = advertise(o.SOCKSAdvertise, l.Addr())
		runs = append(runs, func(ctx context.Context) error { return p.ServeSOCKS(ctx, l) })
	}
	if o.DNSListen != "" {
		l, err := net.Listen("tcp", o.DNSListen)
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("egress proxy: DNS listener: %w", err)
		}
		closers = append(closers, func() { l.Close() })
		// UDP on the port the TCP listener got (DNSListen may say port 0).
		pc, err := net.ListenPacket("udp", l.Addr().String())
		if err != nil {
			closeAll()
			return nil, nil, fmt.Errorf("egress proxy: DNS UDP listener: %w", err)
		}
		closers = append(closers, func() { pc.Close() })
		p.DNSAddr = advertise(o.DNSAdvertise, l.Addr())
		runs = append(runs, func(ctx context.Context) error { return p.ServeDNSTCP(ctx, l) },
			func(ctx context.Context) error { return p.ServeDNSUDP(ctx, pc) })
	}
	health := platform.NewHealth()
	admin := health.Mux(env.Metrics.Registry)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect && !r.URL.IsAbs() {
			admin.ServeHTTP(w, r) // /healthz, /readyz, /metrics
			return
		}
		p.ServeHTTP(w, r)
	})
	return &Component{Handler: h, Run: runs, Close: closeAll}, p, nil
}

// advertise returns adv, or addr with an unspecified host replaced by
// 127.0.0.1.
func advertise(adv string, addr net.Addr) string {
	if adv != "" {
		return adv
	}
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return addr.String()
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// GraphOptions configure the graph service.
type GraphOptions struct{ ScopeAddr string }

// Graph builds the graph service and graph analytics.
func Graph(env *Env, o GraphOptions) (*Component, graphstore.Store, error) {
	conn, err := env.Dial(o.ScopeAddr)
	if err != nil {
		return nil, nil, err
	}
	sc := scopesvc.NewClient(pb.NewScopeServiceClient(conn), "graph")
	cfg := graphstore.Config{Scope: graphstore.PolicyScope(graphsvc.ScopeFunc(sc, 5*time.Second)), Clock: env.Clock}
	var store graphstore.Store
	c := &Component{Ready: ping(env), Close: func() { conn.Close() }}
	if env.DB != nil {
		store = graphpg.New(env.DB, graphpg.Config{Config: cfg})
		if env.Bus != nil {
			relay := &graphpg.Relay{DB: env.DB, Clock: env.Clock, Publish: func(ctx context.Context, evs []graphstore.ChangeEvent) error {
				for _, ev := range evs {
					if err := env.Bus.Publish(ctx, convert.TopicGraphChanges, convert.ChangeEnvelope(ev, "")); err != nil {
						return err
					}
				}
				return nil
			}}
			// Relay graph.changes rows: drain while there are rows, back
			// off 100 ms when idle or after an error.
			c.Run = append(c.Run, func(ctx context.Context) error {
				for ctx.Err() == nil {
					n, err := relay.RunOnce(ctx)
					if err != nil && ctx.Err() == nil {
						env.Logger.WarnContext(ctx, "graph.relay_failed", "error", err.Error())
					}
					if n == 0 || err != nil {
						select {
						case <-ctx.Done():
						case <-time.After(100 * time.Millisecond):
						}
					}
				}
				return nil
			})
		}
	} else {
		store = graphstore.NewMem(cfg)
	}
	srv := graphsvc.NewServer(graphsvc.Options{Store: store, Bus: env.Bus, PublishInline: env.DB == nil, Logger: env.Logger, Metrics: env.Metrics, Clock: env.Clock})
	if env.Bus != nil {
		// Content events (V2 §6): observations and content_changed.
		c.Run = append(c.Run, func(ctx context.Context) error {
			if err := env.Bus.Subscribe(ctx, convert.TopicContentChanges, graphsvc.ContentConsumer, srv.HandleContentEvent); err != nil {
				return err
			}
			<-ctx.Done()
			return nil
		})
	}
	an := graphsvc.NewAnalytics(store, env.Clock)
	an.Blobs = env.Blobs
	an.OnWriteError = func(ctx context.Context, program string, err error) {
		env.Logger.WarnContext(ctx, "graph.snapshot_write_failed", "program_id", program, "error", err.Error())
	}
	c.Register = func(g *grpc.Server) {
		pb.RegisterGraphServiceServer(g, srv)
		graphv1.RegisterGraphAnalyticsServer(g, an)
	}
	return c, store, nil
}

// SimilarityOptions configure the similarity service.
type SimilarityOptions struct {
	GraphAddr, ScopeAddr string
	Embedder             similarity.Embedder
}

// Similarity builds the similarity service.
func Similarity(env *Env, o SimilarityOptions) (*Component, error) {
	gc, err := env.Dial(o.GraphAddr)
	if err != nil {
		return nil, err
	}
	sc, err := env.Dial(o.ScopeAddr)
	if err != nil {
		return nil, err
	}
	srv := similarity.NewServer(similarity.Options{Embedder: o.Embedder, Graph: pb.NewGraphServiceClient(gc),
		Scope: scopesvc.NewClient(pb.NewScopeServiceClient(sc), "similarity"), Logger: env.Logger})
	c := &Component{Register: func(g *grpc.Server) { simv1.RegisterSimilarityServer(g, srv) }, Close: func() { gc.Close(); sc.Close() }}
	if env.Bus != nil {
		c.Run = append(c.Run, func(ctx context.Context) error {
			if err := env.Bus.Subscribe(ctx, convert.TopicGraphChanges, "similarity", srv.HandleChange); err != nil {
				return err
			}
			<-ctx.Done()
			return nil
		})
	}
	return c, nil
}

// NotifierOptions configure the notifier.
type NotifierOptions struct {
	Secrets               notifier.Secrets
	Resolver              scope.Resolver
	Email                 notifier.EmailSender
	AllowInsecureLoopback bool
	ConsoleBaseURL        string
	// Egress routes webhook and Slack deliveries through the egress proxy
	// (nil: direct, local development).
	Egress *notifier.NotifyEgress
}

// Notifier builds the notifier.
func Notifier(env *Env, o NotifierOptions) (*Component, *notifier.Service) {
	var repo notifier.Repo = notifier.NewMemRepo()
	if env.DB != nil {
		repo = &notifier.PgRepo{DB: env.DB}
	}
	if o.Resolver == nil {
		o.Resolver = scopesvc.NetResolver{}
	}
	if o.Email == nil {
		o.Email = notifier.LogEmail{Logger: env.Logger}
	}
	svc := notifier.NewService(notifier.Options{Repo: repo, Bus: env.Bus, Clock: env.Clock, Logger: env.Logger, Metrics: env.Metrics,
		Senders: &notifier.Senders{Secrets: o.Secrets, Resolver: o.Resolver, Email: o.Email, AllowInsecureLoopback: o.AllowInsecureLoopback,
			Egress: o.Egress}})
	srv := &notifier.Server{S: svc}
	return &Component{Register: func(g *grpc.Server) { pb.RegisterNotifierServiceServer(g, srv) },
		Run: []func(context.Context) error{svc.Run}, Ready: repo.Ping}, svc
}
