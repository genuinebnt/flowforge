package platform

import (
	"context"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

// ClientOptions configure Dial.
type ClientOptions struct {
	// Tokens mints the internal token for the identity in each call's
	// context. Required for authenticated services.
	Tokens TokenSource
	// Timeouts overrides the default deadline per full method name.
	Timeouts map[string]time.Duration
}

// Dial opens a client connection whose calls carry the metadata of spec
// 11.1 (authorization, x-tenant-id, x-actor, x-request-id, traceparent,
// idempotency-key), get a default deadline when the caller set none (unary
// 5 s, scope checks 500 ms, streams 10 min) and return *core.Error values
// mapped back from google.rpc.Status (FromStatus).
//
// The identity comes from the call's context (WithIdentity or AsService);
// a call without one fails before it is sent. Transport security is plain
// h2c: Cloud Run terminates TLS in front of every service (spec 17.1).
func Dial(target string, o ClientOptions, extra ...grpc.DialOption) (*grpc.ClientConn, error) {
	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithChainUnaryInterceptor(o.unary),
		grpc.WithChainStreamInterceptor(o.stream),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: KeepaliveInterval, Timeout: 10 * time.Second, PermitWithoutStream: true}),
	}, extra...)
	return grpc.NewClient(target, opts...)
}

func (o ClientOptions) outgoing(ctx context.Context) (context.Context, error) {
	id, err := MustIdentity(ctx)
	if err != nil {
		return ctx, err
	}
	pairs := []string{MDTenant, id.TenantPublicID(), MDActor, id.Actor}
	if o.Tokens != nil {
		tok, err := o.Tokens.Token(ctx, id)
		if err != nil {
			return ctx, err
		}
		pairs = append(pairs, MDAuthorization, "Bearer "+tok)
	}
	rid := RequestIDFrom(ctx)
	if rid == "" {
		rid = NewRequestID()
	}
	pairs = append(pairs, MDRequestID, rid)
	ctx, span := StartSpan(ctx)
	pairs = append(pairs, MDTraceparent, span.String())
	if k := IdempotencyKeyFrom(ctx); k != "" {
		pairs = append(pairs, MDIdempotencyKey, k)
	}
	// Replace, never merge, metadata: a handler's incoming metadata must not
	// leak into its outgoing calls.
	return metadata.NewOutgoingContext(ctx, metadata.Pairs(pairs...)), nil
}

func (o ClientOptions) deadline(method string, streaming bool) time.Duration {
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

func (o ClientOptions) unary(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
	ctx, err := o.outgoing(ctx)
	if err != nil {
		return err
	}
	if _, has := ctx.Deadline(); !has {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.deadline(method, false))
		defer cancel()
	}
	return FromStatus(invoker(ctx, method, req, reply, cc, opts...))
}

func (o ClientOptions) stream(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	ctx, err := o.outgoing(ctx)
	if err != nil {
		return nil, err
	}
	var cancel context.CancelFunc = func() {}
	if _, has := ctx.Deadline(); !has {
		ctx, cancel = context.WithTimeout(ctx, o.deadline(method, true))
	}
	cs, err := streamer(ctx, desc, cc, method, opts...)
	if err != nil {
		cancel()
		return nil, FromStatus(err)
	}
	return &clientStream{ClientStream: cs, cancel: cancel}, nil
}

type clientStream struct {
	grpc.ClientStream
	cancel context.CancelFunc
}

func (s *clientStream) RecvMsg(m any) error {
	err := s.ClientStream.RecvMsg(m)
	if err != nil {
		s.cancel()
		return FromStatus(err)
	}
	return nil
}
