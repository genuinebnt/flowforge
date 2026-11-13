package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// Workflow schedules (G-OPS-10): POST/GET /v1/schedules,
// GET/DELETE /v1/schedules/{id}, permission runs:submit (analyst and
// above: a schedule submits runs). The engine stores and fires them; it
// exposes the API as JSON under /internal/schedules on its port (the proto
// EngineService has no schedule RPCs, and adding them would change the
// v1 contract of spec 11), so the gateway calls it over HTTP with the same
// internal token it would put on a gRPC call.

// Schedules is the engine's schedule API. Do sends one request with the
// caller's internal token and returns the status and body.
type Schedules interface {
	Do(ctx context.Context, method, path string, body []byte) (int, []byte, error)
}

// HTTPSchedules calls the engine at BaseURL (http://<FF_ENGINE_ADDR>, or
// https:// on Cloud Run). With IDTokens and an Audience every request also
// carries a Google ID token in X-Serverless-Authorization (spec 17.3), as
// the gRPC calls do; Authorization keeps the internal token.
type HTTPSchedules struct {
	BaseURL  string
	Keys     MinterSource
	IDTokens platform.IDTokenSource
	Audience string
	Client   *http.Client // default: 10 s timeout
}

// Do implements Schedules.
func (h HTTPSchedules) Do(ctx context.Context, method, path string, body []byte) (int, []byte, error) {
	o, ok := ctx.Value(keyOutgoing).(outgoing)
	if !ok || o.tenant.IsZero() {
		return 0, nil, errNoCaller
	}
	tok, _, err := h.Keys.Minter().Internal(auth.InternalGrant{TenantID: o.tenant, Actor: o.sub, Role: o.role})
	if err != nil {
		return 0, nil, fmt.Errorf("gateway: mint internal token: %w", err)
	}
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(h.BaseURL, "/")+path, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-Id", o.requestID)
	req.Header.Set("Traceparent", o.traceparent)
	if h.IDTokens != nil && h.Audience != "" {
		id, err := h.IDTokens.IDToken(ctx, h.Audience)
		if err != nil {
			return 0, nil, err
		}
		req.Header.Set("X-Serverless-Authorization", "Bearer "+id)
	}
	cl := h.Client
	if cl == nil {
		cl = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b, err
}

// scheduleIn is the create body (strict: unknown fields are refused here,
// before the engine sees them).
type scheduleIn struct {
	Name            string          `json:"name"`
	Cron            string          `json:"cron"`
	WorkflowName    string          `json:"workflow_name"`
	WorkflowVersion int32           `json:"workflow_version,omitempty"`
	ProgramID       string          `json:"program_id,omitempty"`
	Input           json.RawMessage `json:"input,omitempty"`
	Enabled         *bool           `json:"enabled,omitempty"`
}

func (s *Server) schedules(c *call, method, path string, body []byte) error {
	if s.cfg.Backends.Schedules == nil {
		return newError(http.StatusServiceUnavailable, core.ReasonUnavailable, "schedules are not configured")
	}
	code, out, err := s.cfg.Backends.Schedules.Do(c.ctx, method, path, body)
	if err != nil {
		return newError(http.StatusServiceUnavailable, core.ReasonUnavailable, "the engine is unavailable")
	}
	msg := strings.TrimSpace(string(out))
	switch {
	case code == http.StatusNoContent:
		return c.noContent()
	case code == http.StatusOK || code == http.StatusCreated:
		var v any
		if err := json.Unmarshal(out, &v); err != nil {
			return internalError()
		}
		return c.json(code, v)
	case code == http.StatusBadRequest:
		return errValidation("%s", msg)
	case code == http.StatusNotFound:
		return errNotFound()
	case code == http.StatusConflict:
		return newError(http.StatusConflict, core.ReasonAlreadyExists, "%s", msg)
	}
	return internalError()
}

func (s *Server) createSchedule(c *call) error {
	var in scheduleIn
	if err := c.decode(&in); err != nil {
		return err
	}
	b, _ := json.Marshal(in)
	return s.schedules(c, http.MethodPost, "/internal/schedules", b)
}

func (s *Server) listSchedules(c *call) error {
	return s.schedules(c, http.MethodGet, "/internal/schedules", nil)
}

func (s *Server) getSchedule(c *call) error {
	id, err := c.pathID("id", core.PrefixSchedule)
	if err != nil {
		return err
	}
	return s.schedules(c, http.MethodGet, "/internal/schedules/"+id, nil)
}

func (s *Server) deleteSchedule(c *call) error {
	id, err := c.pathID("id", core.PrefixSchedule)
	if err != nil {
		return err
	}
	return s.schedules(c, http.MethodDelete, "/internal/schedules/"+id, nil)
}
