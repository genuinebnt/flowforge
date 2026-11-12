package gateway_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// fixtures creates one of each resource in t1 and returns their ids.
func fixtures(t *testing.T, h *harness) map[string]string {
	t.Helper()
	ids := map[string]string{"pid": h.createProgram("t1")}
	src := "workflow gated(roots: List<Fqdn>) { approve(\"x\"); }"
	if code, m, _ := h.doJSON(request{method: "POST", path: "/v1/workflows", auth: "t1:analyst", body: map[string]any{"name": "gated", "source": src}}); code != 201 {
		t.Fatalf("fixture workflow: %d %v", code, m)
	}
	_, m, _ := h.doJSON(request{method: "POST", path: "/v1/runs", auth: "t1:analyst", headers: map[string]string{"Idempotency-Key": "fx"},
		body: map[string]any{"workflow": "gated", "program_id": ids["pid"]}})
	ids["rid"] = m["id"].(string)
	_, m, _ = h.doJSON(request{method: "GET", path: "/v1/approvals", auth: "t1:viewer"})
	ids["aid"] = m["items"].([]any)[0].(map[string]any)["id"].(string)
	_, m, _ = h.doJSON(request{method: "GET", path: "/v1/graph/entities?program_id=" + ids["pid"], auth: "t1:viewer"})
	ids["eid"] = m["items"].([]any)[0].(map[string]any)["id"].(string)
	_, m, _ = h.doJSON(request{method: "POST", path: "/v1/alerts/rules", auth: "t1:analyst", body: map[string]any{"name": "r", "source": "alert x when true"}})
	ids["rule"] = m["id"].(string)
	_, m, _ = h.doJSON(request{method: "POST", path: "/v1/alerts/channels", auth: "t1:admin", body: map[string]any{"kind": "slack", "name": "c"}})
	ids["ch"] = m["id"].(string)
	_, m, _ = h.doJSON(request{method: "POST", path: "/v1/api-keys", auth: "t1:admin", body: map[string]any{"name": "k", "role": "viewer"}})
	ids["key"] = m["id"].(string)
	ids["user"] = core.PublicID(core.PrefixUser, h.tenants["t1"].accts[auth.RoleViewer].UserID)
	return ids
}

func fill(path string, ids map[string]string) string {
	for _, kv := range [][2]string{
		{"/programs/{id}", "/programs/" + ids["pid"]}, {"/runs/{id}", "/runs/" + ids["rid"]}, {"/approvals/{id}", "/approvals/" + ids["aid"]},
		{"/alerts/rules/{id}", "/alerts/rules/" + ids["rule"]}, {"/alerts/channels/{id}", "/alerts/channels/" + ids["ch"]},
		{"/api-keys/{id}", "/api-keys/" + ids["key"]}, {"/graph/entities/{id}", "/graph/entities/" + ids["eid"]},
		{"{user_id}", ids["user"]}, {"{pid}", ids["pid"]},
	} {
		path = strings.ReplaceAll(path, kv[0], kv[1])
	}
	return path
}

type rbacMatrix struct {
	Roles   []string `json:"roles"`
	Classes []struct {
		Permission string          `json:"permission"`
		Endpoints  []string        `json:"endpoints"`
		Allowed    map[string]bool `json:"allowed"`
	} `json:"classes"`
}

// TestGatewayRBACMatrix enforces spec 13.6 (auth_004) at the HTTP level:
// every role against every endpoint of every class in
// conformance/auth/rbac/matrix.json. Denied requests are 403
// forbidden_role before any internal service is called; allowed ones get
// past authorization (they fail later on a deliberately invalid body).
func TestGatewayRBACMatrix(t *testing.T) {
	var m rbacMatrix
	if err := json.Unmarshal(testkit.ReadFile(t, "auth/rbac", "matrix.json"), &m); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, harnessOpts{})
	ids := fixtures(t, h)
	checked := 0
	for _, cl := range m.Classes {
		for _, ep := range cl.Endpoints {
			method, path, _ := strings.Cut(ep, " ")
			path = "/v1" + fill(path, ids)
			for _, role := range m.Roles {
				before := len(h.be.Calls())
				var body any
				if method != "GET" && method != "DELETE" {
					body = map[string]any{"__invalid__": true} // never valid: fails after authorization
				}
				code, resp, _ := h.doJSON(request{method: method, path: path, auth: "t1:" + role, body: body, headers: map[string]string{"Idempotency-Key": "rbac-" + role + ep}})
				denied := code == http.StatusForbidden && errCode(resp) == "forbidden_role"
				if denied == cl.Allowed[role] {
					t.Errorf("%s as %s: status %d %v, allowed=%v", ep, role, code, resp, cl.Allowed[role])
				}
				if denied && len(h.be.Calls()) != before {
					t.Errorf("%s as %s: denied request reached an internal service", ep, role)
				}
				checked++
			}
		}
	}
	if checked < 90 {
		t.Fatalf("only %d role x endpoint checks", checked)
	}
}

// TestGatewayTenantIsolation proves no cross-tenant read for every get and
// list endpoint (spec 18.2): t2 sees 404 for t1's resources and empty
// lists, and a disagreeing tenant_id is 404 (13.3).
func TestGatewayTenantIsolation(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ids := fixtures(t, h)
	gets := []string{
		"/programs/{id}", "/programs/{id}/scope", "/programs/{id}/scope/versions", "/runs/{id}", "/runs/{id}/events",
		"/approvals/{id}", "/graph/entities/{id}", "/graph/entities/{id}/neighbors", "/graph/entities/{id}/similar",
		"/graph/entities?program_id={pid}", "/graph/search?program_id={pid}&q=acme", "/graph/diff?program_id={pid}&since=2020-01-01T00:00:00Z",
		"/graph/snapshots?program_id={pid}", "/programs/{pid}/graph/snapshots", "/programs/{pid}/entities/" + ids["eid"] + "/similar",
		"/programs/{pid}/similarity/index", "/alerts/rules/{id}", "/tenant/members/{user_id}", "/workflows/gated/versions/1",
	}
	for _, p := range gets {
		path := "/v1" + fill(p, ids)
		if code, _, _ := h.doJSON(request{method: "GET", path: path, auth: "t1:admin"}); code != 200 {
			t.Errorf("t1 %s: %d", path, code)
		}
		code, m, _ := h.doJSON(request{method: "GET", path: path, auth: "t2:admin"})
		if code != 404 || errCode(m) != "not_found" {
			t.Errorf("t2 %s: %d %v", path, code, m)
		}
	}
	lists := []string{"/programs", "/runs", "/approvals", "/workflows", "/alerts/rules", "/alerts/channels", "/alerts/events", "/api-keys", "/tenant/members"}
	t1ids := []string{ids["pid"], ids["rid"], ids["aid"], ids["rule"], ids["ch"], ids["key"], ids["user"], "gated"}
	for _, p := range lists {
		rec := h.do(request{method: "GET", path: "/v1" + p, auth: "t2:admin"})
		if rec.Code != 200 {
			t.Errorf("t2 list %s: %d", p, rec.Code)
		}
		for _, id := range t1ids {
			if strings.Contains(rec.Body.String(), id) {
				t.Errorf("t2 list %s leaks %s", p, id)
			}
		}
		code, m, _ := h.doJSON(request{method: "GET", path: "/v1" + p + "?tenant_id=" + core.PublicID(core.PrefixTenant, h.tenants["t1"].id), auth: "t2:admin"})
		if code != 404 || errCode(m) != "not_found" {
			t.Errorf("t2 list %s with t1's tenant_id: %d", p, code)
		}
	}
	// Writes to another tenant's resources are 404 too.
	for _, w := range []request{
		{method: "PUT", path: "/v1" + fill("/programs/{id}/scope", ids), body: map[string]any{"base_version": 1, "document": map[string]any{"rules": []any{}}}},
		{method: "POST", path: "/v1" + fill("/runs/{id}:cancel", ids)},
		{method: "DELETE", path: "/v1" + fill("/alerts/rules/{id}", ids)},
		{method: "DELETE", path: "/v1" + fill("/api-keys/{id}", ids)},
		{method: "PATCH", path: "/v1" + fill("/tenant/members/{user_id}", ids), body: map[string]any{"role": "admin"}},
	} {
		w.auth = "t2:owner"
		if code, m, _ := h.doJSON(w); code != 404 {
			t.Errorf("t2 %s %s: %d %v", w.method, w.path, code, m)
		}
	}
}

// TestGatewayAuth005NoEnumeration: login for an unknown and a known e-mail
// gives the same status and body (apart from request_id), costs the same
// password hashing, and both are throttled identically (spec 13.1,
// auth_005).
func TestGatewayAuth005NoEnumeration(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	known := h.tenants["t1"].accts[auth.RoleViewer].Email
	norm := func(b string) string {
		var m map[string]any
		_ = json.Unmarshal([]byte(b), &m)
		delete(m["error"].(map[string]any), "request_id")
		out, _ := json.Marshal(m)
		return string(out)
	}
	type result struct {
		code     int
		body     string
		verifies int
		elapsed  time.Duration
	}
	try := func(email, ip string) result {
		h.fx.Hasher.Verifies()
		start := time.Now()
		rec := h.do(request{method: "POST", path: "/v1/auth/login", ip: ip, body: map[string]any{"email": email, "password": "definitely wrong pw"}})
		return result{rec.Code, norm(rec.Body.String()), h.fx.Hasher.Verifies(), time.Since(start)}
	}
	k, u := try(known, "203.0.113.1"), try("nobody@t1.test", "203.0.113.2")
	if k.code != 401 || k.code != u.code || k.body != u.body {
		t.Fatalf("known %d %s vs unknown %d %s", k.code, k.body, u.code, u.body)
	}
	if k.verifies != 1 || u.verifies != 1 {
		t.Fatalf("password verifications known=%d unknown=%d, want 1 each (same timing class)", k.verifies, u.verifies)
	}
	// Signup with an existing vs a new e-mail: same status and shape.
	s1 := h.do(request{method: "POST", path: "/v1/auth/signup", ip: "203.0.113.3", body: map[string]any{"email": known, "password": "long enough password", "tenant_name": "x"}})
	s2 := h.do(request{method: "POST", path: "/v1/auth/signup", ip: "203.0.113.3", body: map[string]any{"email": "fresh@t1.test", "password": "long enough password", "tenant_name": "x"}})
	keys := func(b []byte) string {
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		var ks []string
		for k, v := range m {
			if s, ok := v.(string); ok && (strings.HasPrefix(s, "usr_") || strings.HasPrefix(s, "ten_")) {
				v = "<id>"
			}
			b, _ := json.Marshal(v)
			ks = append(ks, k+"="+string(b))
		}
		return strings.Join(sortStrings(ks), ",")
	}
	if s1.Code != 202 || s2.Code != 202 || keys(s1.Body.Bytes()) != keys(s2.Body.Bytes()) {
		t.Fatalf("signup existing %d %s vs new %d %s", s1.Code, s1.Body, s2.Code, s2.Body)
	}
	// After 10 failures both accounts are throttled with the same answer.
	var lastK, lastU result
	for i := 0; i < 10; i++ {
		ip := "198.51.100." + string(rune('a'+i))
		lastK, lastU = try(known, ip+"k"), try("nobody@t1.test", ip+"u")
	}
	if lastK.code != 429 || lastU.code != 429 || lastK.body != lastU.body {
		t.Fatalf("throttled known %d %s vs unknown %d %s", lastK.code, lastK.body, lastU.code, lastU.body)
	}
	// Even the right password is refused during the cool-down.
	rec := h.do(request{method: "POST", path: "/v1/auth/login", ip: "192.0.2.200", body: map[string]any{"email": known, "password": h.tenants["t1"].accts[auth.RoleViewer].Password}})
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("correct password during cool-down: %d", rec.Code)
	}
}

func sortStrings(s []string) []string {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	return s
}

// TestGatewayRefreshCookieFlow checks the console cookie attributes of
// spec 13.2/13.5 and rotation through ff_rt.
func TestGatewayRefreshCookieFlow(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	a := h.tenants["t1"].accts[auth.RoleAnalyst]
	rec := h.do(request{method: "POST", path: "/v1/auth/login", body: map[string]any{"email": a.Email, "password": a.Password, "use_cookies": true}})
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	cookies := map[string]*http.Cookie{}
	for _, c := range rec.Result().Cookies() {
		cookies[c.Name] = c
	}
	at, rt, csrf := cookies["ff_at"], cookies["ff_rt"], cookies["ff_csrf"]
	switch {
	case at == nil || !at.HttpOnly || !at.Secure || at.SameSite != http.SameSiteLaxMode || at.Path != "/" || at.MaxAge != 600:
		t.Fatalf("ff_at: %+v", at)
	case rt == nil || !rt.HttpOnly || !rt.Secure || rt.SameSite != http.SameSiteStrictMode || rt.Path != "/v1/auth" || rt.MaxAge != 2592000:
		t.Fatalf("ff_rt: %+v", rt)
	case csrf == nil || csrf.HttpOnly || !csrf.Secure || csrf.Value == "":
		t.Fatalf("ff_csrf: %+v", csrf)
	}
	if strings.Contains(rec.Body.String(), rt.Value) {
		t.Fatal("refresh token in the body of a cookie login")
	}
	rec = h.do(request{method: "POST", path: "/v1/auth/refresh", cookies: map[string]string{"ff_rt": rt.Value, "ff_csrf": csrf.Value},
		headers: map[string]string{"X-CSRF-Token": csrf.Value}})
	if rec.Code != 200 {
		t.Fatalf("cookie refresh: %d %s", rec.Code, rec.Body)
	}
	var newRT string
	for _, c := range rec.Result().Cookies() {
		if c.Name == "ff_rt" {
			newRT = c.Value
		}
	}
	if newRT == "" || newRT == rt.Value {
		t.Fatal("refresh did not rotate ff_rt")
	}
	// Reusing the old cookie revokes the family and clears cookies.
	rec = h.do(request{method: "POST", path: "/v1/auth/refresh", cookies: map[string]string{"ff_rt": rt.Value, "ff_csrf": csrf.Value},
		headers: map[string]string{"X-CSRF-Token": csrf.Value}})
	if rec.Code != 401 || !strings.Contains(strings.Join(rec.Header().Values("Set-Cookie"), ";"), "ff_rt=;") {
		t.Fatalf("reuse: %d %v", rec.Code, rec.Header().Values("Set-Cookie"))
	}
	rec = h.do(request{method: "POST", path: "/v1/auth/refresh", body: map[string]any{"refresh_token": newRT}})
	if rec.Code != 401 {
		t.Fatalf("family member after reuse: %d", rec.Code)
	}
}

// TestGatewayAPIKeyFlow: a key authenticates with its own role and tenant,
// and the internal call carries apikey:<id> as the actor.
func TestGatewayAPIKeyFlow(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	pid := h.createProgram("t1")
	_, m, _ := h.doJSON(request{method: "POST", path: "/v1/api-keys", auth: "t1:admin", body: map[string]any{"name": "ci", "role": "analyst"}})
	key, kid := m["key"].(string), m["id"].(string)
	if !strings.HasPrefix(key, "ffk_") {
		t.Fatalf("key %q", key)
	}
	code, _, _ := h.doJSON(request{method: "GET", path: "/v1/programs/" + pid, auth: "bearer " + key})
	if code != 200 {
		t.Fatalf("read with key: %d", code)
	}
	calls := h.be.Calls()
	last := calls[len(calls)-1]
	if last.Actor != "apikey:"+kid || last.Role != "analyst" || last.Tenant != core.PublicID(core.PrefixTenant, h.tenants["t1"].id) {
		t.Fatalf("internal call: %+v", last)
	}
	// An API key in a cookie is refused.
	rec := h.do(request{method: "GET", path: "/v1/me", cookies: map[string]string{"ff_at": key}})
	if rec.Code != 401 {
		t.Fatalf("key as cookie: %d", rec.Code)
	}
}

// TestGatewayInternalMetadata: every internal call carries the spec 11.1
// metadata derived from the token and the request.
func TestGatewayInternalMetadata(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	pid := h.createProgram("t1")
	h.doJSON(request{method: "POST", path: "/v1/workflows", auth: "t1:analyst", body: map[string]any{"name": "w", "source": "workflow w() {}"}})
	tp := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	rec := h.do(request{method: "POST", path: "/v1/runs", auth: "t1:analyst", body: map[string]any{"workflow": "w", "program_id": pid},
		headers: map[string]string{"Idempotency-Key": "idem-77", "X-Request-Id": "req-abc", "traceparent": tp}})
	if rec.Code != 202 {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body)
	}
	var submit *struct{ m, tn, a, rid, tp, idem string }
	for _, c := range h.be.Calls() {
		if strings.HasSuffix(c.Method, "/SubmitRun") {
			submit = &struct{ m, tn, a, rid, tp, idem string }{c.Method, c.Tenant, c.Actor, c.RequestID, c.Traceparent, c.IdempotencyKey}
		}
	}
	analyst := h.tenants["t1"].accts[auth.RoleAnalyst]
	switch {
	case submit == nil:
		t.Fatal("no SubmitRun call")
	case submit.tn != core.PublicID(core.PrefixTenant, analyst.TenantID), submit.a != "user:"+core.PublicID(core.PrefixUser, analyst.UserID):
		t.Fatalf("tenant/actor %+v", submit)
	case submit.rid != "req-abc", submit.tp != tp, submit.idem != "idem-77":
		t.Fatalf("request id / traceparent / idempotency key %+v", submit)
	}
}

// TestGatewaySSEResume bridges WatchRun to SSE: Last-Event-ID resumes after
// that seq, keep-alive comments are sent, and the stream ends with the run.
func TestGatewaySSEResume(t *testing.T) {
	h := newHarness(t, harnessOpts{keepAlive: 20 * time.Millisecond})
	ids := fixtures(t, h) // run with events 1 (submitted) and 2 (approval_requested)
	srv := httptest.NewServer(h.h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/runs/"+ids["rid"]+"/watch", nil)
	req.Header.Set("Authorization", "Bearer "+h.tenants["t1"].tokens[auth.RoleViewer])
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Last-Event-ID", "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("watch: %d %v", resp.StatusCode, resp.Header)
	}
	sc := bufio.NewScanner(resp.Body)
	var ids2 []string
	keepAlive := false
	emitted := false
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "id: "):
			ids2 = append(ids2, strings.TrimPrefix(line, "id: "))
		case line == ": keep-alive":
			keepAlive = true
			if !emitted {
				emitted = true
				h.be.AppendRunEvent(ids["rid"], "run_paused", map[string]any{"reason": "approval_pending"})
				h.be.FinishRun(ids["rid"])
			}
		case strings.HasPrefix(line, "data: "):
			var ev map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil || ev["type"] == nil {
				t.Fatalf("bad data line %q", line)
			}
		}
	}
	if strings.Join(ids2, ",") != "2,3,4" {
		t.Fatalf("event ids %v, want 2,3,4 (resumed after Last-Event-ID 1)", ids2)
	}
	if !keepAlive {
		t.Fatal("no keep-alive comment")
	}
	// A missing or foreign run is a plain 404 before any stream.
	rec := h.do(request{method: "GET", path: "/v1/runs/" + ids["rid"] + "/watch", auth: "t2:viewer"})
	if rec.Code != 404 {
		t.Fatalf("foreign watch: %d", rec.Code)
	}
	rec = h.do(request{method: "GET", path: "/v1/runs/" + ids["rid"] + "/watch", auth: "t1:viewer", headers: map[string]string{"Last-Event-ID": "x"}})
	if rec.Code != 400 {
		t.Fatalf("bad Last-Event-ID: %d", rec.Code)
	}
}

// TestGatewaySSEGraphChanges resumes the graph change feed.
func TestGatewaySSEGraphChanges(t *testing.T) {
	h := newHarness(t, harnessOpts{keepAlive: time.Hour})
	pid := h.createProgram("t1") // seeds changes 1..3
	srv := httptest.NewServer(h.h)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v1/graph/changes/watch?program_id="+pid, nil)
	req.Header.Set("Authorization", "Bearer "+h.tenants["t1"].tokens[auth.RoleViewer])
	req.Header.Set("Last-Event-ID", "2")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	var got []string
	for sc.Scan() && len(got) < 2 {
		if line := sc.Text(); strings.HasPrefix(line, "id: ") {
			got = append(got, strings.TrimPrefix(line, "id: "))
			if len(got) == 1 {
				h.be.EmitChange(pid, "entity_stale")
			}
		} else if line == "event: graph_change" || line == "" || strings.HasPrefix(line, "data: ") || strings.HasPrefix(line, ":") {
			continue
		}
	}
	if strings.Join(got, ",") != "3,4" {
		t.Fatalf("change ids %v, want 3,4", got)
	}
}

// TestGatewayBodyLimit: oversized bodies are 413 in the error model.
func TestGatewayBodyLimit(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	big := `{"name":"` + strings.Repeat("a", 70<<10) + `"}`
	code, m, _ := h.doJSON(request{method: "POST", path: "/v1/programs", auth: "t1:admin", body: big})
	if code != 413 || errCode(m) != "validation_failed" {
		t.Fatalf("%d %v", code, m)
	}
}
