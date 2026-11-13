package gatewayd

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func full() map[string]string {
	m := map[string]string{"FF_JWT_ISSUER": "https://api.flowforge.example"}
	for _, v := range BackendVars {
		m[v] = "localhost:9" + v[3:4]
	}
	return m
}

func TestGatewayLoadConfig(t *testing.T) {
	c, err := LoadConfig(env(full()))
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":8080" || c.PublicURL != "https://api.flowforge.example" || c.Backends["FF_ENGINE_ADDR"] == "" {
		t.Fatalf("%+v", c)
	}
	m := full()
	m["PORT"], m["FF_COOKIE_INSECURE"], m["FF_JWT_AUDIENCE"] = "9000", "true", "flowforge-api"
	if c, err = LoadConfig(env(m)); err != nil || c.Addr != ":9000" || !c.CookieInsec {
		t.Fatalf("%+v %v", c, err)
	}
	m["FF_JWT_AUDIENCE"] = "other"
	if _, err := LoadConfig(env(m)); err == nil {
		t.Fatal("wrong audience accepted")
	}
	m = full()
	delete(m, "FF_GRAPH_ADDR")
	if _, err := LoadConfig(env(m)); err == nil || !strings.Contains(err.Error(), "FF_GRAPH_ADDR") {
		t.Fatalf("missing backend: %v", err)
	}
	m = full()
	delete(m, "FF_JWT_ISSUER")
	if _, err := LoadConfig(env(m)); err == nil {
		t.Fatal("missing issuer accepted")
	}
}

// TestRegress_G_SVC_24_ServeListenError: when the HTTP listener failed
// (address in use), Serve waited for the outbox relay, which only stopped
// with the caller's context, so the process hung instead of exiting with
// the error.
func TestRegress_G_SVC_24_ServeListenError(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg := Config{Addr: lis.Addr().String(), Issuer: "http://127.0.0.1", ShutdownGrace: time.Second}
	g, err := New(ctx, cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	backends := map[string]string{}
	for _, v := range BackendVars {
		backends[v] = "127.0.0.1:9"
	}
	done := make(chan error, 1)
	go func() { done <- g.Serve(ctx, backends) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "address already in use") {
			t.Fatalf("Serve on a used address: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Serve did not return after its listener failed")
	}
}
