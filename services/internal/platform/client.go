package platform

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
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
	// IDTokens, when set, adds a Google ID token for the callee to every
	// call on an https:// target (Cloud Run IAM, spec 17.3); cmd sets it on
	// Cloud Run (gcpauth.IDTokens). Plain host:port targets never get one.
	IDTokens IDTokenSource
	// TLS overrides the TLS configuration of https:// targets (tests pass
	// a private CA); nil means the system roots.
	TLS *tls.Config

	// audience is the ID-token audience of the dialled target, set by Dial.
	audience string
}

// IDTokenSource mints Google-signed ID tokens for an audience (the callee
// URL, "https://<service>-<n>.<region>.run.app").
type IDTokenSource interface {
	IDToken(ctx context.Context, audience string) (string, error)
}

// MDServerlessAuthorization carries the Google ID token. Cloud Run checks
// it instead of authorization when present, which stays free for the
// internal JWT our own interceptors verify; both are required (IAM admits
// the caller service, the JWT says which tenant and actor it acts for).
const MDServerlessAuthorization = "x-serverless-authorization"

// Target is a parsed service address.
type Target struct {
	// Addr is what grpc.NewClient dials (host:port).
	Addr string
	// TLS is set for https:// addresses: Cloud Run serves TLS on 443 only.
	TLS bool
	// Audience is the ID-token audience, "https://host" (Cloud Run
	// compares it with the service URL, which has no port), empty for
	// plaintext targets.
	Audience string
}

// ParseTarget accepts "https://host[:port]" (TLS, default port 443),
// "http://host:port" and plain "host:port" (h2c, local and dev stacks).
func ParseTarget(raw string) (Target, error) {
	switch {
	case strings.HasPrefix(raw, "https://"):
		u, err := url.Parse(raw)
		if err != nil || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			return Target{}, fmt.Errorf("platform: service address %q: want https://host[:port]", raw)
		}
		port := u.Port()
		if port == "" {
			port = "443"
		}
		return Target{Addr: net.JoinHostPort(u.Hostname(), port), TLS: true, Audience: "https://" + u.Hostname()}, nil
	case strings.HasPrefix(raw, "http://"):
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return Target{}, fmt.Errorf("platform: service address %q: want http://host:port", raw)
		}
		return Target{Addr: u.Host}, nil
	}
	return Target{Addr: raw}, nil
}

// TransportCredentials returns TLS credentials (system roots unless conf
// is given) for TLS targets and h2c otherwise.
func (t Target) TransportCredentials(conf *tls.Config) credentials.TransportCredentials {
	if !t.TLS {
		return insecure.NewCredentials()
	}
	if conf == nil {
		conf = &tls.Config{MinVersion: tls.VersionTLS12}
	}
	return credentials.NewTLS(conf)
}

// IDTokenMetadata returns the metadata pair carrying an ID token for t, or
// nil when t needs none (plaintext target or no source).
func (t Target) IDTokenMetadata(ctx context.Context, src IDTokenSource) ([]string, error) {
	if src == nil || t.Audience == "" {
		return nil, nil
	}
	tok, err := src.IDToken(ctx, t.Audience)
	if err != nil {
		return nil, fmt.Errorf("platform: ID token for %s: %w", t.Audience, err)
	}
	return []string{MDServerlessAuthorization, "Bearer " + tok}, nil
}

// Dial opens a client connection whose calls carry the metadata of spec
// 11.1 (authorization, x-tenant-id, x-actor, x-request-id, traceparent,
// idempotency-key), get a default deadline when the caller set none (unary
// 5 s, scope checks 500 ms, streams 10 min) and return *core.Error values
// mapped back from google.rpc.Status (FromStatus).
//
// The identity comes from the call's context (WithIdentity or AsService);
// a call without one fails before it is sent. Transport (ParseTarget): an
// https:// target (Cloud Run URL) is TLS to port 443 and, with IDTokens,
// carries a Google ID token per call (G-SVC-13); host:port stays h2c.
func Dial(target string, o ClientOptions, extra ...grpc.DialOption) (*grpc.ClientConn, error) {
	t, err := ParseTarget(target)
	if err != nil {
		return nil, err
	}
	o.audience = t.Audience
	if !t.TLS {
		o.IDTokens = nil
	}
	opts := append([]grpc.DialOption{
		grpc.WithTransportCredentials(t.TransportCredentials(o.TLS)),
		grpc.WithChainUnaryInterceptor(o.unary),
		grpc.WithChainStreamInterceptor(o.stream),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: KeepaliveInterval, Timeout: 10 * time.Second, PermitWithoutStream: true}),
	}, extra...)
	return grpc.NewClient(t.Addr, opts...)
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
	idt, err := (Target{Audience: o.audience}).IDTokenMetadata(ctx, o.IDTokens)
	if err != nil {
		return ctx, err
	}
	pairs = append(pairs, idt...)
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

// IDTokenDialOptions returns interceptors that append t's ID-token
// metadata to every call, for clients that mint their own authorization
// (the gateway's backend connections). Nothing is added for plaintext
// targets or without a source.
func IDTokenDialOptions(t Target, src IDTokenSource) []grpc.DialOption {
	if src == nil || t.Audience == "" {
		return nil
	}
	add := func(ctx context.Context) (context.Context, error) {
		kv, err := t.IDTokenMetadata(ctx, src)
		if err != nil {
			return ctx, err
		}
		return metadata.AppendToOutgoingContext(ctx, kv...), nil
	}
	return []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
			ctx, err := add(ctx)
			if err != nil {
				return err
			}
			return invoker(ctx, method, req, reply, cc, opts...)
		}),
		grpc.WithChainStreamInterceptor(func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			ctx, err := add(ctx)
			if err != nil {
				return nil, err
			}
			return streamer(ctx, desc, cc, method, opts...)
		}),
	}
}
