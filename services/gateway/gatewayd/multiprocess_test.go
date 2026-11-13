package gatewayd_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/genuinebnt/flowforge/services/internal/servicetest"
)

// syncBuffer is a process's combined output.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestRegress_GOPS21_MultiProcessTopology is the local multi-process
// profile (deploy/compose.local.yml) without containers: every service is
// its own OS process built from cmd/, they share one Postgres database
// (migrated by cmd/migrate), the Postgres bus (FF_BUS=postgres), a local
// blob directory and one signing key from the environment, and the same
// REST flow as TestGatewayDevStackEndToEnd runs against the gateway
// process. It proves the outboxes, relays (identity included) and
// consumers work across processes, which one process cannot show.
func TestRegress_GOPS21_MultiProcessTopology(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and starts every binary")
	}
	dsn := servicetest.FreshDB(t)
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", bin+string(os.PathSeparator), "./cmd/gateway", "./cmd/scope", "./cmd/engine",
		"./cmd/tool-runner", "./cmd/egress-proxy", "./cmd/graph", "./cmd/similarity", "./cmd/notifier", "./cmd/migrate")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	if out, err := exec.Command(filepath.Join(bin, "migrate")).CombinedOutput(); err == nil {
		t.Fatalf("migrate without FF_DB_DSN succeeded: %s", out)
	}
	migrate := exec.Command(filepath.Join(bin, "migrate"))
	migrate.Env = append(os.Environ(), "FF_DB_DSN="+dsn)
	if out, err := migrate.CombinedOutput(); err != nil {
		t.Fatalf("migrate: %v\n%s", err, out)
	}

	seed := make([]byte, 32)
	_, _ = rand.Read(seed)
	addr := map[string]string{}
	for _, n := range []string{"gateway", "scope", "engine", "tool-runner", "egress-proxy", "graph", "similarity", "notifier"} {
		addr[n] = freeAddr(t)
	}
	common := []string{
		"FF_ENV=dev", "FF_DB_DSN=" + dsn, "FF_BUS=postgres", "FF_BLOB_DIR=" + t.TempDir(), "FF_LOG_LEVEL=debug",
		"FF_SIGNING_KID=local-1", "FF_SIGNING_KEY_LOCAL_1=" + hex.EncodeToString(seed),
		"FF_JWT_ISSUER=http://" + addr["gateway"],
		"FF_SCOPE_ADDR=" + addr["scope"], "FF_ENGINE_ADDR=" + addr["engine"], "FF_GRAPH_ADDR=" + addr["graph"],
		"FF_TOOL_RUNNER_ADDR=" + addr["tool-runner"], "FF_NOTIFIER_ADDR=" + addr["notifier"], "FF_SIMILARITY_ADDR=" + addr["similarity"],
		"FF_EGRESS_PROXY=http://" + addr["egress-proxy"],
	}
	logs := map[string]*syncBuffer{}
	var procs []*exec.Cmd
	t.Cleanup(func() {
		for _, p := range procs {
			_ = p.Process.Signal(syscall.SIGTERM)
		}
		for _, p := range procs {
			done := make(chan struct{})
			go func() { _ = p.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				_ = p.Process.Kill()
			}
		}
		if t.Failed() {
			for n, l := range logs {
				t.Logf("---- %s ----\n%s", n, tail(l.String(), 4000))
			}
		}
	})
	for _, n := range []string{"scope", "graph", "egress-proxy", "tool-runner", "engine", "similarity", "notifier", "gateway"} {
		cmd := exec.Command(filepath.Join(bin, n))
		listen := "FF_LISTEN_ADDR=" + addr[n]
		if n == "gateway" {
			listen = "FF_GATEWAY_ADDR=" + addr[n]
		}
		cmd.Env = append(append(os.Environ(), common...), listen)
		logs[n] = &syncBuffer{}
		cmd.Stdout, cmd.Stderr = logs[n], logs[n]
		if err := cmd.Start(); err != nil {
			t.Fatalf("start %s: %v", n, err)
		}
		procs = append(procs, cmd)
	}
	c := &client{t: t, base: "http://" + addr["gateway"] + "/v1"}
	c.waitReady("http://" + addr["gateway"] + "/readyz")
	c.waitFor(func() (string, bool) { return "", strings.Contains(logs["engine"].String(), "engine.tools_synced") })
	for _, n := range []string{"scope", "engine", "graph", "notifier"} {
		if !strings.Contains(logs[n].String(), `"bus":"postgres"`) {
			t.Fatalf("%s did not select the Postgres bus:\n%s", n, tail(logs[n].String(), 2000))
		}
	}
	gatewayFlow(t, c, logs["gateway"].String)

	// The events crossed processes on the Postgres bus: the notifier
	// acked auth.events, run.events and graph.changes deliveries.
	resp, err := http.Get("http://" + addr["gateway"] + "/readyz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("gateway not ready after the flow: %v", err)
	}
	resp.Body.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	waitAcked(ctx, t, dsn, "auth.events", "notifier")
	waitAcked(ctx, t, dsn, "run.events", "notifier")
	waitAcked(ctx, t, dsn, "graph.changes", "notifier")
}

func waitAcked(ctx context.Context, t *testing.T, dsn, topic, consumer string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	for {
		var n int
		err := conn.QueryRow(ctx, `SELECT count(*) FROM bus.deliveries WHERE topic = $1 AND consumer = $2 AND status = 'acked'`, topic, consumer).Scan(&n)
		if err == nil && n > 0 {
			return
		}
		if ctx.Err() != nil {
			t.Fatalf("no acked %s delivery for %s (%v)", topic, consumer, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
