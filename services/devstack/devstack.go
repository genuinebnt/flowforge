// Package devstack runs every internal service in one process on loopback
// ports (flowforge dev, end-to-end tests): scope, engine, tool runner,
// egress proxy, graph (+ analytics), similarity, notifier and content, wired over
// real gRPC with one shared internal signing key, one in-process event bus
// and in-memory stores, or Postgres when Options.DBDSN is set (migrations
// are applied first).
//
// The gateway and its tests use Start to get an internal backend:
//
//	st, err := devstack.Start(ctx, devstack.Options{})
//	defer st.Close()
//	ctx = st.As(ctx, tenant, "user:"+userID, auth.RoleAdmin)
//	prog, err := st.Scope.CreateProgram(ctx, ...)
//
// Every client in Stack mints internal tokens for the identity in the
// call's context (platform.WithIdentity / Stack.As).
package devstack

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/contentstore"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	graphv1 "github.com/genuinebnt/flowforge/gen/flowforge/graph/v1"
	simv1 "github.com/genuinebnt/flowforge/gen/flowforge/similarity/v1"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/scope"
	contentsvc "github.com/genuinebnt/flowforge/services/content"
	"github.com/genuinebnt/flowforge/services/egressproxy"
	enginesvc "github.com/genuinebnt/flowforge/services/engine"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/wire"
	"github.com/genuinebnt/flowforge/services/notifier"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
	"github.com/genuinebnt/flowforge/services/toolrunner"
)

// Service names, the keys of Stack.Addrs.
const (
	ScopeName      = "scope"
	EngineName     = "engine"
	ToolRunnerName = "tool-runner"
	EgressName     = "egress-proxy"
	GraphName      = "graph"
	SimilarityName = "similarity"
	NotifierName   = "notifier"
	ContentName    = "content"
	// SOCKSName and DNSName are the egress proxy's SOCKS5 listener and DNS
	// forwarder (UDP and TCP on the same port).
	SOCKSName = "egress-socks5"
	DNSName   = "egress-dns"
)

// Options configure the stack. Zero values give an in-memory stack on
// random loopback ports.
type Options struct {
	// DBDSN selects Postgres for every store (migrations are applied).
	DBDSN string
	// Host is the listen host (default 127.0.0.1); Ports optionally fixes a
	// port per service name (0 or absent: a free port).
	Host  string
	Ports map[string]int
	// Resolver serves scope checks, dnsresolve and webhook destinations
	// (default: the system resolver). Tests inject a static one.
	Resolver scope.Resolver
	// ProbePorts are the schemes and ports httpprobe tries (default
	// https:443, http:80).
	ProbePorts []toolrunner.ProbePort
	// Secrets resolves notifier secret references (default: an empty map).
	Secrets notifier.Secrets
	// AllowInsecureWebhooks permits http://127.0.0.1 webhook destinations.
	AllowInsecureWebhooks bool
	// Workers is the engine worker count (default 4).
	Workers int
	// Engine tunes the engine (tests shorten PollInterval).
	Engine engine.Config
	// Blobs receives raw tool output, snapshot files and run archives
	// (services/internal/blob); nil keeps them out of storage.
	Blobs graphstore.BlobStore
	// Logger defaults to discarding everything.
	Logger *slog.Logger
	// Issuer of internal tokens (default "flowforge-dev").
	Issuer string
	// EgressPorts is the TCP port policy for host-level decisions at the
	// egress proxy (default egressproxy.CommonPorts).
	EgressPorts []int
	// IntelSources is the intelligence-source allowlist (default
	// egressproxy.DefaultIntelSources()).
	IntelSources []egressproxy.IntelSource
	// DNSUpstream answers the DNS forwarder (default: Resolver for A/AAAA
	// when set, else the system resolver).
	DNSUpstream egressproxy.Upstream
	// EgressClock drives the proxy's per-target limiter (tests inject a
	// fake clock; default the system clock).
	EgressClock core.Clock
	// ContentLimits are the content service's default quotas (zero:
	// unlimited).
	ContentLimits contentstore.Limits
	// ToolClock is the tool runner's clock: capture times (fetched_at) and
	// crawl's revisit schedule (default the system clock). The lab drives
	// it to space deploys days apart.
	ToolClock core.Clock
	// TargetTLS verifies HTTPS program targets (nil: the system roots; the
	// lab passes its CA).
	TargetTLS *tls.Config
	// Content configures the content tools (profiles, bounds, revisit
	// policy); their content client and ports are set by the stack (ports:
	// ProbePorts).
	Content toolrunner.ContentConfig
	// TrustKeys, when set, is trusted for internal tokens in addition to
	// the stack's own dev key. cmd/flowforge dev passes the gateway's key
	// manager so tokens the gateway mints verify; they must carry Issuer.
	TrustKeys auth.KeyResolver
}

// Stack is a running dev stack.
type Stack struct {
	// Addrs maps service names to host:port.
	Addrs map[string]string
	// Clients over real gRPC on loopback.
	Scope      pb.ScopeServiceClient
	Engine     pb.EngineServiceClient
	ToolRunner pb.ToolRunnerServiceClient
	Graph      pb.GraphServiceClient
	Analytics  graphv1.GraphAnalyticsClient
	Notifier   pb.NotifierServiceClient
	Similarity simv1.SimilarityClient
	Content    pb.ContentServiceClient
	// Conns are the client connections by service name.
	Conns map[string]*grpc.ClientConn
	// Keys signs and verifies internal tokens; Tokens mints them (the
	// gateway mints with the same key in the dev stack).
	Keys     platform.Keys
	Tokens   *platform.MinterTokens
	Verifier *auth.Verifier
	Bus      *bus.Mem
	Metrics  *platform.Metrics
	// ScopeRepo is the scope service's storage (decision log assertions).
	ScopeRepo scopesvc.Repo
	// EngineService and Proxy expose internals for tests.
	EngineService *enginesvc.Service
	Proxy         *egressproxy.Proxy
	ContentServer *contentsvc.Server
	DB            *pgxpool.Pool

	cancel context.CancelFunc
	dnsUDP net.PacketConn
	wg     sync.WaitGroup
	errMu  sync.Mutex
	errs   []error
}

// As returns ctx carrying the identity the stack's clients mint tokens for.
func (s *Stack) As(ctx context.Context, tenant core.UUID, actor string, role auth.Role) context.Context {
	return platform.WithIdentity(ctx, platform.Identity{Tenant: tenant, Actor: actor, Role: role})
}

// Close stops every service and waits for them.
func (s *Stack) Close() error {
	s.cancel()
	s.wg.Wait()
	for _, c := range s.Conns {
		c.Close()
	}
	if s.DB != nil {
		s.DB.Close()
	}
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return errors.Join(s.errs...)
}

// Start starts the stack and returns once every service listens and the
// engine has loaded the tool runner's tools.
func Start(ctx context.Context, o Options) (*Stack, error) {
	if o.Host == "" {
		o.Host = "127.0.0.1"
	}
	if o.Logger == nil {
		o.Logger = platform.DiscardLogger()
	}
	if o.Issuer == "" {
		o.Issuer = "flowforge-dev"
	}
	if o.Secrets == nil {
		o.Secrets = notifier.NewMapSecrets(nil)
	}
	clock := &core.SystemClock{}
	keys, err := platform.NewDevKeys(o.Issuer)
	if err != nil {
		return nil, err
	}
	if o.TrustKeys != nil {
		keys.Resolver = anyResolver{keys.Resolver, o.TrustKeys}
	}
	verifier, err := keys.Verifier(clock)
	if err != nil {
		return nil, err
	}
	tokens, err := keys.Tokens(clock)
	if err != nil {
		return nil, err
	}
	metrics := platform.NewMetrics(nil)
	st := &Stack{Addrs: map[string]string{}, Conns: map[string]*grpc.ClientConn{}, Keys: keys, Tokens: tokens,
		Verifier: verifier, Bus: bus.NewMem(o.Logger, metrics), Metrics: metrics}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	st.cancel = cancel
	fail := func(err error) (*Stack, error) {
		_ = st.Close()
		return nil, err
	}
	if o.DBDSN != "" {
		pool, err := pgxpool.New(ctx, o.DBDSN)
		if err != nil {
			return fail(err)
		}
		st.DB = pool
		if err := migrations.Apply(ctx, pool); err != nil {
			return fail(fmt.Errorf("migrations: %w", err))
		}
	}
	// Listen first so every address is known before services dial.
	names := []string{ScopeName, GraphName, ToolRunnerName, EgressName, EngineName, SimilarityName, NotifierName, ContentName, SOCKSName, DNSName}
	lis := map[string]net.Listener{}
	closeAll := func() {
		for _, x := range lis {
			x.Close()
		}
	}
	for _, n := range names {
		l, err := net.Listen("tcp", net.JoinHostPort(o.Host, fmt.Sprint(o.Ports[n])))
		if err != nil {
			closeAll()
			return fail(err)
		}
		lis[n] = l
		st.Addrs[n] = l.Addr().String()
	}
	// The DNS forwarder also needs UDP on the TCP listener's port.
	for attempt := 0; ; attempt++ {
		pc, err := net.ListenPacket("udp", st.Addrs[DNSName])
		if err == nil {
			st.dnsUDP = pc
			break
		}
		if attempt >= 5 || o.Ports[DNSName] != 0 {
			closeAll()
			return fail(fmt.Errorf("devstack: DNS forwarder UDP listener: %w", err))
		}
		lis[DNSName].Close()
		l, err := net.Listen("tcp", net.JoinHostPort(o.Host, "0"))
		if err != nil {
			closeAll()
			return fail(err)
		}
		lis[DNSName] = l
		st.Addrs[DNSName] = l.Addr().String()
	}
	env := func(name string) *wire.Env {
		return &wire.Env{Service: name, Logger: o.Logger.With("component", name), Metrics: metrics, Clock: clock,
			Tokens: tokens, Verifier: verifier, DB: st.DB, DSN: o.DBDSN, Bus: st.Bus, Blobs: o.Blobs}
	}
	serve := func(name string, c *wire.Component) {
		st.wg.Add(1)
		go func() {
			defer st.wg.Done()
			if err := wire.Serve(runCtx, env(name), lis[name], c); err != nil {
				st.errMu.Lock()
				st.errs = append(st.errs, fmt.Errorf("%s: %w", name, err))
				st.errMu.Unlock()
			}
		}()
	}
	scopeC, repo := wire.Scope(env(ScopeName), wire.ScopeOptions{Resolver: o.Resolver})
	st.ScopeRepo = repo
	serve(ScopeName, scopeC)

	graphC, _, err := wire.Graph(env(GraphName), wire.GraphOptions{ScopeAddr: st.Addrs[ScopeName]})
	if err != nil {
		return fail(err)
	}
	serve(GraphName, graphC)

	egC, proxy, err := wire.EgressProxy(env(EgressName), wire.EgressOptions{ScopeAddr: st.Addrs[ScopeName], Decisions: scopesvc.DecisionLog{Repo: repo}})
	if err != nil {
		return fail(err)
	}
	st.Proxy = proxy
	if err := configureEgress(st, proxy, o, repo, lis); err != nil {
		return fail(err)
	}
	serve(EgressName, egC)
	st.serveEgressListeners(runCtx, proxy, lis)

	trC, err := wire.ToolRunner(env(ToolRunnerName), wire.ToolRunnerOptions{ScopeAddr: st.Addrs[ScopeName],
		EgressProxy: "http://" + st.Addrs[EgressName], Resolver: o.Resolver, ProbePorts: o.ProbePorts,
		ContentAddr: st.Addrs[ContentName], Content: o.Content, TargetTLS: o.TargetTLS, Clock: o.ToolClock})
	if err != nil {
		return fail(err)
	}
	serve(ToolRunnerName, trC)

	engC, esvc, err := wire.Engine(ctx, env(EngineName), wire.EngineOptions{ScopeAddr: st.Addrs[ScopeName],
		ToolRunnerAddr: st.Addrs[ToolRunnerName], GraphAddr: st.Addrs[GraphName], Workers: o.Workers, Engine: o.Engine})
	if err != nil {
		return fail(err)
	}
	st.EngineService = esvc
	serve(EngineName, engC)

	simC, err := wire.Similarity(env(SimilarityName), wire.SimilarityOptions{GraphAddr: st.Addrs[GraphName], ScopeAddr: st.Addrs[ScopeName]})
	if err != nil {
		return fail(err)
	}
	serve(SimilarityName, simC)

	contC, csrv, err := wire.Content(env(ContentName), wire.ContentOptions{Limits: o.ContentLimits, SweepInterval: -1})
	if err != nil {
		return fail(err)
	}
	st.ContentServer = csrv
	serve(ContentName, contC)

	notC, _ := wire.Notifier(env(NotifierName), wire.NotifierOptions{Secrets: o.Secrets, Resolver: o.Resolver, AllowInsecureLoopback: o.AllowInsecureWebhooks})
	serve(NotifierName, notC)

	for _, n := range []string{ScopeName, EngineName, ToolRunnerName, GraphName, SimilarityName, NotifierName, ContentName} {
		c, err := platform.Dial(st.Addrs[n], platform.ClientOptions{Tokens: tokens})
		if err != nil {
			return fail(err)
		}
		st.Conns[n] = c
	}
	st.Scope = pb.NewScopeServiceClient(st.Conns[ScopeName])
	st.Engine = pb.NewEngineServiceClient(st.Conns[EngineName])
	st.ToolRunner = pb.NewToolRunnerServiceClient(st.Conns[ToolRunnerName])
	st.Graph = pb.NewGraphServiceClient(st.Conns[GraphName])
	st.Analytics = graphv1.NewGraphAnalyticsClient(st.Conns[GraphName])
	st.Notifier = pb.NewNotifierServiceClient(st.Conns[NotifierName])
	st.Similarity = simv1.NewSimilarityClient(st.Conns[SimilarityName])
	st.Content = pb.NewContentServiceClient(st.Conns[ContentName])

	select {
	case <-esvc.ToolsReady():
	case <-time.After(15 * time.Second):
		return fail(errors.New("devstack: the engine did not load the tool runner's tools"))
	case <-ctx.Done():
		return fail(ctx.Err())
	}
	return st, nil
}

// configureEgress sets the egress proxy's v2 features before it serves:
// the port policy, the intelligence-source allowlist (resolved with the
// stack's resolver, recorded in the scope decision log), the DNS upstream
// and the advertised SOCKS5 and DNS addresses.
func configureEgress(st *Stack, p *egressproxy.Proxy, o Options, repo scopesvc.Repo, lis map[string]net.Listener) error {
	ports := o.EgressPorts
	pp := &egressproxy.PortPolicy{Default: egressproxy.CommonPorts}
	if ports != nil {
		pp.Default = egressproxy.Ports(ports...)
	}
	p.Ports = pp
	p.Intel = o.IntelSources
	if p.Intel == nil {
		p.Intel = egressproxy.DefaultIntelSources()
	}
	p.IntelResolver = o.Resolver
	if p.IntelResolver == nil {
		p.IntelResolver = scopesvc.NetResolver{}
	}
	p.Recorder = repo
	p.Upstream = o.DNSUpstream
	if p.Upstream == nil {
		if o.Resolver != nil {
			p.Upstream = egressproxy.ResolverUpstream{Resolver: o.Resolver}
		} else {
			p.Upstream = egressproxy.NetUpstream{}
		}
	}
	if o.EgressClock != nil {
		p.Clock = o.EgressClock
	}
	p.SOCKSAddr, p.DNSAddr = st.Addrs[SOCKSName], st.Addrs[DNSName]
	if lis[SOCKSName] == nil || lis[DNSName] == nil || st.dnsUDP == nil {
		return errors.New("devstack: egress listeners missing")
	}
	return nil
}

// serveEgressListeners serves the SOCKS5 listener and the DNS forwarder.
func (s *Stack) serveEgressListeners(ctx context.Context, p *egressproxy.Proxy, lis map[string]net.Listener) {
	run := func(name string, f func() error) {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			if err := f(); err != nil && ctx.Err() == nil {
				s.errMu.Lock()
				s.errs = append(s.errs, fmt.Errorf("%s: %w", name, err))
				s.errMu.Unlock()
			}
		}()
	}
	run(SOCKSName, func() error { return p.ServeSOCKS(ctx, lis[SOCKSName]) })
	run(DNSName+"/udp", func() error { return p.ServeDNSUDP(ctx, s.dnsUDP) })
	run(DNSName+"/tcp", func() error { return p.ServeDNSTCP(ctx, lis[DNSName]) })
}

// anyResolver resolves a kid with the first resolver that knows it.
type anyResolver []auth.KeyResolver

// ResolveKey implements auth.KeyResolver.
func (rs anyResolver) ResolveKey(ctx context.Context, kid string) (auth.PublicKey, error) {
	var err error
	for _, r := range rs {
		var k auth.PublicKey
		if k, err = r.ResolveKey(ctx, kid); err == nil {
			return k, nil
		}
	}
	return auth.PublicKey{}, err
}
