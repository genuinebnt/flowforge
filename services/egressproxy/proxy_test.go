package egressproxy_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
	"github.com/genuinebnt/flowforge/services/toolrunner"
)

type server struct {
	*httptest.Server
	hits atomic.Int64
	port int
}

func listen(t *testing.T, addr string, h http.HandlerFunc) *server {
	t.Helper()
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Skipf("cannot listen on %s: %v", addr, err)
	}
	s := &server{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.hits.Add(1); h(w, r) }))
	s.Server.Listener.Close()
	s.Server.Listener = l
	s.Start()
	t.Cleanup(s.Close)
	_, ps, _ := net.SplitHostPort(l.Addr().String())
	s.port, _ = strconv.Atoi(ps)
	return s
}

var tenant = core.MustParseUUID("0190a1b2-0000-7000-8000-0000000e9e55")

// TestEgressProxy_MockTarget is the M7 mock-target integration test: an
// allowed request succeeds; a redirect to an out-of-scope host, DNS
// rebinding after the decision and userinfo / decimal-IP URL tricks are all
// blocked, and every refusal is logged and counted.
func TestEgressProxy_MockTarget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	evil := listen(t, "127.0.0.1:0", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "evil") })
	target := listen(t, "127.0.0.1:0", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, "/", http.StatusFound)
			return
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "http://evil.test:"+strconv.Itoa(evil.port)+"/", http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, "target")
	})
	// The rebinding destination: same port, another loopback address.
	rebound := listen(t, "127.0.0.2:"+strconv.Itoa(target.port), func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "rebound") })

	resolver := servicetest.NewResolver(map[string]string{"app.test": "127.0.0.1", "evil.test": "127.0.0.1"})
	st, err := devstack.Start(ctx, devstack.Options{Resolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx = st.As(ctx, tenant, "user:0190a1b2-0000-7000-8000-000000000001", auth.RoleAdmin)
	prog, err := st.Scope.CreateProgram(ctx, &pb.CreateProgramRequest{Name: "egress", InitialScope: &pb.ScopeDocument{
		Rules: []*pb.ScopeRule{
			{Id: "app", Effect: pb.RuleEffect_RULE_EFFECT_ALLOW, Kind: pb.RuleKind_RULE_KIND_DOMAIN, Value: "app.test"},
			{Id: "lo", Effect: pb.RuleEffect_RULE_EFFECT_ALLOW, Kind: pb.RuleKind_RULE_KIND_CIDR, Value: "127.0.0.1/32"},
		},
		Activity: &pb.ActivityPolicy{MaxClass: pb.ActivityClass_ACTIVITY_CLASS_ACTIVE},
	}})
	if err != nil {
		t.Fatal(err)
	}
	program, _ := core.ParsePublicID(core.PrefixProgram, prog.GetId())
	sc := scopesvc.NewClient(st.Scope, "tool-runner")
	tc := &toolrunner.ToolCtx{Tenant: tenant, Program: program, Path: "0", Clock: &core.SystemClock{}}
	tc.Check = func(ctx context.Context, tgt string, kind scope.TargetKind, class core.ActivityClass) (scope.Decision, error) {
		return sc.CheckTarget(ctx, scopesvc.Target{Tenant: tenant, Program: program, Target: tgt, Kind: kind, Class: class, Resolve: true})
	}
	eg, err := toolrunner.NewEgress("http://"+st.Addrs[devstack.EgressName], st.Tokens)
	if err != nil {
		t.Fatal(err)
	}
	appURL := "http://app.test:" + strconv.Itoa(target.port) + "/"
	decide := func(u string) scope.Decision {
		t.Helper()
		d, err := tc.Check(ctx, u, scope.TargetURL, core.ClassActive)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	refusals := func(reason string) float64 { return st.Metrics.Registry.Value("ff_egress_refused_total", reason) }

	// (a) An allowed request succeeds, connecting to the pinned address.
	d := decide(appURL)
	if !d.Allowed() || len(d.PinnedIPs) != 1 || d.PinnedIPs[0].String() != "127.0.0.1" {
		t.Fatalf("decision %+v", d)
	}
	var skipped []string
	client := eg.HTTPClient(tc, d, func(target, reason string) { skipped = append(skipped, target+" "+reason) })
	body := get(t, client, appURL, http.StatusOK)
	if body != "target" {
		t.Fatalf("body %q", body)
	}

	// (b) Every redirect hop is re-checked. An in-scope hop is followed under
	// its own fresh decision; a redirect to an out-of-scope host is refused
	// by the proxy (plain HTTP) before the client could follow it, and the
	// client re-checks every Location itself (the only check for https,
	// where the proxy sees a tunnel).
	if body := get(t, client, appURL+"hop", http.StatusOK); body != "target" || len(skipped) != 0 {
		t.Fatalf("in-scope hop: %q %v", body, skipped)
	}
	get(t, client, appURL+"redirect", http.StatusForbidden)
	if evil.hits.Load() != 0 {
		t.Fatalf("redirect followed: evil hits %d", evil.hits.Load())
	}
	naive := rawProxyClient(t, st, d.ID)
	resp := do(t, naive, appURL+"redirect")
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get(platform.HeaderRefused) != "redirect_denied" || evil.hits.Load() != 0 {
		t.Fatalf("proxy passed an out-of-scope redirect: %d %q", resp.StatusCode, resp.Header.Get(platform.HeaderRefused))
	}

	// (c) DNS rebinding: after the decision app.test resolves to another
	// address; the proxy still dials only the pinned one.
	resolver.Set("app.test", "127.0.0.2")
	before := target.hits.Load()
	if body := get(t, client, appURL, http.StatusOK); body != "target" || rebound.hits.Load() != 0 || target.hits.Load() != before+1 {
		t.Fatalf("rebinding reached %q (rebound hits %d)", body, rebound.hits.Load())
	}
	// A fresh decision now pins 127.0.0.2, which is loopback and not
	// explicitly in scope: the proxy refuses it.
	d2 := decide(appURL)
	if !d2.Allowed() || d2.PinnedIPs[0].String() != "127.0.0.2" {
		t.Fatalf("decision after rebinding %+v", d2)
	}
	resp = do(t, rawProxyClient(t, st, d2.ID), appURL)
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get(platform.HeaderRefused) != "forbidden_address" || rebound.hits.Load() != 0 {
		t.Fatalf("internal address not refused: %d %q", resp.StatusCode, resp.Header.Get(platform.HeaderRefused))
	}
	resolver.Set("app.test", "127.0.0.1")

	// (d) URL tricks: userinfo and decimal IPs are denied by the scope
	// check, and the proxy refuses them under app.test's decision.
	for _, trick := range []string{
		"http://app.test@evil.test:" + strconv.Itoa(evil.port) + "/",
		"http://2130706433:" + strconv.Itoa(target.port) + "/",
		"http://app.test.evil.test:" + strconv.Itoa(evil.port) + "/",
	} {
		if d := decide(trick); d.Allowed() {
			t.Errorf("scope allowed %s", trick)
		}
		resp := do(t, rawProxyClient(t, st, d.ID), trick)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("proxy passed %s: %d", trick, resp.StatusCode)
		}
	}
	if evil.hits.Load() != 0 {
		t.Fatalf("evil reached %d times", evil.hits.Load())
	}

	// No decision, an unknown decision, no token, a kill switch.
	if resp := do(t, rawProxyClient(t, st, ""), appURL); resp.Header.Get(platform.HeaderRefused) != "decision_missing" {
		t.Errorf("missing decision: %q", resp.Header.Get(platform.HeaderRefused))
	}
	if resp := do(t, rawProxyClient(t, st, "dec_01JD8KBB00000000000000000"), appURL); resp.Header.Get(platform.HeaderRefused) != "decision_unknown" {
		t.Errorf("unknown decision: %q", resp.Header.Get(platform.HeaderRefused))
	}
	pu, _ := url.Parse("http://" + st.Addrs[devstack.EgressName])
	anon := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	if resp := do(t, anon, appURL); resp.StatusCode != http.StatusProxyAuthRequired {
		t.Errorf("no token: %d", resp.StatusCode)
	}
	if _, err := st.Scope.SetKillSwitch(ctx, &pb.SetKillSwitchRequest{Level: "program", TargetId: prog.GetId(), Enabled: true, Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	servicetest.Eventually(t, 5*time.Second, "kill switch at the proxy", func() bool {
		return do(t, rawProxyClient(t, st, d.ID), appURL).Header.Get(platform.HeaderRefused) == "kill_switch"
	})
	for _, r := range []string{"redirect_denied", "forbidden_address", "decision_missing", "kill_switch"} {
		if refusals(r) == 0 {
			t.Errorf("refusal %s not counted", r)
		}
	}
	// Through CONNECT (what https and tcpconnect use) the same rules hold.
	conn, err := eg.Dial(ctx, tc, d, "evil.test:"+strconv.Itoa(evil.port))
	if err == nil {
		conn.Close()
		t.Fatal("CONNECT to another host under app.test's decision succeeded")
	}
}

// rawProxyClient sends every request through the proxy with the decision
// header and never follows redirects (a misbehaving tool).
func rawProxyClient(t *testing.T, st *devstack.Stack, decision string) *http.Client {
	t.Helper()
	pu, _ := url.Parse("http://" + st.Addrs[devstack.EgressName])
	tok, err := st.Tokens.Token(context.Background(), platform.Identity{Tenant: tenant, Actor: "service:tool-runner", Role: auth.RoleAnalyst})
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{Proxy: http.ProxyURL(pu), DisableKeepAlives: true}
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			r = r.Clone(r.Context())
			r.Header.Set(platform.HeaderProxyAuthorization, "Bearer "+tok)
			if decision != "" {
				r.Header.Set(platform.HeaderDecisionID, decision)
			}
			return tr.RoundTrip(r)
		}),
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func do(t *testing.T, c *http.Client, u string) *http.Response {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

func get(t *testing.T, c *http.Client, u string, want int) string {
	t.Helper()
	resp, err := c.Get(u)
	if err != nil {
		t.Fatalf("GET %s: %v", u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		t.Fatalf("GET %s: %d %s (%s), want %d", u, resp.StatusCode, b, resp.Header.Get(platform.HeaderRefused), want)
	}
	return string(b)
}
