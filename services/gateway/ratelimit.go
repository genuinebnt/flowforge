package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/limiter"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
)

// rateLimit charges the request against rl:api:<tenant>:<route class>
// (spec 14.3; unauthenticated callers are keyed by client IP) and sets
// the RateLimit headers of spec 12.1. A limiter failure is logged and the
// request admitted: the API limit protects the platform, and failing
// closed on a cache outage would take the whole API down.
func (s *Server) rateLimit(c *call) error {
	if c.rt.class == "" {
		return nil
	}
	plan, subject := "anonymous", "ip/"+c.ip
	if c.p != nil {
		plan, subject = s.plans.get(c.ctx, c.p.TenantID), c.p.tenant()
	}
	p, ok := s.cfg.Plans[plan][c.rt.class]
	if !ok {
		if p, ok = s.cfg.Plans["free"][c.rt.class]; !ok {
			return nil
		}
	}
	res, err := s.cfg.Limiter.Take(c.ctx, limiter.APIKey(subject, c.rt.class), p, 1)
	if err != nil {
		s.cfg.Log.WarnContext(c.ctx, "gateway: rate limiter unavailable", "err", err)
		return nil
	}
	h := c.w.Header()
	h.Set("RateLimit-Limit", strconv.Itoa(p.Burst))
	if !res.Allowed {
		h.Set("RateLimit-Remaining", "0")
		h.Set("RateLimit-Reset", strconv.Itoa(identity.RetrySeconds(res.RetryAfter)))
		return &apiError{Status: http.StatusTooManyRequests, Reason: core.ReasonRateLimited,
			Message: "rate limit exceeded for " + c.rt.class + " requests", RetryAfter: res.RetryAfter}
	}
	remaining := max(res.Remaining, 0)
	h.Set("RateLimit-Remaining", strconv.Itoa(remaining))
	reset := int(math.Ceil(float64(p.Burst-remaining) / p.Rate))
	h.Set("RateLimit-Reset", strconv.Itoa(max(reset, 0)))
	return nil
}

// IdempotencyTTL is how long Idempotency-Key responses are kept (9.10).
const IdempotencyTTL = 7 * 24 * time.Hour

// idemStale is when an in-flight key is presumed abandoned.
const idemStale = time.Minute

func validIdemKey(k string) bool {
	if len(k) < 1 || len(k) > 128 {
		return false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7e {
			return false
		}
	}
	return true
}

// requestHash is SHA-256 over method, path and the canonical JSON body (the
// raw body when it is not JSON).
func requestHash(method, path string, body []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(method + "\n" + path + "\n"))
	var v any
	if json.Unmarshal(body, &v) == nil {
		if canon, err := core.CanonicalJSON(v); err == nil {
			body = canon
		}
	}
	h.Write(body)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// capture buffers a response so it can be stored for replay.
type capture struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (c *capture) WriteHeader(code int) {
	if c.status == 0 {
		c.status = code
	}
}

func (c *capture) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	return c.buf.Write(b)
}

// idempotent runs a POST with Idempotency-Key semantics (spec 12.1): the
// first response (below 500) is stored for 7 days and replayed with
// Idempotent-Replayed: true; the same key with another request is 409
// IDEMPOTENCY_KEY_REUSED.
func (s *Server) idempotent(c *call) error {
	key := c.r.Header.Get("Idempotency-Key")
	if key != "" && !validIdemKey(key) {
		return errValidation("Idempotency-Key: must be 1-128 printable ASCII characters")
	}
	if key == "" {
		if c.rt.idem == idemRequired {
			return errValidation("Idempotency-Key header is required for this endpoint")
		}
		return c.rt.h(c)
	}
	if c.rt.idem == idemNone || c.p == nil {
		return c.rt.h(c)
	}
	body, err := readBody(c.r)
	if err != nil {
		return err
	}
	st := s.cfg.Identity.Store()
	now := s.cfg.Clock.Now()
	hash := requestHash(c.r.Method, c.path, body)
	rec, fresh, err := st.IdemBegin(c.ctx, identity.IdemRecord{
		TenantID: c.p.TenantID, Key: key, Method: c.r.Method, Path: c.path, RequestHash: hash,
		CreatedAt: now, ExpiresAt: now.Add(IdempotencyTTL),
	}, idemStale)
	if err != nil {
		return err
	}
	if !fresh {
		switch {
		case rec.RequestHash != hash:
			return newError(http.StatusConflict, core.ReasonIdempotencyKeyReused, "Idempotency-Key was already used with a different request")
		case rec.CompletedAt.IsZero():
			return newError(http.StatusConflict, core.ReasonIdempotencyKeyReused, "a request with this Idempotency-Key is still in progress")
		}
		h := c.w.Header()
		if rec.ContentType != "" {
			h.Set("Content-Type", rec.ContentType)
		}
		h.Set("Idempotent-Replayed", "true")
		if c.rt.method == http.MethodPost && c.rt.pattern == "/v1/runs" {
			setRunLocation(h, rec.Body)
		}
		c.wrote = true
		c.w.WriteHeader(rec.Status)
		_, _ = c.w.Write(rec.Body)
		return nil
	}
	real := c.w
	cw := &capture{ResponseWriter: real}
	c.w = cw
	if err := c.rt.h(c); err != nil {
		c.fail(err)
	}
	c.w = real
	if cw.status == 0 {
		cw.status = http.StatusOK
	}
	if cw.status < 500 {
		if err := st.IdemComplete(c.ctx, c.p.TenantID, key, cw.status, cw.buf.Bytes(), real.Header().Get("Content-Type"), s.cfg.Clock.Now()); err != nil {
			s.cfg.Log.WarnContext(c.ctx, "gateway: store idempotent response", "err", err)
		}
	} else {
		_ = st.IdemAbandon(c.ctx, c.p.TenantID, key)
	}
	real.WriteHeader(cw.status)
	_, _ = real.Write(cw.buf.Bytes())
	return nil
}

func setRunLocation(h http.Header, body []byte) {
	var v struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(body, &v) == nil && v.ID != "" {
		h.Set("Location", "/v1/runs/"+v.ID)
	}
}
