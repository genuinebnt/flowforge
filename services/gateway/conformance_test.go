package gateway_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// Transcript is one conformance/api/*.json file (spec 19 api/contract):
// HTTP requests with the expected status, headers and JSON shape. Each
// file runs against a fresh gateway wired to in-memory identity and fake
// gRPC backends, with tenants t1 and t2 (plan free) and t3 (plan tiny),
// each with one account per role.
//
// Request "auth": "t1:analyst" (bearer access token), "t1:analyst:cookie"
// (ff_at and ff_csrf cookies; the ff_csrf value is "test-csrf-token"),
// "bearer {{var}}" (a captured token), or empty.
//
// Shapes: an expected object lists required keys (extra keys allowed
// unless "$exact": true; "$absent": [keys] forbids keys); a string "<type>" matches a JSON type: string,
// nonempty, int, number, bool, null, array, object, any, timestamp, or
// id:<prefix>; alternatives with "|" ("<timestamp|null>"); any other
// value must equal the actual one after {{var}} substitution. Arrays match
// element by element; {"$each": shape, "$min": n, "$max": n, "$len": n}
// matches every element of an array.
//
// "capture" stores values for later steps: a dotted body path
// ("items.0.id"), "header:<Name>", or "email:<kind>:<address>" (the token
// of the newest e-mail of that kind).
type Transcript struct {
	Description string `json:"description"`
	Steps       []Step `json:"steps"`
}

// Step is one request and its expectations.
type Step struct {
	Name    string            `json:"name"`
	Request StepRequest       `json:"request"`
	Expect  StepExpect        `json:"expect"`
	Capture map[string]string `json:"capture"`
	// AdvanceSeconds moves the fake clock before the request.
	AdvanceSeconds int `json:"advance_seconds"`
}

// StepRequest is the request of a step.
type StepRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Auth    string            `json:"auth"`
	Headers map[string]string `json:"headers"`
	Cookies map[string]string `json:"cookies"`
	Body    json.RawMessage   `json:"body"`
	Raw     *string           `json:"raw"`
	IP      string            `json:"ip"`
}

// StepExpect is what the response must look like.
type StepExpect struct {
	Status         int               `json:"status"`
	Headers        map[string]string `json:"headers"`
	HeadersPresent []string          `json:"headers_present"`
	HeadersAbsent  []string          `json:"headers_absent"`
	Body           json.RawMessage   `json:"body"`
	Cookies        []string          `json:"set_cookies"`
}

// Headers every response must carry (spec 12.1, 13.5).
var universalHeaders = []string{"X-Request-Id", "Content-Security-Policy"}

func TestGatewayConformanceAPI(t *testing.T) {
	cases := testkit.LoadCases[Transcript](t, "api")
	steps := 0
	for _, cf := range cases {
		t.Run(strings.TrimSuffix(cf.Name, ".json"), func(t *testing.T) {
			if len(cf.Data.Steps) == 0 {
				t.Fatal("transcript has no steps")
			}
			h := newHarness(t, harnessOpts{})
			vars := map[string]string{}
			for name, tn := range h.tenants {
				vars[name] = core.PublicID(core.PrefixTenant, tn.id)
				for r, a := range tn.accts {
					vars[name+"_"+string(r)] = core.PublicID(core.PrefixUser, a.UserID)
					vars[name+"_"+string(r)+"_email"] = a.Email
				}
			}
			vars["password"] = "seeded account password 1"
			for i, st := range cf.Data.Steps {
				steps++
				runStep(t, h, vars, i, st)
			}
		})
	}
	t.Logf("%d transcripts, %d steps", len(cases), steps)
}

var varRE = regexp.MustCompile(`\{\{([A-Za-z0-9_.:@\-]+)\}\}`)

func subst(t *testing.T, vars map[string]string, s string) string {
	return varRE.ReplaceAllStringFunc(s, func(m string) string {
		k := m[2 : len(m)-2]
		v, ok := vars[k]
		if !ok {
			t.Fatalf("undefined variable %s", m)
		}
		return v
	})
}

func runStep(t *testing.T, h *harness, vars map[string]string, i int, st Step) {
	t.Helper()
	label := fmt.Sprintf("step %d (%s %s %s)", i, st.Name, st.Request.Method, st.Request.Path)
	if st.AdvanceSeconds > 0 {
		h.clock.Advance(time.Duration(st.AdvanceSeconds) * time.Second)
	}
	r := request{method: st.Request.Method, path: subst(t, vars, st.Request.Path), auth: subst(t, vars, st.Request.Auth),
		headers: map[string]string{}, cookies: map[string]string{}, ip: st.Request.IP}
	for k, v := range st.Request.Headers {
		r.headers[k] = subst(t, vars, v)
	}
	for k, v := range st.Request.Cookies {
		r.cookies[k] = subst(t, vars, v)
	}
	switch {
	case st.Request.Raw != nil:
		r.body = subst(t, vars, *st.Request.Raw)
	case len(st.Request.Body) > 0:
		r.body = subst(t, vars, string(st.Request.Body))
	}
	rec := h.do(r)
	body := rec.Body.Bytes()
	if rec.Code != st.Expect.Status {
		t.Fatalf("%s: status %d, want %d; body %s", label, rec.Code, st.Expect.Status, body)
	}
	hdr := rec.Header()
	var actual any
	if len(body) > 0 && strings.Contains(hdr.Get("Content-Type"), "json") {
		if err := json.Unmarshal(body, &actual); err != nil {
			t.Fatalf("%s: response is not JSON: %v: %s", label, err, body)
		}
		if e, ok := actual.(map[string]any)["error"].(map[string]any); ok {
			checkErrorModel(t, label, e, rec.Code, hdr)
		}
	}
	for name, src := range st.Capture {
		switch {
		case strings.HasPrefix(src, "header:"):
			vars[name] = hdr.Get(strings.TrimPrefix(src, "header:"))
		case strings.HasPrefix(src, "email:"):
			parts := strings.SplitN(src, ":", 3)
			m, ok := h.fx.Email.Last(subst(t, vars, parts[2]), parts[1])
			if !ok {
				t.Fatalf("%s: no %s e-mail to %s", label, parts[1], parts[2])
			}
			vars[name] = m.Token
		case strings.HasPrefix(src, "cookie:"):
			want := strings.TrimPrefix(src, "cookie:")
			for _, ck := range rec.Result().Cookies() {
				if ck.Name == want {
					vars[name] = ck.Value
				}
			}
		default:
			v, ok := lookup(actual, src)
			if !ok {
				t.Fatalf("%s: capture %s: path %q not in body %s", label, name, src, body)
			}
			vars[name] = fmt.Sprint(v)
		}
	}

	for _, k := range append(append([]string{}, universalHeaders...), st.Expect.HeadersPresent...) {
		if hdr.Get(k) == "" {
			t.Errorf("%s: header %s missing", label, k)
		}
	}
	for _, k := range st.Expect.HeadersAbsent {
		if hdr.Get(k) != "" {
			t.Errorf("%s: header %s present (%q)", label, k, hdr.Get(k))
		}
	}
	for k, want := range st.Expect.Headers {
		want = subst(t, vars, want)
		got := hdr.Get(k)
		if err := matchScalar(want, got); err != nil {
			t.Errorf("%s: header %s: %v", label, k, err)
		}
	}
	for _, c := range st.Expect.Cookies {
		found := false
		for _, sc := range hdr.Values("Set-Cookie") {
			found = found || strings.HasPrefix(sc, c+"=")
		}
		if !found {
			t.Errorf("%s: no Set-Cookie %s (got %v)", label, c, hdr.Values("Set-Cookie"))
		}
	}
	if len(st.Expect.Body) > 0 {
		var want any
		if err := json.Unmarshal([]byte(subst(t, vars, string(st.Expect.Body))), &want); err != nil {
			t.Fatalf("%s: bad expected body: %v", label, err)
		}
		if err := match("$", want, actual); err != nil {
			t.Errorf("%s: body: %v\n%s", label, err, body)
		}
	}
}

// checkErrorModel enforces spec 12.2 on every error body.
func checkErrorModel(t *testing.T, label string, e map[string]any, status int, hdr http.Header) {
	t.Helper()
	code, _ := e["code"].(string)
	if code == "" || code != strings.ToLower(code) {
		t.Errorf("%s: error.code %q is not a lower-cased reason", label, code)
	}
	if s, _ := e["status"].(float64); int(s) != status {
		t.Errorf("%s: error.status %v != HTTP status %d", label, e["status"], status)
	}
	if _, ok := e["details"].([]any); !ok {
		t.Errorf("%s: error.details is not an array", label)
	}
	// A replay returns the original body (spec 12.1), so its request_id
	// is the original request's.
	replay := hdr.Get("Idempotent-Replayed") == "true"
	if rid, _ := e["request_id"].(string); rid == "" || (!replay && rid != hdr.Get("X-Request-Id")) {
		t.Errorf("%s: error.request_id %q != X-Request-Id %q", label, rid, hdr.Get("X-Request-Id"))
	}
	if status == 429 && hdr.Get("Retry-After") == "" {
		t.Errorf("%s: 429 without Retry-After", label)
	}
}

func lookup(v any, path string) (any, bool) {
	for _, p := range strings.Split(path, ".") {
		switch x := v.(type) {
		case map[string]any:
			var ok bool
			if v, ok = x[p]; !ok {
				return nil, false
			}
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			v = x[i]
		default:
			return nil, false
		}
	}
	return v, true
}

var tsRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$`)

func isPlaceholder(s string) bool { return strings.HasPrefix(s, "<") && strings.HasSuffix(s, ">") }

func matchType(typ string, v any) bool {
	switch {
	case typ == "any":
		return true
	case typ == "string":
		_, ok := v.(string)
		return ok
	case typ == "nonempty":
		s, ok := v.(string)
		return ok && s != ""
	case typ == "int":
		f, ok := v.(float64)
		return ok && f == float64(int64(f))
	case typ == "number":
		_, ok := v.(float64)
		return ok
	case typ == "bool":
		_, ok := v.(bool)
		return ok
	case typ == "null":
		return v == nil
	case typ == "array":
		_, ok := v.([]any)
		return ok
	case typ == "object":
		_, ok := v.(map[string]any)
		return ok
	case typ == "timestamp":
		s, ok := v.(string)
		return ok && tsRE.MatchString(s)
	case strings.HasPrefix(typ, "id:"):
		s, ok := v.(string)
		return ok && strings.HasPrefix(s, strings.TrimPrefix(typ, "id:")+"_") && len(s) > len(typ)
	}
	return false
}

func matchPlaceholder(p string, v any) bool {
	for _, alt := range strings.Split(p[1:len(p)-1], "|") {
		if matchType(alt, v) {
			return true
		}
	}
	return false
}

// matchScalar matches a header value.
func matchScalar(want, got string) error {
	if isPlaceholder(want) {
		var v any = got
		if n, err := strconv.ParseFloat(got, 64); err == nil {
			v = n
		}
		if got == "" || !matchPlaceholder(want, v) {
			return fmt.Errorf("%q does not match %s", got, want)
		}
		return nil
	}
	if want != got {
		return fmt.Errorf("got %q, want %q", got, want)
	}
	return nil
}

func match(path string, want, got any) error {
	switch w := want.(type) {
	case string:
		if isPlaceholder(w) {
			if !matchPlaceholder(w, got) {
				return fmt.Errorf("%s: %v does not match %s", path, got, w)
			}
			return nil
		}
	case map[string]any:
		if each, ok := w["$each"]; ok {
			arr, ok := got.([]any)
			if !ok {
				return fmt.Errorf("%s: want array, got %T", path, got)
			}
			if n, ok := w["$len"].(float64); ok && len(arr) != int(n) {
				return fmt.Errorf("%s: length %d, want %v", path, len(arr), n)
			}
			if n, ok := w["$min"].(float64); ok && len(arr) < int(n) {
				return fmt.Errorf("%s: length %d, want at least %v", path, len(arr), n)
			}
			if n, ok := w["$max"].(float64); ok && len(arr) > int(n) {
				return fmt.Errorf("%s: length %d, want at most %v", path, len(arr), n)
			}
			for i, x := range arr {
				if err := match(fmt.Sprintf("%s[%d]", path, i), each, x); err != nil {
					return err
				}
			}
			return nil
		}
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: want object, got %T (%v)", path, got, got)
		}
		keys := make([]string, 0, len(w))
		for k := range w {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if k == "$exact" || k == "$absent" {
				continue
			}
			gv, ok := g[k]
			if !ok {
				return fmt.Errorf("%s: missing key %q", path, k)
			}
			if err := match(path+"."+k, w[k], gv); err != nil {
				return err
			}
		}
		if abs, ok := w["$absent"].([]any); ok {
			for _, k := range abs {
				if _, has := g[fmt.Sprint(k)]; has {
					return fmt.Errorf("%s: key %q must be absent", path, k)
				}
			}
		}
		if exact, _ := w["$exact"].(bool); exact {
			for k := range g {
				if _, ok := w[k]; !ok {
					return fmt.Errorf("%s: unexpected key %q", path, k)
				}
			}
		}
		return nil
	case []any:
		g, ok := got.([]any)
		if !ok || len(g) != len(w) {
			return fmt.Errorf("%s: want array of %d, got %v", path, len(w), got)
		}
		for i := range w {
			if err := match(fmt.Sprintf("%s[%d]", path, i), w[i], g[i]); err != nil {
				return err
			}
		}
		return nil
	}
	wb, _ := json.Marshal(want)
	gb, _ := json.Marshal(got)
	if string(wb) != string(gb) {
		return fmt.Errorf("%s: got %s, want %s", path, gb, wb)
	}
	return nil
}

// TestGatewayConformanceMatcher pins the shape language so a permissive
// matcher cannot make the transcripts pass vacuously.
func TestGatewayConformanceMatcher(t *testing.T) {
	dec := func(s string) any {
		var v any
		if err := json.Unmarshal([]byte(s), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range []struct {
		want, got string
		ok        bool
	}{
		{`{"a":"<int>"}`, `{"a":1,"b":2}`, true},
		{`{"a":"<int>"}`, `{"a":"1"}`, false},
		{`{"a":"<int>"}`, `{"b":1}`, false},
		{`{"a":1,"$exact":true}`, `{"a":1,"b":2}`, false},
		{`{"$absent":["k"]}`, `{"k":null}`, false},
		{`{"a":"<timestamp|null>"}`, `{"a":null}`, true},
		{`{"a":"<timestamp>"}`, `{"a":"yesterday"}`, false},
		{`{"a":"<id:run>"}`, `{"a":"prg_1"}`, false},
		{`{"a":{"$each":"<string>","$len":2}}`, `{"a":["x"]}`, false},
		{`{"a":{"$each":"<string>"}}`, `{"a":["x",1]}`, false},
		{`[1,2]`, `[1,2,3]`, false},
		{`{"a":"lit"}`, `{"a":"other"}`, false},
		{`{"a":null}`, `{"a":0}`, false},
	} {
		err := match("$", dec(c.want), dec(c.got))
		if (err == nil) != c.ok {
			t.Errorf("match(%s, %s) = %v, want ok=%v", c.want, c.got, err, c.ok)
		}
	}
}
