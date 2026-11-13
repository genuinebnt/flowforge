package gateway_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/services/gateway"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
	"github.com/genuinebnt/flowforge/services/gateway/identity/identitytest"
	"github.com/genuinebnt/flowforge/services/gateway/internal/fakebackend"
	"github.com/genuinebnt/flowforge/testkit"
)

var t0 = time.Date(2026, 11, 12, 13, 0, 0, 0, time.UTC)

// csrfToken is the ff_csrf cookie value the harness sets for cookie auth.
const csrfToken = "test-csrf-token"

type tenant struct {
	id     core.UUID
	accts  map[auth.Role]identitytest.Account
	tokens map[auth.Role]string
}

type harness struct {
	t       testing.TB
	clock   *testkit.FakeClock
	fx      *identitytest.Fixture
	be      *fakebackend.Backend
	srv     *gateway.Server
	h       http.Handler
	tenants map[string]*tenant
}

type harnessOpts struct {
	keepAlive time.Duration
	// limiterFactory backs both the API limits and the auth throttles.
	limiterFactory identity.LimiterFactory
}

func bigLimits() map[string]limiter.Params {
	p := limiter.Params{Rate: 1e6, Burst: 1 << 20}
	return map[string]limiter.Params{gateway.ClassAuth: p, gateway.ClassRead: p, gateway.ClassWrite: p, gateway.ClassStream: p}
}

func newHarness(t testing.TB, o harnessOpts) *harness {
	t.Helper()
	clock := testkit.NewFakeClock(t0)
	fx := identitytest.New(t, clock, identitytest.Options{LimiterFactory: o.limiterFactory})
	be, backends := fakebackend.Start(t, clock, fx.Keys, identitytest.Issuer, fx.Keys)
	plans := gateway.PlanLimits{"free": bigLimits(), "anonymous": bigLimits(),
		"tiny": {gateway.ClassRead: {Rate: 0.001, Burst: 2}, gateway.ClassWrite: {Rate: 0.001, Burst: 2}}}
	srv, err := gateway.New(gateway.Config{
		Identity: fx.Service, Revocations: fx.Revocations, Limiter: fx.Limiter, Backends: backends,
		Clock: clock, IDs: core.NewSplitMix64(1234), Issuer: identitytest.Issuer, Plans: plans,
		KeepAlive: o.keepAlive, BodyLimit: 64 << 10,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, clock: clock, fx: fx, be: be, srv: srv, h: srv.Handler(), tenants: map[string]*tenant{}}
	for _, name := range []string{"t1", "t2"} {
		h.seed(name, "free")
	}
	h.seed("t3", "tiny")
	return h
}

func (h *harness) seed(name, plan string) {
	tid, accts := h.fx.SeedTenantPlan(h.t, name, plan)
	tk := &tenant{id: tid, accts: accts, tokens: map[auth.Role]string{}}
	for r, a := range accts {
		tk.tokens[r] = h.fx.AccessToken(h.t, a)
	}
	h.tenants[name] = tk
}

// request is one HTTP request in tests.
type request struct {
	method, path string
	auth         string // "t1:admin" bearer, "t1:admin:cookie", "bearer <token>", "" none
	headers      map[string]string
	cookies      map[string]string
	body         any // string = raw, otherwise JSON-encoded; nil = no body
	ip           string
}

func (h *harness) build(r request) *http.Request {
	var body io.Reader
	switch b := r.body.(type) {
	case nil:
	case string:
		body = strings.NewReader(b)
	case []byte:
		body = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			h.t.Fatal(err)
		}
		body = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(r.method, r.path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.ip != "" {
		req.RemoteAddr = r.ip + ":4242"
	}
	switch {
	case strings.HasPrefix(r.auth, "bearer "):
		req.Header.Set("Authorization", "Bearer "+strings.TrimPrefix(r.auth, "bearer "))
	case r.auth != "":
		parts := strings.Split(r.auth, ":")
		tn := h.tenants[parts[0]]
		if tn == nil || len(parts) < 2 {
			h.t.Fatalf("bad auth %q", r.auth)
		}
		tok := tn.tokens[auth.Role(parts[1])]
		if len(parts) == 3 && parts[2] == "cookie" {
			req.AddCookie(&http.Cookie{Name: gateway.CookieAccess, Value: tok})
			req.AddCookie(&http.Cookie{Name: gateway.CookieCSRF, Value: csrfToken})
		} else {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	for k, v := range r.headers {
		req.Header.Set(k, v)
	}
	for k, v := range r.cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	return req
}

func (h *harness) do(r request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, h.build(r))
	return rec
}

// doJSON performs r and decodes the JSON body.
func (h *harness) doJSON(r request) (int, map[string]any, http.Header) {
	rec := h.do(r)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m, rec.Header()
}

func errCode(m map[string]any) string {
	e, _ := m["error"].(map[string]any)
	s, _ := e["code"].(string)
	return s
}

// createProgram creates a program in tenant (as its admin) and returns its id.
func (h *harness) createProgram(tn string) string {
	h.t.Helper()
	code, m, _ := h.doJSON(request{method: "POST", path: "/v1/programs", auth: tn + ":admin", body: map[string]any{
		"name": "Acme", "platform": "hackerone",
		"scope": map[string]any{"rules": []any{map[string]any{"id": "r1", "effect": "allow", "kind": "wildcard_domain", "value": "*.acme.com"}}},
	}})
	if code != 201 {
		h.t.Fatalf("create program: %d %v", code, m)
	}
	return m["id"].(string)
}
