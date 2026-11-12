package platform

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

// NewLogger returns the JSON logger of spec 18.1: one line per event with
// the fields ts, level, service, env, trace_id, span_id, request_id,
// tenant_id, run_id, effect_path and msg. The context fields are taken from
// the context passed to the *Context logging methods. Callers must never
// log secrets, tokens or raw target payloads.
func NewLogger(w io.Writer, service, env string, level slog.Level) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 {
				switch a.Key {
				case slog.TimeKey:
					a.Key = "ts"
				case slog.LevelKey:
					a.Value = slog.StringValue(strings.ToLower(a.Value.String()))
				}
			}
			return a
		},
	})
	return slog.New(ctxHandler{h.WithAttrs([]slog.Attr{slog.String("service", service), slog.String("env", env)})})
}

// DiscardLogger returns a logger that drops everything (tests).
func DiscardLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// ParseLevel parses debug, info, warn or error (default info).
func ParseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	}
	return slog.LevelInfo
}

type logFieldsKey struct{}

type logFields struct {
	runID, effectPath string
}

// WithLogFields returns ctx whose log lines carry run_id and effect_path.
func WithLogFields(ctx context.Context, runID, effectPath string) context.Context {
	return context.WithValue(ctx, logFieldsKey{}, logFields{runID: runID, effectPath: effectPath})
}

type ctxHandler struct{ slog.Handler }

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if ctx != nil {
		if t, ok := TraceFrom(ctx); ok {
			r.AddAttrs(slog.String("trace_id", t.TraceIDHex()), slog.String("span_id", t.SpanIDHex()))
		}
		if id := RequestIDFrom(ctx); id != "" {
			r.AddAttrs(slog.String("request_id", id))
		}
		if id, ok := IdentityFrom(ctx); ok {
			r.AddAttrs(slog.String("tenant_id", id.TenantPublicID()))
		}
		if f, ok := ctx.Value(logFieldsKey{}).(logFields); ok {
			if f.runID != "" {
				r.AddAttrs(slog.String("run_id", f.runID))
			}
			if f.effectPath != "" {
				r.AddAttrs(slog.String("effect_path", f.effectPath))
			}
		}
	}
	return h.Handler.Handle(ctx, r)
}

func (h ctxHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return ctxHandler{h.Handler.WithAttrs(as)}
}
func (h ctxHandler) WithGroup(name string) slog.Handler { return ctxHandler{h.Handler.WithGroup(name)} }
