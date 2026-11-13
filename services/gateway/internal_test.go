package gateway

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/protoadapt"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/testkit"
)

func grpcErr(code codes.Code, reason string, details ...protoadapt.MessageV1) error {
	st := status.New(code, "upstream says no")
	all := append([]protoadapt.MessageV1{protoadapt.MessageV1Of(&errdetails.ErrorInfo{Reason: reason, Metadata: map[string]string{"rule_id": "r5", "scope_version": "7"}})}, details...)
	st, _ = st.WithDetails(all...)
	return st.Err()
}

// TestGatewayErrorMapping pins the spec 11.8 table.
func TestGatewayErrorMapping(t *testing.T) {
	for _, c := range []struct {
		code   codes.Code
		reason string
		status int
	}{
		{codes.InvalidArgument, core.ReasonValidationFailed, 400},
		{codes.InvalidArgument, core.ReasonInvalidTarget, 400},
		{codes.InvalidArgument, core.ReasonDSLDiagnostics, 422},
		{codes.Unauthenticated, core.ReasonTokenExpired, 401},
		{codes.PermissionDenied, core.ReasonScopeDenied, 403},
		{codes.PermissionDenied, core.ReasonSelfApprovalForbidden, 403},
		{codes.PermissionDenied, core.ReasonKillSwitchActive, 403},
		{codes.NotFound, core.ReasonNotFound, 404},
		{codes.AlreadyExists, core.ReasonIdempotencyKeyReused, 409},
		{codes.Aborted, core.ReasonVersionConflict, 409},
		{codes.Aborted, core.ReasonFenceLost, 409},
		{codes.Aborted, core.ReasonApprovalAlreadyDecided, 409},
		{codes.FailedPrecondition, core.ReasonRunNotCancellable, 409},
		{codes.FailedPrecondition, core.ReasonOutsideWindow, 409},
		{codes.ResourceExhausted, core.ReasonQuotaExceeded, 429},
		{codes.Unavailable, core.ReasonUnavailable, 503},
		{codes.DeadlineExceeded, core.ReasonDeadlineExceeded, 504},
	} {
		e := toAPIError(grpcErr(c.code, c.reason))
		if e.Status != c.status || e.Reason != c.reason {
			t.Errorf("%v/%s: got %d %s, want %d", c.code, c.reason, e.Status, e.Reason, c.status)
		}
		b := e.body("req_1")
		if b.Error.Code != strings.ToLower(c.reason) {
			t.Errorf("%s: code %q", c.reason, b.Error.Code)
		}
		if c.reason != core.ReasonDSLDiagnostics {
			d := b.Error.Details[0].(map[string]any)
			if d["reason"] != c.reason || d["rule_id"] != "r5" || d["scope_version"] != int64(7) {
				t.Errorf("%s: details %v", c.reason, d)
			}
		}
	}
	// INTERNAL and unknown errors expose nothing.
	for _, err := range []error{status.Error(codes.Internal, "db password is hunter2"), errors.New("boom"), status.Error(codes.Unknown, "x")} {
		e := toAPIError(err)
		if e.Status != 500 || e.Reason != core.ReasonInternal || strings.Contains(e.Message, "hunter2") || len(e.body("r").Error.Details) != 0 {
			t.Errorf("%v: %+v", err, e)
		}
	}
	// RetryInfo becomes Retry-After.
	e := toAPIError(grpcErr(codes.ResourceExhausted, core.ReasonRateLimited, protoadapt.MessageV1Of(&errdetails.RetryInfo{RetryDelay: durationpb.New(1500 * time.Millisecond)})))
	if e.Status != 429 || e.RetryAfter != 1500*time.Millisecond {
		t.Fatalf("retry: %+v", e)
	}
	// Diagnostics ride in details for DSL_DIAGNOSTICS.
	e = toAPIError(grpcErr(codes.InvalidArgument, core.ReasonDSLDiagnostics, protoadapt.MessageV1Of(&pb.Diagnostic{Code: "E0301", Severity: pb.Diagnostic_SEVERITY_ERROR})))
	if len(e.Details) != 1 || e.Details[0].(map[string]any)["code"] != "E0301" || e.Details[0].(map[string]any)["severity"] != "error" {
		t.Fatalf("diagnostics: %+v", e.Details)
	}
	// A status without ErrorInfo gets the code's default reason.
	if e := toAPIError(status.Error(codes.NotFound, "x")); e.Reason != core.ReasonNotFound || e.Status != 404 {
		t.Fatalf("default reason: %+v", e)
	}
	// Identity errors (*core.Error) map through the same table.
	if e := toAPIError(&core.Error{Kind: core.KindResourceExhausted, Reason: core.ReasonRateLimited, RetryAfter: time.Minute}); e.Status != 429 || e.RetryAfter != time.Minute {
		t.Fatalf("core error: %+v", e)
	}
	if e := toAPIError(&http.MaxBytesError{Limit: 10}); e.Status != 413 {
		t.Fatalf("max bytes: %+v", e)
	}
}

func TestGatewayRewriteVerb(t *testing.T) {
	for in, want := range map[string]string{
		"/v1/runs/run_1:cancel":                 "/v1/runs/run_1/:cancel",
		"/v1/workflows:check":                   "/v1/workflows/:check",
		"/v1/programs/prg_1/scope:check":        "/v1/programs/prg_1/scope/:check",
		"/v1/alerts/rules:test":                 "/v1/alerts/rules/:test",
		"/v1/auth/verify-email:resend":          "/v1/auth/verify-email/:resend",
		"/v1/runs/run_1":                        "/v1/runs/run_1",
		"/v1/runs/run_1:":                       "/v1/runs/run_1:",
		"/v1/runs/:x":                           "/v1/runs/:x",
		"/healthz:x":                            "/healthz:x",
		"/v1/approvals/apr_1:decide":            "/v1/approvals/apr_1/:decide",
		"/v1/workflows/recon/versions/1":        "/v1/workflows/recon/versions/1",
		"/v1/programs/prg_1/entities/e:similar": "/v1/programs/prg_1/entities/e/:similar",
	} {
		if got, _ := rewriteVerb(in); got != want {
			t.Errorf("rewriteVerb(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGatewayRouteTableMatchesMatrix checks the static route table against
// conformance/auth/rbac/matrix.json and that every authenticated route
// declares a 13.6 permission (only /me is open to any role).
func TestGatewayRouteTableMatchesMatrix(t *testing.T) {
	var m struct {
		Classes []struct {
			Permission string   `json:"permission"`
			Endpoints  []string `json:"endpoints"`
		} `json:"classes"`
	}
	if err := json.Unmarshal(testkit.ReadFile(t, "auth/rbac", "matrix.json"), &m); err != nil {
		t.Fatal(err)
	}
	table := map[string]route{}
	for _, r := range (&Server{}).routes() {
		table[r.method+" "+r.pattern] = r
	}
	for _, cl := range m.Classes {
		for _, ep := range cl.Endpoints {
			method, path, _ := strings.Cut(ep, " ")
			p, _ := rewriteVerb("/v1" + strings.ReplaceAll(path, "{user_id}", "{user_id}"))
			r, ok := table[method+" "+p]
			if !ok {
				t.Errorf("%s: no route %s %s", ep, method, p)
				continue
			}
			if string(r.perm) != cl.Permission {
				t.Errorf("%s: route permission %q, matrix %q", ep, r.perm, cl.Permission)
			}
		}
	}
	valid := map[auth.Permission]bool{}
	for _, p := range auth.Permissions() {
		valid[p] = true
	}
	for k, r := range table {
		switch {
		case r.public:
		case r.perm == "" && k != "GET /v1/me":
			t.Errorf("%s: authenticated route without a permission", k)
		case r.perm != "" && !valid[r.perm]:
			t.Errorf("%s: unknown permission %q", k, r.perm)
		case r.method != http.MethodGet && r.perm == auth.PermRead && !strings.Contains(k, "similar") && !strings.Contains(k, "paths"):
			t.Errorf("%s: state-changing route with read permission", k)
		}
	}
}

// TestRegress_G_SVC_18_UserAgentUTF8: the gateway cut User-Agent at 256
// bytes without regard to runes and passed invalid UTF-8 through, which
// Postgres refuses in identity.auth_audit.user_agent.
func TestRegress_G_SVC_18_UserAgentUTF8(t *testing.T) {
	for _, ua := range []string{"a\xffb", strings.Repeat("a", 255) + "é" + "tail", strings.Repeat("€", 200), "plain"} {
		r, _ := http.NewRequest(http.MethodGet, "/", nil)
		r.Header.Set("User-Agent", ua)
		got := userAgent(r)
		if !utf8.ValidString(got) || len(got) > 256 || (len(ua) > 256 && len(got) < 250) {
			t.Errorf("userAgent(%q) = %q (%d bytes)", ua, got, len(got))
		}
	}
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("User-Agent", "plain")
	if userAgent(r) != "plain" {
		t.Error("a valid short User-Agent was changed")
	}
}
