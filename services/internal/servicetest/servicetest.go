// Package servicetest holds helpers for the services' tests: a throwaway
// Postgres database per test (FF_TEST_PG_DSN), a counting static resolver
// and a webhook receiver that verifies Flowforge signatures. It is
// imported only from _test.go files.
package servicetest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/devstack"
)

var dbSeq atomic.Int64

// FreshDB creates an empty database on the FF_TEST_PG_DSN server, drops it
// when the test ends and returns its DSN. It skips the test when the
// variable is unset.
func FreshDB(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("FF_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("FF_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	name := fmt.Sprintf("ff_services_%d_%d", os.Getpid(), dbSeq.Add(1))
	if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = admin.Close(context.Background())
	})
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	return fmt.Sprintf("postgres://%s@%s:%d/%s?sslmode=disable", cfg.User, cfg.Host, cfg.Port, name)
}

// Resolver is a static resolver that counts lookups per name and can be
// re-pointed at run time (DNS rebinding tests).
type Resolver struct {
	mu      sync.Mutex
	answers map[string][]netip.Addr
	counts  map[string]int
}

var _ scope.Resolver = (*Resolver)(nil)

// NewResolver returns a resolver answering name -> addresses.
func NewResolver(m map[string]string) *Resolver {
	r := &Resolver{answers: map[string][]netip.Addr{}, counts: map[string]int{}}
	for k, v := range m {
		r.Set(k, v)
	}
	return r
}

// Set points name at addrs (comma-free single address).
func (r *Resolver) Set(name, addr string) {
	r.mu.Lock()
	r.answers[name] = []netip.Addr{netip.MustParseAddr(addr)}
	r.mu.Unlock()
}

// LookupAddrs implements scope.Resolver.
func (r *Resolver) LookupAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counts[host]++
	a, ok := r.answers[host]
	if !ok {
		return nil, scope.ErrNoSuchHost
	}
	return append([]netip.Addr(nil), a...), ctx.Err()
}

// Count returns the number of lookups of name.
func (r *Resolver) Count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[name]
}

// Webhook is a local receiver that verifies X-Flowforge-Signature.
type Webhook struct {
	*httptest.Server
	Secret   []byte
	mu       sync.Mutex
	Bodies   [][]byte
	Rejected int
}

// NewWebhook starts a receiver on 127.0.0.1.
func NewWebhook(t testing.TB, secret string) *Webhook {
	w := &Webhook{Secret: []byte(secret)}
	w.Server = httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		err := alerts.Verify(w.Secret, r.Header.Get(alerts.HeaderTimestamp), r.Header.Get(alerts.HeaderSignature), body, time.Now())
		w.mu.Lock()
		defer w.mu.Unlock()
		if err != nil || r.Header.Get(alerts.HeaderEvent) != alerts.EventAlertFired {
			w.Rejected++
			http.Error(rw, "bad signature", http.StatusUnauthorized)
			return
		}
		w.Bodies = append(w.Bodies, body)
	}))
	t.Cleanup(w.Close)
	return w
}

// Received returns the verified bodies so far.
func (w *Webhook) Received() [][]byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([][]byte(nil), w.Bodies...)
}

// Eventually polls cond every 20 ms until it holds or d elapses.
func Eventually(t testing.TB, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Tenants used by the service tests.
var (
	TenantA = core.MustParseUUID("0190a1b2-0000-7000-8000-00000000a001")
	TenantB = core.MustParseUUID("0190a1b2-0000-7000-8000-00000000b002")
	UserA   = "user:0190a1b2-0000-7000-8000-0000000000a1"
	UserB   = "user:0190a1b2-0000-7000-8000-0000000000b1"
)

// Stack starts a dev stack for a test (Postgres when FF_TEST_PG_DSN is set
// and pg is true) and returns contexts for tenants A and B (admin role).
func Stack(t testing.TB, o devstack.Options) (*devstack.Stack, context.Context, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	if o.Engine.PollInterval == 0 {
		o.Engine.PollInterval = 10 * time.Millisecond
		o.Engine.IdleBackoff = 50 * time.Millisecond
	}
	st, err := devstack.Start(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, st.As(ctx, TenantA, UserA, auth.RoleAdmin), st.As(ctx, TenantB, UserB, auth.RoleAdmin)
}

// ScopeDoc returns a scope document allowing the given domains (and the
// loopback address, which the egress proxy needs explicitly) up to active.
func ScopeDoc(domains ...string) *pb.ScopeDocument {
	doc := &pb.ScopeDocument{Activity: &pb.ActivityPolicy{MaxClass: pb.ActivityClass_ACTIVITY_CLASS_ACTIVE},
		Rules: []*pb.ScopeRule{{Id: "lo", Effect: pb.RuleEffect_RULE_EFFECT_ALLOW, Kind: pb.RuleKind_RULE_KIND_CIDR, Value: "127.0.0.1/32"}}}
	for i, d := range domains {
		doc.Rules = append(doc.Rules, &pb.ScopeRule{Id: fmt.Sprintf("d%d", i), Effect: pb.RuleEffect_RULE_EFFECT_ALLOW, Kind: pb.RuleKind_RULE_KIND_DOMAIN, Value: d})
	}
	return doc
}

// IsKind fails the test unless err is a *core.Error of kind with reason
// (reason "" matches any).
func IsKind(t testing.TB, err error, kind core.Kind, reason string) {
	t.Helper()
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Kind != kind || (reason != "" && ce.Reason != reason) {
		t.Fatalf("want %v %s, got %v", kind, reason, err)
	}
}
