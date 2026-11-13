package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
)

// idemMode says how a route treats Idempotency-Key.
type idemMode int

const (
	idemNone     idemMode = iota // header validated, not stored (public routes)
	idemOptional                 // stored when present (every authenticated POST)
	idemRequired                 // POST /runs
)

// route is one endpoint of the table in routes.go.
type route struct {
	method  string
	pattern string // ServeMux pattern path; ":verb" suffixes are written "/:verb"
	public  bool   // no authentication required
	// optionalAuth: a public route that uses valid credentials when given.
	optionalAuth bool
	perm         auth.Permission
	class        string
	idem         idemMode
	stream       bool
	h            func(c *call) error
}

// call carries one request through the pipeline and into a handler.
type call struct {
	s     *Server
	w     http.ResponseWriter
	r     *http.Request
	ctx   context.Context
	rt    *route
	p     *principal
	reqID string
	ip    string
	path  string // original request path (before ":verb" rewriting)
	wrote bool
}

type ctxKey int

const (
	keyReqID ctxKey = iota
	keyOrigPath
	keyOutgoing
)

var reqIDRE = regexp.MustCompile(`^[A-Za-z0-9._\-]{1,128}$`)
var traceparentRE = regexp.MustCompile(`^[0-9a-f]{2}-[0-9a-f]{32}-[0-9a-f]{16}-[0-9a-f]{2}$`)

// outer wraps the mux with request ids, panic recovery, security headers,
// the body limit and ":verb" path rewriting.
func (s *Server) outer(mux http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-Id")
		if !reqIDRE.MatchString(reqID) {
			reqID = core.PublicID(core.PrefixRequest, s.ids.new())
		}
		h := w.Header()
		h.Set("X-Request-Id", reqID)
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Cache-Control", "no-store")
		if !s.cfg.Cookies.Insecure {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.cfg.Log.ErrorContext(r.Context(), "gateway: panic", "request_id", reqID, "panic", v, "stack", string(debug.Stack()))
				writeErrorRaw(w, internalError(), reqID)
			}
		}()
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, s.cfg.BodyLimit)
		}
		orig := r.URL.Path
		if p, ok := rewriteVerb(orig); ok {
			r2 := r.Clone(r.Context())
			r2.URL.Path, r2.URL.RawPath = p, ""
			r = r2
		}
		ctx := context.WithValue(r.Context(), keyReqID, reqID)
		ctx = context.WithValue(ctx, keyOrigPath, orig)
		if rc := http.NewResponseController(w); rc != nil {
			_ = rc.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout)) // socket deadline: wall clock, not the injected clock
		}
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}

// rewriteVerb turns a trailing ":verb" custom method (Google AIP-136, used
// by spec 12.3: /runs/{id}:cancel, /workflows:check, ...) into its own path
// segment so ServeMux patterns can match it: /v1/runs/run_1:cancel ->
// /v1/runs/run_1/:cancel.
func rewriteVerb(p string) (string, bool) {
	if !strings.HasPrefix(p, "/v1/") {
		return p, false
	}
	i := strings.LastIndexByte(p, '/')
	last := p[i+1:]
	j := strings.IndexByte(last, ':')
	if j <= 0 || j == len(last)-1 {
		return p, false
	}
	return p[:i+1] + last[:j] + "/:" + last[j+1:], true
}

func clientIP(r *http.Request, trustXFF bool) string {
	if trustXFF {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if a, err := netip.ParseAddr(strings.TrimSpace(first)); err == nil {
				return a.Unmap().String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if a, err := netip.ParseAddr(host); err == nil {
		return a.Unmap().String()
	}
	return "unknown"
}

// wrap builds the per-route pipeline: authenticate, rate limit, authorize,
// CSRF, tenant cross-check, idempotency, handler.
func (s *Server) wrap(rt *route) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		reqID, _ := ctx.Value(keyReqID).(string)
		orig, _ := ctx.Value(keyOrigPath).(string)
		c := &call{s: s, w: w, r: r, ctx: ctx, rt: rt, reqID: reqID, ip: clientIP(r, s.cfg.TrustForwardedFor), path: orig}
		if err := s.serve(c); err != nil {
			c.fail(err)
		}
	})
}

func (s *Server) serve(c *call) error {
	var authErr error
	if !c.rt.public || (c.rt.optionalAuth && hasCredentials(c.r)) {
		c.p, authErr = s.authenticate(c.ctx, c.r)
	}
	if c.rt.public {
		// Public endpoints ignore missing or bad credentials.
		if authErr != nil {
			c.p = nil
		}
		authErr = nil
	}
	if err := s.rateLimit(c); err != nil {
		return err
	}
	if !c.rt.public {
		if authErr != nil {
			return authErr
		}
		if c.rt.perm != "" && !auth.Can(c.p.Role, c.rt.perm) {
			return errForbiddenRole()
		}
	}
	if c.p != nil && c.p.ViaCookie && auth.CSRFRequired(c.r.Method, false) {
		if err := checkCSRF(c.r); err != nil {
			return err
		}
	}
	if c.p != nil {
		if q := c.r.URL.Query(); q.Has("tenant_id") && !c.p.sameTenant(q.Get("tenant_id")) {
			return errNotFound()
		}
	}
	c.ctx = context.WithValue(c.ctx, keyOutgoing, c.outgoing())
	if c.r.Method == http.MethodPost {
		return s.idempotent(c)
	}
	return c.rt.h(c)
}

func checkCSRF(r *http.Request) error {
	cookie := ""
	if ck, err := r.Cookie(auth.CSRFCookie); err == nil {
		cookie = ck.Value
	}
	if err := auth.CheckCSRF(r.Header.Get(auth.CSRFHeader), cookie); err != nil {
		return newError(http.StatusForbidden, ReasonCSRFFailed, "missing or mismatched %s header", auth.CSRFHeader)
	}
	return nil
}

// ---- responses ---------------------------------------------------------

func writeErrorRaw(w http.ResponseWriter, e *apiError, reqID string) {
	h := w.Header()
	// 429 always carries Retry-After (12.1); other statuses only when the
	// cause named a delay (an unreachable rate limiter's 503, RetryInfo).
	if e.Status == http.StatusTooManyRequests || e.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(identity.RetrySeconds(e.RetryAfter)))
	}
	writeJSONRaw(w, e.Status, e.body(reqID))
}

func writeJSONRaw(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		buf.Reset()
		buf.WriteString(`{"error":{"code":"internal","status":500,"message":"internal error","details":[],"request_id":""}}` + "\n")
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// fail writes err in the error model unless a response was already
// started (then it is only logged).
func (c *call) fail(err error) {
	e := toAPIError(err)
	if e.Status >= 500 {
		c.s.cfg.Log.ErrorContext(c.ctx, "gateway: request failed", "request_id", c.reqID, "method", c.r.Method, "path", c.path, "err", err)
	}
	if c.wrote {
		return
	}
	c.wrote = true
	writeErrorRaw(c.w, e, c.reqID)
}

// json writes a JSON response.
func (c *call) json(status int, v any) error {
	c.wrote = true
	writeJSONRaw(c.w, status, v)
	return nil
}

func (c *call) ok(v any) error { return c.json(http.StatusOK, v) }

func (c *call) noContent() error {
	c.wrote = true
	c.w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- request decoding --------------------------------------------------

// decode reads a JSON object body strictly into v (unknown fields and
// trailing data are rejected). A tenant_id member must equal the caller's
// tenant (else 404, spec 13.3) and is then dropped.
func (c *call) decode(v any) error {
	body, err := readBody(c.r)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return errValidation("request body is required")
	}
	if c.p != nil && bytes.Contains(body, []byte(`tenant_id`)) {
		var top map[string]json.RawMessage
		if err := json.Unmarshal(body, &top); err != nil {
			return errValidation("request body must be a JSON object: %s", jsonErr(err))
		}
		if raw, ok := top["tenant_id"]; ok {
			var tid string
			if err := json.Unmarshal(raw, &tid); err != nil || !c.p.sameTenant(tid) {
				return errNotFound()
			}
			delete(top, "tenant_id")
			if body, err = json.Marshal(top); err != nil {
				return errValidation("request body must be a JSON object")
			}
		}
	}
	if err := core.DecodeStrictBytes(body, v); err != nil {
		return errValidation("invalid request body: %s", jsonErr(err))
	}
	return nil
}

// decodeOptional is decode for endpoints whose body may be empty.
func (c *call) decodeOptional(v any) error {
	body, err := readBody(c.r)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}
	return c.decode(v)
}

type readCloser struct{ *bytes.Reader }

func (readCloser) Close() error { return nil }

func readBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	var buf bytes.Buffer
	_, err := buf.ReadFrom(r.Body)
	var mbe *http.MaxBytesError
	if errors.As(err, &mbe) {
		return nil, err
	}
	if err != nil {
		return nil, errValidation("could not read request body")
	}
	b := buf.Bytes()
	r.Body = readCloser{bytes.NewReader(b)}
	return b, nil
}

func jsonErr(err error) string {
	msg := err.Error()
	msg = strings.TrimPrefix(msg, "json: ")
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

// ---- query helpers -----------------------------------------------------

// page parses page_size (1-500, default 50) and page_token.
func (c *call) page() (int32, string, error) {
	q := c.r.URL.Query()
	size := int32(50)
	if v := q.Get("page_size"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			return 0, "", errValidation("page_size: must be an integer from 1 to 500")
		}
		size = int32(n)
	}
	tok := q.Get("page_token")
	if len(tok) > 2048 {
		return 0, "", errValidation("page_token: too long")
	}
	return size, tok, nil
}

func (c *call) queryInt(name string, def, lo, hi int64) (int64, error) {
	v := c.r.URL.Query().Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < lo || n > hi {
		return 0, errValidation("%s: must be an integer from %d to %d", name, lo, hi)
	}
	return n, nil
}

func (c *call) queryBool(name string) (bool, error) {
	v := c.r.URL.Query().Get(name)
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, errValidation("%s: must be true or false", name)
	}
	return b, nil
}

func (c *call) queryTime(name string) (time.Time, error) {
	v := c.r.URL.Query().Get(name)
	if v == "" {
		return time.Time{}, nil
	}
	t, err := core.ParseTime(v)
	if err != nil {
		return time.Time{}, errValidation("%s: must be an RFC 3339 timestamp", name)
	}
	return t, nil
}

// queryList splits a comma-separated parameter (also accepts repeats).
func (c *call) queryList(name string) []string {
	var out []string
	for _, v := range c.r.URL.Query()[name] {
		for _, p := range strings.Split(v, ",") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
	}
	return out
}

var idRE = regexp.MustCompile(`^[A-Za-z0-9_\-]{1,128}$`)

// pathID returns path parameter name, which must be a public id with
// prefix (any other value cannot name a resource: 404).
func (c *call) pathID(name string, prefix core.Prefix) (string, error) {
	return checkID(c.r.PathValue(name), prefix)
}

func checkID(v string, prefix core.Prefix) (string, error) {
	if !idRE.MatchString(v) || !strings.HasPrefix(v, string(prefix)+"_") || len(v) == len(prefix)+1 {
		return "", errNotFound()
	}
	return v, nil
}

// bodyID validates an id that arrives in a request body or query (400).
func bodyID(field, v string, prefix core.Prefix, required bool) (string, error) {
	if v == "" {
		if required {
			return "", errValidation("%s: required", field)
		}
		return "", nil
	}
	if _, err := checkID(v, prefix); err != nil {
		return "", errValidation("%s: must be a %s_ identifier", field, prefix)
	}
	return v, nil
}

func nextToken(tok string) any {
	if tok == "" {
		return nil
	}
	return tok
}

// list is the paginated response envelope of spec 12.1.
type list struct {
	Items         any `json:"items"`
	NextPageToken any `json:"next_page_token"`
}

func listOf[T any](items []T, next string) list {
	if items == nil {
		items = []T{}
	}
	return list{Items: items, NextPageToken: nextToken(next)}
}

// etag computes a weak-free strong ETag from a resource's JSON form.
func etag(v any) string {
	b, err := core.CanonicalJSON(v)
	if err != nil {
		return `"0"`
	}
	return `"` + core.SHA256Hex(b)[:20] + `"`
}

// checkIfMatch enforces If-Match against the current representation.
func (c *call) checkIfMatch(current any) error {
	im := strings.TrimSpace(c.r.Header.Get("If-Match"))
	if im == "" || im == "*" {
		return nil
	}
	want := etag(current)
	for _, v := range strings.Split(im, ",") {
		if strings.TrimSpace(v) == want {
			return nil
		}
	}
	return newError(http.StatusPreconditionFailed, ReasonPreconditionFailed, "If-Match does not match the current ETag")
}
