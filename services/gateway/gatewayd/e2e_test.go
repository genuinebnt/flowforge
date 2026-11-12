package gatewayd_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/gateway/gatewayd"
)

// logBuffer collects log output so the test can read the verification
// e-mail the in-memory sender logs at debug level.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

// TestGatewayDevStackEndToEnd drives the public REST API against the real
// internal services (the wiring of cmd/flowforge dev): signup, e-mail
// verification, login, program with scope, scope check, workflow
// registration, idempotent run submission and completion, graph read. It
// pins the internal-token actor contract between the gateway and the
// services.
func TestGatewayDevStackEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the whole stack")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	logs := &logBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	addr := freeAddr(t)
	issuer := "http://" + addr
	gw, err := gatewayd.New(ctx, gatewayd.Config{Addr: addr, Issuer: issuer, PublicURL: issuer, CookieInsec: true, ShutdownGrace: time.Second}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer gw.Close()
	st, err := devstack.Start(ctx, devstack.Options{Issuer: issuer, TrustKeys: gw.Keys(), Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	backends := map[string]string{
		"FF_SCOPE_ADDR": st.Addrs[devstack.ScopeName], "FF_ENGINE_ADDR": st.Addrs[devstack.EngineName],
		"FF_GRAPH_ADDR": st.Addrs[devstack.GraphName], "FF_NOTIFIER_ADDR": st.Addrs[devstack.NotifierName],
		"FF_TOOLRUNNER_ADDR": st.Addrs[devstack.ToolRunnerName], "FF_SIMILARITY_ADDR": st.Addrs[devstack.SimilarityName],
	}
	served := make(chan error, 1)
	go func() { served <- gw.Serve(ctx, backends) }()
	defer func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve: %v", err)
		}
	}()

	base := "http://" + addr + "/v1"
	c := &client{t: t, base: base}
	c.waitReady("http://" + addr + "/readyz")

	signup := c.do("POST", "/auth/signup", map[string]any{"email": "e2e@example.com", "password": "a long enough passphrase 42", "tenant_name": "E2E"}, 202)
	tenant := signup["tenant_id"].(string)
	token := c.waitFor(func() (string, bool) {
		m := regexp.MustCompile(`token=([A-Za-z0-9_-]+)`).FindStringSubmatch(logs.String())
		if m == nil {
			return "", false
		}
		return m[1], true
	})
	c.do("POST", "/auth/verify-email", map[string]any{"token": token}, 200)
	login := c.do("POST", "/auth/login", map[string]any{"email": "e2e@example.com", "password": "a long enough passphrase 42", "tenant_id": tenant}, 200)
	c.bearer = login["access_token"].(string)

	prog := c.do("POST", "/programs", map[string]any{"name": "Acme", "scope": map[string]any{
		"rules": []any{
			map[string]any{"id": "r1", "effect": "allow", "kind": "wildcard_domain", "value": "*.acme.com"},
			map[string]any{"id": "r2", "effect": "deny", "kind": "domain", "value": "pay.acme.com"},
		},
		"activity": map[string]any{"max_class": "active", "windows": []any{}},
	}}, 201)
	pid := prog["id"].(string)
	dec := c.do("POST", "/programs/"+pid+"/scope:check", map[string]any{"target": "https://PAY.Acme.com:443/login", "kind": "url", "activity_class": "active"}, 200)
	if dec["result"] != "deny" || dec["reason"] != "explicit_deny" || dec["rule_id"] != "r2" || dec["normalized_target"] != "https://pay.acme.com/login" {
		t.Fatalf("scope check: %v", dec)
	}

	src := "workflow hello(names: List<String>) -> String {\n" +
		"  let out = names |> map(n => mock(n));\n" +
		"  let joined = join(out, \",\");\n" +
		"  \"done: {joined}\"\n}\n"
	wf := c.do("POST", "/workflows", map[string]any{"name": "hello", "program_id": pid, "source": src}, 201)
	if wf["status"] != "published" {
		t.Fatalf("register: %v", wf)
	}
	body := map[string]any{"workflow": "hello", "program_id": pid, "input": map[string]any{"names": []any{"a", "b"}}}
	c.idem = "e2e-1"
	run := c.do("POST", "/runs", body, 202)
	replay := c.do("POST", "/runs", body, 202)
	if replay["id"] != run["id"] {
		t.Fatalf("idempotent replay returned another run: %v vs %v", replay["id"], run["id"])
	}
	c.idem = ""
	rid := run["id"].(string)
	if !strings.HasPrefix(run["created_by"].(string), "usr_") {
		t.Fatalf("created_by not a public user id: %v", run["created_by"])
	}
	done := c.waitFor(func() (string, bool) {
		r := c.do("GET", "/runs/"+rid, nil, 200)
		s, _ := r["status"].(string)
		return fmt.Sprint(r["output"]), s == "succeeded"
	})
	if done != "done: a,b" {
		t.Fatalf("output = %q", done)
	}
	ents := c.do("GET", "/graph/entities?program_id="+pid, nil, 200)
	if items, _ := ents["items"].([]any); len(items) == 0 {
		t.Fatalf("no graph entities after the run: %v", ents)
	}
}

type client struct {
	t      *testing.T
	base   string
	bearer string
	idem   string
}

func (c *client) do(method, path string, body any, want int) map[string]any {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			c.t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		c.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.idem != "" && method == "POST" {
		req.Header.Set("Idempotency-Key", c.idem)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != want {
		c.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, raw)
	}
	out := map[string]any{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			c.t.Fatalf("%s %s: %v: %s", method, path, err, raw)
		}
	}
	return out
}

func (c *client) waitReady(url string) {
	c.t.Helper()
	c.waitFor(func() (string, bool) {
		resp, err := http.Get(url)
		if err != nil {
			return "", false
		}
		resp.Body.Close()
		return "", resp.StatusCode == 200
	})
}

// waitFor polls f for up to 15 seconds.
func (c *client) waitFor(f func() (string, bool)) string {
	c.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if v, ok := f(); ok {
			return v
		}
		time.Sleep(50 * time.Millisecond)
	}
	c.t.Fatal("timed out")
	return ""
}
