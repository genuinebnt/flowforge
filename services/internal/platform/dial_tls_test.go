package platform_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// testTLS returns a server certificate for 127.0.0.1 and a client config
// trusting only its private CA (Cloud Run's front end in production).
func testTLS(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, _ := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	caCert, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}},
		&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
}

type mdRecorder struct {
	mu sync.Mutex
	md metadata.MD
}

func (r *mdRecorder) unary(ctx context.Context, req any, _ *grpc.UnaryServerInfo, h grpc.UnaryHandler) (any, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	r.mu.Lock()
	r.md = md.Copy()
	r.mu.Unlock()
	return h(ctx, req)
}

func (r *mdRecorder) get(k string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v := r.md.Get(k); len(v) > 0 {
		return v[0]
	}
	return ""
}

func serve(t *testing.T, creds credentials.TransportCredentials) (string, *mdRecorder) {
	t.Helper()
	rec := &mdRecorder{}
	opts := []grpc.ServerOption{grpc.UnaryInterceptor(rec.unary)}
	if creds != nil {
		opts = append(opts, grpc.Creds(creds))
	}
	s := grpc.NewServer(opts...)
	healthpb.RegisterHealthServer(s, health.NewServer())
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(lis) }()
	t.Cleanup(s.Stop)
	return lis.Addr().String(), rec
}

type fakeIDTokens struct {
	mu   sync.Mutex
	auds []string
}

func (f *fakeIDTokens) IDToken(_ context.Context, aud string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.auds = append(f.auds, aud)
	return "id-token-for-" + aud, nil
}

type fixedTokens struct{}

func (fixedTokens) Token(context.Context, platform.Identity) (string, error) {
	return "internal-jwt", nil
}

// TestRegress_G_SVC_13_CloudRunDial: an https:// service address is dialled
// over TLS (here against a private CA) and every call carries both the
// internal JWT (authorization) and a Google ID token whose audience is the
// callee URL (x-serverless-authorization, spec 17.3); a plain host:port
// stays h2c and never gets an ID token.
func TestRegress_G_SVC_13_CloudRunDial(t *testing.T) {
	ctx := platform.WithIdentity(context.Background(), platform.Identity{
		Tenant: core.MustParseUUID("0190a1b2-0000-7000-8000-00000000a001"), Actor: "service:engine", Role: auth.RoleAnalyst})
	srvTLS, cliTLS := testTLS(t)
	addr, rec := serve(t, credentials.NewTLS(srvTLS))
	ids := &fakeIDTokens{}
	conn, err := platform.Dial("https://"+addr, platform.ClientOptions{Tokens: fixedTokens{}, IDTokens: ids, TLS: cliTLS})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatalf("TLS call: %v", err)
	}
	if rec.get("authorization") != "Bearer internal-jwt" || rec.get(platform.MDServerlessAuthorization) != "Bearer id-token-for-https://127.0.0.1" {
		t.Fatalf("metadata on the wire: %v", rec.md)
	}
	// The gateway's backend dialer: its own interceptors mint the JWT and
	// IDTokenDialOptions adds the ID token.
	tgt, _ := platform.ParseTarget("https://" + addr)
	gc, err := grpc.NewClient(tgt.Addr, append([]grpc.DialOption{grpc.WithTransportCredentials(tgt.TransportCredentials(cliTLS))},
		platform.IDTokenDialOptions(tgt, ids)...)...)
	if err != nil {
		t.Fatal(err)
	}
	defer gc.Close()
	if _, err := healthpb.NewHealthClient(gc).Check(context.Background(), &healthpb.HealthCheckRequest{}); err != nil ||
		rec.get(platform.MDServerlessAuthorization) != "Bearer id-token-for-https://127.0.0.1" {
		t.Fatalf("gateway-style dial: %v %v", err, rec.md)
	}
	// h2c for host:port, even with an ID-token source configured.
	plainAddr, plainRec := serve(t, nil)
	pc, err := platform.Dial(plainAddr, platform.ClientOptions{Tokens: fixedTokens{}, IDTokens: ids})
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	if _, err := healthpb.NewHealthClient(pc).Check(ctx, &healthpb.HealthCheckRequest{}); err != nil {
		t.Fatalf("h2c call: %v", err)
	}
	if plainRec.get(platform.MDServerlessAuthorization) != "" || plainRec.get("authorization") != "Bearer internal-jwt" {
		t.Fatalf("h2c metadata: %v", plainRec.md)
	}
	// The TLS target refuses a server its CA did not sign (no silent
	// downgrade to h2c).
	bad, _ := platform.Dial("https://"+plainAddr, platform.ClientOptions{Tokens: fixedTokens{}, TLS: cliTLS})
	defer bad.Close()
	cctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if _, err := healthpb.NewHealthClient(bad).Check(cctx, &healthpb.HealthCheckRequest{}); err == nil {
		t.Fatal("TLS client talked to a plaintext server")
	}
	for raw, want := range map[string]platform.Target{
		"https://ff-scope-123.us-west1.run.app": {Addr: "ff-scope-123.us-west1.run.app:443", TLS: true, Audience: "https://ff-scope-123.us-west1.run.app"},
		"https://host:8443":                     {Addr: "host:8443", TLS: true, Audience: "https://host"},
		"http://127.0.0.1:7100":                 {Addr: "127.0.0.1:7100"},
		"127.0.0.1:7100":                        {Addr: "127.0.0.1:7100"},
	} {
		if got, err := platform.ParseTarget(raw); err != nil || got != want {
			t.Errorf("%s = %+v %v", raw, got, err)
		}
	}
	if _, err := platform.ParseTarget("https://host/path"); err == nil {
		t.Error("path accepted")
	}
}
