package gatewayd

import (
	"strings"
	"testing"
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
