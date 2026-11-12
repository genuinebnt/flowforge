package platform

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// ShutdownGrace bounds graceful shutdown after SIGTERM (Cloud Run gives 10 s).
const ShutdownGrace = 8 * time.Second

// Health tracks liveness and readiness: the gRPC health service, /healthz
// (process alive) and /readyz (every registered check passes).
type Health struct {
	GRPC *health.Server

	mu     sync.Mutex
	checks map[string]func(context.Context) error
}

// NewHealth returns a health tracker reporting SERVING.
func NewHealth() *Health {
	h := &Health{GRPC: health.NewServer(), checks: map[string]func(context.Context) error{}}
	h.GRPC.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	return h
}

// AddCheck registers a readiness check (for example a database ping).
func (h *Health) AddCheck(name string, f func(context.Context) error) {
	h.mu.Lock()
	h.checks[name] = f
	h.mu.Unlock()
}

// Ready runs every readiness check.
func (h *Health) Ready(ctx context.Context) error {
	h.mu.Lock()
	checks := make(map[string]func(context.Context) error, len(h.checks))
	for k, v := range h.checks {
		checks[k] = v
	}
	h.mu.Unlock()
	var errs []error
	for name, f := range checks {
		if err := f(ctx); err != nil {
			errs = append(errs, errors.New(name+": "+err.Error()))
		}
	}
	return errors.Join(errs...)
}

// Mux returns the HTTP admin mux: /healthz, /readyz and /metrics.
func (h *Health) Mux(reg *Registry) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := h.Ready(ctx); err != nil {
			http.Error(w, "not ready: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	if reg != nil {
		mux.Handle("GET /metrics", reg.Handler())
	}
	return mux
}

// Shutdown marks the service NOT_SERVING (load balancers stop routing).
func (h *Health) Shutdown() { h.GRPC.Shutdown() }

// Handler serves gRPC and plain HTTP on one port over h2c (Cloud Run's
// HTTP/2 end-to-end mode, spec 17.1): requests with content-type
// application/grpc go to the gRPC server, everything else to mux.
func Handler(g *grpc.Server, mux http.Handler) http.Handler {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g != nil && r.ProtoMajor == 2 && strings.HasPrefix(r.Header.Get("Content-Type"), "application/grpc") {
			g.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
	return h2c.NewHandler(h, &http2.Server{IdleTimeout: 5 * time.Minute})
}

// Serve serves h on lis until ctx is done, then shuts down gracefully
// within ShutdownGrace: health goes NOT_SERVING, new connections are
// refused, in-flight requests finish (streams are cut at the deadline).
func Serve(ctx context.Context, lis net.Listener, h http.Handler, health *Health, log *slog.Logger) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(lis) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	if health != nil {
		health.Shutdown()
	}
	if log != nil {
		log.Info("shutdown.start")
	}
	sctx, cancel := context.WithTimeout(context.Background(), ShutdownGrace)
	defer cancel()
	err := srv.Shutdown(sctx)
	if err != nil {
		_ = srv.Close()
	}
	if errors.Is(err, context.DeadlineExceeded) {
		err = nil
	}
	return err
}

// RegisterHealth registers the gRPC health service on g.
func RegisterHealth(g *grpc.Server, h *Health) { healthpb.RegisterHealthServer(g, h.GRPC) }
