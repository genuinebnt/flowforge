package platform

import (
	"context"
	"fmt"
	"log/slog"
	"runtime/debug"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/genuinebnt/flowforge/core"
)

// Default deadlines of spec 11.1.
const (
	DefaultUnaryTimeout  = 5 * time.Second
	ScopeCheckTimeout    = 500 * time.Millisecond
	DefaultStreamTimeout = 10 * time.Minute
	KeepaliveInterval    = 30 * time.Second
)

// MethodScopeCheck and MethodScopeCheckBatch are the scope hot path (500 ms).
const (
	MethodScopeCheck      = "/flowforge.v1.ScopeService/Check"
	MethodScopeCheckBatch = "/flowforge.v1.ScopeService/CheckBatch"
)

// ServerOptions configure NewGRPCServer.
type ServerOptions struct {
	// Service names the service in ErrorInfo.domain and logs.
	Service string
	// Verifier checks internal tokens. Required.
	Verifier TokenVerifier
	Logger   *slog.Logger
	Metrics  *Metrics
	// Timeouts overrides the deadline applied to calls that arrive without
	// one, per full method name.
	Timeouts map[string]time.Duration
}

// publicMethods need no token (health checks).
func publicMethod(m string) bool { return strings.HasPrefix(m, "/grpc.health.v1.Health/") }

// NewGRPCServer returns a gRPC server whose every call runs through the
// interceptor chain: request id and traceparent, logging and metrics,
// error mapping to google.rpc.Status (spec 11.8), panic recovery,
// internal-token authentication (spec 3.2) and a default deadline (spec
// 11.1) for calls that arrive without one.
func NewGRPCServer(o ServerOptions, extra ...grpc.ServerOption) *grpc.Server {
	if o.Logger == nil {
		o.Logger = DiscardLogger()
	}
	if o.Metrics == nil {
		o.Metrics = NewMetrics(nil)
	}
	opts := append([]grpc.ServerOption{
		grpc.ChainUnaryInterceptor(o.unary),
		grpc.ChainStreamInterceptor(o.stream),
		grpc.KeepaliveParams(keepalive.ServerParameters{Time: KeepaliveInterval, Timeout: 10 * time.Second}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{MinTime: 10 * time.Second, PermitWithoutStream: true}),
	}, extra...)
	return grpc.NewServer(opts...)
}

// prepare builds the handler context: request id, trace, idempotency key,
// identity. It returns the context and an error to fail the call with.
func (o ServerOptions) prepare(ctx context.Context, method string) (context.Context, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	get := func(k string) string {
		if v := md.Get(k); len(v) > 0 {
			return v[0]
		}
		return ""
	}
	rid := get(MDRequestID)
	if rid == "" || len(rid) > 128 {
		rid = NewRequestID()
	}
	ctx = WithRequestID(ctx, rid)
	parent, ok := ParseTraceparent(get(MDTraceparent))
	if !ok {
		parent = NewTrace()
	}
	ctx = WithTrace(ctx, parent.Child())
	if k := get(MDIdempotencyKey); k != "" {
		ctx = WithIdempotencyKey(ctx, k)
	}
	if publicMethod(method) {
		return ctx, nil
	}
	if o.Verifier == nil {
		return ctx, core.Errorf(core.KindUnauthenticated, core.ReasonTokenInvalid, "server has no token verifier")
	}
	id, err := identityFromToken(ctx, o.Verifier, get(MDAuthorization), get(MDTenant), get(MDActor))
	if err != nil {
		return ctx, err
	}
	return WithIdentity(ctx, id), nil
}

func (o ServerOptions) timeout(method string, streaming bool) time.Duration {
	if d, ok := o.Timeouts[method]; ok {
		return d
	}
	switch {
	case method == MethodScopeCheck || method == MethodScopeCheckBatch:
		return ScopeCheckTimeout
	case streaming:
		return DefaultStreamTimeout
	}
	return DefaultUnaryTimeout
}

func (o ServerOptions) finish(ctx context.Context, method string, start time.Time, err error) error {
	err = ToStatus(err, o.Service, RequestIDFrom(ctx))
	code := status.Code(err)
	o.Metrics.RPCTotal.Inc(method, code.String())
	o.Metrics.RPCDuration.Since(start, method)
	if err != nil && !publicMethod(method) {
		lvl := slog.LevelInfo
		if code.String() == "Internal" || code.String() == "Unknown" {
			lvl = slog.LevelError
		}
		o.Logger.Log(ctx, lvl, "rpc.error", "method", method, "code", code.String(), "error", status.Convert(err).Message())
	}
	return err
}

func (o ServerOptions) unary(ctx context.Context, req any, info *grpc.UnaryServerInfo, h grpc.UnaryHandler) (resp any, err error) {
	start := time.Now()
	ctx, err = o.prepare(ctx, info.FullMethod)
	defer func() { err = o.finish(ctx, info.FullMethod, start, err) }()
	if err != nil {
		return nil, err
	}
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout(info.FullMethod, false))
		defer cancel()
	}
	defer func() {
		if r := recover(); r != nil {
			o.Logger.ErrorContext(ctx, "rpc.panic", "method", info.FullMethod, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			resp, err = nil, core.Errorf(core.KindInternal, core.ReasonInternal, "panic")
		}
	}()
	return h(ctx, req)
}

type wrappedStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (w wrappedStream) Context() context.Context { return w.ctx }

func (o ServerOptions) stream(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, h grpc.StreamHandler) (err error) {
	start := time.Now()
	ctx, err := o.prepare(ss.Context(), info.FullMethod)
	defer func() { err = o.finish(ctx, info.FullMethod, start, err) }()
	if err != nil {
		return err
	}
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout(info.FullMethod, true))
		defer cancel()
	}
	defer func() {
		if r := recover(); r != nil {
			o.Logger.ErrorContext(ctx, "rpc.panic", "method", info.FullMethod, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
			err = core.Errorf(core.KindInternal, core.ReasonInternal, "panic")
		}
	}()
	return h(srv, wrappedStream{ServerStream: ss, ctx: ctx})
}
