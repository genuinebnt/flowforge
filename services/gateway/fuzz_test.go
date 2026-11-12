package gateway_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// fuzzEndpoints are the body-taking endpoints; {x} placeholders are filled
// from fixtures. auth is the most privileged role so bodies reach the
// handlers.
var fuzzEndpoints = []struct{ method, path, auth string }{
	{"POST", "/v1/auth/signup", ""}, {"POST", "/v1/auth/verify-email", ""}, {"POST", "/v1/auth/verify-email:resend", ""},
	{"POST", "/v1/auth/login", ""}, {"POST", "/v1/auth/refresh", ""}, {"POST", "/v1/auth/logout", "t1:owner"},
	{"POST", "/v1/auth/password/forgot", ""}, {"POST", "/v1/auth/password/reset", ""}, {"POST", "/v1/auth/invites:accept", ""},
	{"PATCH", "/v1/tenant/members/{user_id}", "t1:owner"}, {"POST", "/v1/tenant/invites", "t1:owner"}, {"POST", "/v1/api-keys", "t1:owner"},
	{"POST", "/v1/programs", "t1:owner"}, {"PATCH", "/v1/programs/{id}", "t1:owner"}, {"PUT", "/v1/programs/{id}/scope", "t1:owner"},
	{"POST", "/v1/programs/{id}/scope:check", "t1:owner"}, {"POST", "/v1/programs/{id}/kill-switch", "t1:owner"},
	{"POST", "/v1/workflows:check", "t1:owner"}, {"POST", "/v1/workflows", "t1:owner"}, {"POST", "/v1/runs", "t1:owner"},
	{"POST", "/v1/runs/{rid}:cancel", "t1:owner"}, {"POST", "/v1/approvals/{aid}:decide", "t1:approver"},
	{"POST", "/v1/graph/similar", "t1:owner"}, {"POST", "/v1/graph/analysis", "t1:owner"}, {"POST", "/v1/graph/paths", "t1:owner"},
	{"POST", "/v1/programs/{pid}/similar", "t1:owner"}, {"POST", "/v1/programs/{pid}/graph/analysis", "t1:owner"},
	{"POST", "/v1/alerts/rules", "t1:owner"}, {"POST", "/v1/alerts/rules:test", "t1:owner"}, {"PUT", "/v1/alerts/rules/{rule}", "t1:owner"},
	{"POST", "/v1/alerts/channels", "t1:owner"}, {"PUT", "/v1/alerts/channels/{ch}", "t1:owner"},
}

// FuzzRESTJSON (spec 25.5 fuzz_rest_json): arbitrary request bodies for
// every body-taking endpoint always yield a defined response below 500,
// never a 5xx or a panic.
func FuzzRESTJSON(f *testing.F) {
	seeds := []string{
		``, `{}`, `[]`, `null`, `"x"`, `{"a":1}`, `{"name":"x","source":"alert x"}`, `{"tenant_id":"ten_x"}`, `{"tenant_id":5}`,
		`{"email":"a@b.co","password":"long enough password","tenant_name":"x"}`, `{"scope":{"rules":[{"id":"r","effect":"allow","kind":"domain","value":"a.b"}]}}`,
		`{"base_version":1,"document":{"rules":[]}}`, `{"workflow":"w","program_id":"prg_1","input":{"a":[1,{"b":null}]}}`,
		`{"input":{"x":1e400}}`, `{"k":4294967296}`, `{"pagerank":{"damping":2}}`, `{"bfs":{"start_entity_id":"ent_1","direction":"up"}}`,
		`{"change":{"at":"never","entity":{"content":{"a":1}}},"source":"alert x"}`, "{\"name\":\"\\u0000\"}", `{"decision":"approve","comment":""}`,
		`{"enabled":true,"reason":"x"}`, `{"target":"https://a.b","kind":"url"}`, `{"role":"owner"}`, `{"expires_at":"2099-01-01T00:00:00Z","name":"k","role":"viewer"}`,
		`{"refresh_token":"ffr_x"}`, `{"token":"` + "A" + `"}`, `{"a":1}{"b":2}`, "\xff\xfe", `{"text":"x","k":0,"mode":"vector"}`,
	}
	for i, s := range seeds {
		f.Add(uint8(i), []byte(s))
	}
	h := newHarness(f, harnessOpts{})
	ids := fixtures(f, h)
	f.Fuzz(func(t *testing.T, which uint8, body []byte) {
		ep := fuzzEndpoints[int(which)%len(fuzzEndpoints)]
		path := fill(ep.path, ids)
		for k, v := range map[string]string{"{rid}": ids["rid"], "{aid}": ids["aid"], "{rule}": ids["rule"], "{ch}": ids["ch"]} {
			path = strings.ReplaceAll(path, k, v)
		}
		rec := h.do(request{method: ep.method, path: path, auth: ep.auth, body: body, headers: map[string]string{"Idempotency-Key": idemKey(which, body)}})
		if rec.Code >= 500 || rec.Code < 200 {
			t.Fatalf("%s %s with %q: status %d: %s", ep.method, path, body, rec.Code, rec.Body)
		}
	})
}

// idemKey is unique per input, so replays never mask the handler.
func idemKey(which uint8, body []byte) string {
	sum := sha256.Sum256(append([]byte{which}, body...))
	return hex.EncodeToString(sum[:16])
}
