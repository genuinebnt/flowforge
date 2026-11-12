package gateway

import (
	"context"
	"errors"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	graphv1 "github.com/genuinebnt/flowforge/gen/flowforge/graph/v1"
	similarityv1 "github.com/genuinebnt/flowforge/gen/flowforge/similarity/v1"
	flowforgev1 "github.com/genuinebnt/flowforge/gen/flowforge/v1"
)

// Default deadlines of spec 11.1.
const (
	UnaryTimeout      = 5 * time.Second
	ScopeCheckTimeout = 500 * time.Millisecond
	StreamMaxDuration = 10 * time.Minute
)

// outgoing is what the client interceptors need to know about the caller.
type outgoing struct {
	tenant      core.UUID
	role        auth.Role
	sub         string
	actor       string
	requestID   string
	traceparent string
	idemKey     string
}

func (c *call) outgoing() outgoing {
	o := outgoing{requestID: c.reqID, traceparent: c.r.Header.Get("traceparent"), idemKey: c.r.Header.Get("Idempotency-Key")}
	if !traceparentRE.MatchString(o.traceparent) {
		o.traceparent = "00-" + c.s.ids.hex(32) + "-" + c.s.ids.hex(16) + "-01"
	}
	if c.p != nil {
		o.tenant, o.role, o.sub, o.actor = c.p.TenantID, c.p.Role, c.p.subject(), c.p.actor()
	}
	return o
}

// MinterSource yields the current token minter (identity.KeyManager).
type MinterSource interface {
	Minter() *auth.Minter
}

// errNoCaller is returned when a backend call is made outside a request.
var errNoCaller = errors.New("gateway: backend call without an authenticated caller")

// withInternalToken attaches the spec 11.1 metadata: a 60-second internal
// JWT (aud flowforge-internal) carrying tenant, actor and role, plus
// x-tenant-id, x-actor, x-request-id, traceparent and idempotency-key.
func withInternalToken(ctx context.Context, keys MinterSource) (context.Context, error) {
	o, ok := ctx.Value(keyOutgoing).(outgoing)
	if !ok || o.tenant.IsZero() {
		return nil, status.Error(codes.Internal, errNoCaller.Error())
	}
	tok, _, err := keys.Minter().Internal(auth.InternalGrant{TenantID: o.tenant, Actor: o.sub, Role: o.role})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "gateway: mint internal token: %v", err)
	}
	kv := []string{
		"authorization", "Bearer " + tok,
		"x-tenant-id", core.PublicID(core.PrefixTenant, o.tenant),
		"x-actor", o.actor,
		"x-request-id", o.requestID,
		"traceparent", o.traceparent,
	}
	if o.idemKey != "" {
		kv = append(kv, "idempotency-key", o.idemKey)
	}
	return metadata.AppendToOutgoingContext(ctx, kv...), nil
}

// UnaryClientInterceptor attaches caller metadata and the default deadline
// (5 s; 500 ms for ScopeService.Check) to unary calls.
func UnaryClientInterceptor(keys MinterSource) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx, err := withInternalToken(ctx, keys)
		if err != nil {
			return err
		}
		if _, ok := ctx.Deadline(); !ok {
			d := UnaryTimeout
			if method == flowforgev1.ScopeService_Check_FullMethodName {
				d = ScopeCheckTimeout
			}
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, d)
			defer cancel()
		}
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

// StreamClientInterceptor attaches caller metadata to streaming calls (the
// SSE handlers bound their duration).
func StreamClientInterceptor(keys MinterSource) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx, err := withInternalToken(ctx, keys)
		if err != nil {
			return nil, err
		}
		return streamer(ctx, desc, cc, method, opts...)
	}
}

// DialOptions returns the interceptors every backend connection needs.
func DialOptions(keys MinterSource) []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithChainUnaryInterceptor(UnaryClientInterceptor(keys)),
		grpc.WithChainStreamInterceptor(StreamClientInterceptor(keys)),
	}
}

// BackendsFromConns builds Backends from one connection per service.
func BackendsFromConns(scope, engine, graph, notifier, toolrunner, similarity grpc.ClientConnInterface) Backends {
	return Backends{
		Scope: flowforgev1.NewScopeServiceClient(scope), Engine: flowforgev1.NewEngineServiceClient(engine),
		Graph: flowforgev1.NewGraphServiceClient(graph), Notifier: flowforgev1.NewNotifierServiceClient(notifier),
		ToolRunner: flowforgev1.NewToolRunnerServiceClient(toolrunner), Analytics: graphv1.NewGraphAnalyticsClient(graph),
		Similarity: similarityv1.NewSimilarityClient(similarity),
	}
}
