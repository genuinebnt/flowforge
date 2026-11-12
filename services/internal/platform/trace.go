package platform

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"strings"

	"github.com/genuinebnt/flowforge/core"
)

// Trace is a W3C trace context (traceparent, spec 18.1, 23.7): version 00,
// a 16-byte trace id, the 8-byte id of the current span and the flags.
// There is no tracing SDK in this module; spans are propagated (and logged
// as trace_id/span_id) but not exported.
type Trace struct {
	TraceID [16]byte
	SpanID  [8]byte
	Flags   byte
}

// ParseTraceparent parses "00-<32 hex>-<16 hex>-<2 hex>". All-zero ids and
// other versions' unknown layouts are rejected (ok false).
func ParseTraceparent(s string) (Trace, bool) {
	parts := strings.Split(strings.TrimSpace(s), "-")
	if len(parts) < 4 || len(parts[0]) != 2 || len(parts[1]) != 32 || len(parts[2]) != 16 || len(parts[3]) != 2 {
		return Trace{}, false
	}
	if parts[0] == "ff" || (parts[0] == "00" && len(parts) != 4) {
		return Trace{}, false
	}
	var t Trace
	if _, err := hex.Decode(t.TraceID[:], []byte(parts[1])); err != nil {
		return Trace{}, false
	}
	if _, err := hex.Decode(t.SpanID[:], []byte(parts[2])); err != nil {
		return Trace{}, false
	}
	var f [1]byte
	if _, err := hex.Decode(f[:], []byte(parts[3])); err != nil {
		return Trace{}, false
	}
	t.Flags = f[0]
	if t.TraceID == ([16]byte{}) || t.SpanID == ([8]byte{}) {
		return Trace{}, false
	}
	return t, true
}

// String renders the traceparent header value.
func (t Trace) String() string {
	return "00-" + hex.EncodeToString(t.TraceID[:]) + "-" + hex.EncodeToString(t.SpanID[:]) + "-" + hex.EncodeToString([]byte{t.Flags})
}

// TraceIDHex returns the trace id in hex.
func (t Trace) TraceIDHex() string { return hex.EncodeToString(t.TraceID[:]) }

// SpanIDHex returns the span id in hex.
func (t Trace) SpanIDHex() string { return hex.EncodeToString(t.SpanID[:]) }

// IsZero reports whether t is unset.
func (t Trace) IsZero() bool { return t.TraceID == [16]byte{} }

// NewTrace starts a new sampled trace.
func NewTrace() Trace {
	var t Trace
	rng := core.SystemRng{}
	binary.BigEndian.PutUint64(t.TraceID[:8], rng.Uint64())
	binary.BigEndian.PutUint64(t.TraceID[8:], rng.Uint64())
	t.SpanID = newSpanID()
	t.Flags = 1
	return t
}

// Child returns a new span of the same trace.
func (t Trace) Child() Trace {
	if t.IsZero() {
		return NewTrace()
	}
	return Trace{TraceID: t.TraceID, SpanID: newSpanID(), Flags: t.Flags}
}

func newSpanID() [8]byte {
	var s [8]byte
	for s == ([8]byte{}) {
		binary.BigEndian.PutUint64(s[:], core.SystemRng{}.Uint64())
	}
	return s
}

type traceKey struct{}

// WithTrace returns ctx carrying t as the current span.
func WithTrace(ctx context.Context, t Trace) context.Context {
	return context.WithValue(ctx, traceKey{}, t)
}

// TraceFrom returns the current span in ctx.
func TraceFrom(ctx context.Context) (Trace, bool) {
	t, ok := ctx.Value(traceKey{}).(Trace)
	return t, ok
}

// StartSpan returns ctx with a child span of the current one (or a new
// trace) and that span.
func StartSpan(ctx context.Context) (context.Context, Trace) {
	t, _ := TraceFrom(ctx)
	c := t.Child()
	return WithTrace(ctx, c), c
}
