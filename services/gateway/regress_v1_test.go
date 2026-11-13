package gateway_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
)

// switchLimiter is an in-process limiter that fails every call while down
// is set (a Redis outage).
type switchLimiter struct {
	inner limiter.ContextLimiter
	down  *atomic.Bool
}

var errLimiterDown = errors.New("test: limiter store unreachable")

func (s switchLimiter) Acquire(ctx context.Context, key string, cost int, now time.Duration) (limiter.Result, error) {
	if s.down.Load() {
		return limiter.Result{}, errLimiterDown
	}
	return s.inner.Acquire(ctx, key, cost, now)
}

func (s switchLimiter) AcquireAll(ctx context.Context, reqs []limiter.Request, now time.Duration) (limiter.Result, error) {
	if s.down.Load() {
		return limiter.Result{}, errLimiterDown
	}
	return s.inner.AcquireAll(ctx, reqs, now)
}

func switchFactory(down *atomic.Bool) identity.LimiterFactory {
	return func(p limiter.Params) (limiter.ContextLimiter, error) {
		in, err := identity.MemLimiterFactory(p)
		return switchLimiter{inner: in, down: down}, err
	}
}

// TestRegress_G_LIM_01_LimiterFailsClosed: when the rate limiter errors,
// API requests are denied with 503 unavailable and Retry-After (spec 2.4,
// 11.8) and never reach a backend; the auth throttles deny too.
func TestRegress_G_LIM_01_LimiterFailsClosed(t *testing.T) {
	var down atomic.Bool
	h := newHarness(t, harnessOpts{limiterFactory: switchFactory(&down)})
	if code, _, _ := h.doJSON(request{method: http.MethodGet, path: "/v1/programs", auth: "t1:admin"}); code != http.StatusOK {
		t.Fatalf("limiter up: %d", code)
	}
	down.Store(true)
	calls := len(h.be.Calls())
	code, body, hdr := h.doJSON(request{method: http.MethodGet, path: "/v1/programs", auth: "t1:admin"})
	if code != http.StatusServiceUnavailable || errCode(body) != "unavailable" || hdr.Get("Retry-After") == "" {
		t.Fatalf("limiter down: %d %v %v", code, body, hdr)
	}
	if len(h.be.Calls()) != calls {
		t.Fatal("a request reached a backend while the limiter was down")
	}
	// Public auth endpoints: the API limit (keyed by IP) and the identity
	// throttles both fail closed.
	for _, r := range []request{
		{method: http.MethodPost, path: "/v1/auth/signup", body: map[string]any{"email": "x@example.com", "password": "correct horse battery staple!", "tenant_name": "x"}},
		{method: http.MethodPost, path: "/v1/auth/login", body: map[string]any{"email": "x@example.com", "password": "correct horse battery staple!"}},
	} {
		if code, body, _ := h.doJSON(r); code != http.StatusServiceUnavailable {
			t.Fatalf("%s with limiter down: %d %v", r.path, code, body)
		}
	}
}

// TestRegress_G_LIM_05_RoleGroupApprovals: an admin puts a member in role
// group finance_ap; a finance_ap approval is then decided by that member
// (the internal token carries groups=[finance_ap] and the engine-side
// check matches it), while a caller with approvals:decide outside the
// group, and the member after removal, are refused 403. Group names that
// are membership roles are rejected, and members of other tenants cannot
// be added.
func TestRegress_G_LIM_05_RoleGroupApprovals(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ids := fixtures(t, h)
	h.be.SetApprovalRole(ids["aid"], "finance_ap")
	approver := core.PublicID(core.PrefixUser, h.tenants["t1"].accts[auth.RoleApprover].UserID)
	member := func(method, name, user string) int {
		code, _, _ := h.doJSON(request{method: method, path: "/v1/tenant/role-groups/" + name + "/members/" + user, auth: "t1:admin"})
		return code
	}
	decide := func(as string) (int, map[string]any) {
		code, body, _ := h.doJSON(request{method: http.MethodPost, path: "/v1/approvals/" + ids["aid"] + ":decide", auth: as,
			body: map[string]any{"decision": "approve"}})
		return code, body
	}

	// Not in the group yet: refused, also for an admin (approvals:decide
	// but neither role nor group matches).
	if code, body := decide("t1:approver"); code != http.StatusForbidden {
		t.Fatalf("non-member approver: %d %v", code, body)
	}
	if code, _ := decide("t1:admin"); code != http.StatusForbidden {
		t.Fatalf("admin outside the group: %d", code)
	}
	if code := member(http.MethodPut, "admin", approver); code != http.StatusBadRequest {
		t.Fatalf("group named like a role: %d", code)
	}
	other := core.PublicID(core.PrefixUser, h.tenants["t2"].accts[auth.RoleApprover].UserID)
	if code := member(http.MethodPut, "finance_ap", other); code != http.StatusNotFound {
		t.Fatalf("member of another tenant: %d", code)
	}
	if code, _, _ := h.doJSON(request{method: http.MethodPut, path: "/v1/tenant/role-groups/finance_ap/members/" + approver, auth: "t1:approver"}); code != http.StatusForbidden {
		t.Fatalf("non-admin manages groups: %d", code)
	}
	if code := member(http.MethodPut, "finance_ap", approver); code != http.StatusNoContent {
		t.Fatalf("add: %d", code)
	}
	if code, body, _ := h.doJSON(request{method: http.MethodGet, path: "/v1/tenant/role-groups", auth: "t1:admin"}); code != http.StatusOK ||
		len(body["items"].([]any)) != 1 || body["items"].([]any)[0].(map[string]any)["name"] != "finance_ap" {
		t.Fatalf("list: %d %v", code, body)
	}
	if _, body, _ := h.doJSON(request{method: http.MethodGet, path: "/v1/tenant/role-groups", auth: "t2:admin"}); len(body["items"].([]any)) != 0 {
		t.Fatalf("t2 sees t1's groups: %v", body)
	}

	// Removal takes effect on the next request.
	if code := member(http.MethodDelete, "finance_ap", approver); code != http.StatusNoContent {
		t.Fatalf("remove: %d", code)
	}
	if code, _ := decide("t1:approver"); code != http.StatusForbidden {
		t.Fatalf("removed member decided: %d", code)
	}
	if code := member(http.MethodDelete, "finance_ap", approver); code != http.StatusNotFound {
		t.Fatalf("remove twice: %d", code)
	}

	member(http.MethodPut, "finance_ap", approver)
	code, body := decide("t1:approver")
	if code != http.StatusOK || body["status"] != "approved" {
		t.Fatalf("group member decides: %d %v", code, body)
	}
	calls := h.be.Calls()
	last := calls[len(calls)-1]
	if !strings.HasSuffix(last.Method, "DecideApproval") || !slices.Equal(last.Groups, []string{"finance_ap"}) {
		t.Fatalf("internal token of the decision: %+v", last)
	}
}
