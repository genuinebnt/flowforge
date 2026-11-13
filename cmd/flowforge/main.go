// Command flowforge is the developer CLI. "flowforge dev" runs every
// internal service plus the public REST gateway in one process on loopback
// ports, with in-memory stores (or Postgres when FF_DB_DSN or -db is set),
// and prints their addresses.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/services/app"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/gateway/gatewayd"
)

// version is set at link time with -ldflags "-X main.version=..." and
// printed in the dev stack banner.
var version = "dev"

func main() {
	if len(os.Args) < 2 || os.Args[1] != "dev" {
		fmt.Fprintln(os.Stderr, "usage: flowforge dev [-db DSN] [-host 127.0.0.1] [-gateway 127.0.0.1:8080] [-base-port 0] [-log info]")
		os.Exit(2)
	}
	if err := dev(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "flowforge dev:", err)
		os.Exit(1)
	}
}

// backendVar maps devstack service names to the gateway's backend
// configuration keys.
var backendVar = map[string]string{
	devstack.ScopeName:      "FF_SCOPE_ADDR",
	devstack.EngineName:     "FF_ENGINE_ADDR",
	devstack.GraphName:      "FF_GRAPH_ADDR",
	devstack.NotifierName:   "FF_NOTIFIER_ADDR",
	devstack.ToolRunnerName: "FF_TOOL_RUNNER_ADDR",
	devstack.SimilarityName: "FF_SIMILARITY_ADDR",
}

func dev(args []string) error {
	fs := flag.NewFlagSet("dev", flag.ExitOnError)
	db := fs.String("db", os.Getenv("FF_DB_DSN"), "Postgres DSN (empty: in-memory stores)")
	host := fs.String("host", "127.0.0.1", "listen host for internal services")
	gwAddr := fs.String("gateway", "127.0.0.1:8080", "REST gateway listen address (empty: no gateway)")
	base := fs.Int("base-port", 0, "first port (services get consecutive ports; 0: random)")
	level := fs.String("log", "info", "log level")
	_ = fs.Parse(args)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	ports := map[string]int{}
	if *base > 0 {
		for i, n := range []string{devstack.ScopeName, devstack.EngineName, devstack.ToolRunnerName, devstack.EgressName,
			devstack.GraphName, devstack.SimilarityName, devstack.NotifierName} {
			ports[n] = *base + i
		}
	}
	log := app.Logger("flowforge-dev", "dev", *level)

	// The gateway's identity side comes first: internal services must
	// trust the keys it signs internal tokens with.
	opts := devstack.Options{DBDSN: *db, Host: *host, Ports: ports, Logger: log}
	if dir := os.Getenv("FF_BLOB_DIR"); dir != "" {
		// Raw tool output, snapshot files and run archives (G-OPS-14).
		opts.Blobs = graphstore.DirBlobs{Root: dir}
	}
	var gw *gatewayd.Gateway
	if *gwAddr != "" {
		issuer := "http://" + *gwAddr
		var err error
		gw, err = gatewayd.New(ctx, gatewayd.Config{
			Addr: *gwAddr, DBDSN: *db, Migrate: *db != "", Issuer: issuer, PublicURL: issuer,
			CookieInsec: true, ShutdownGrace: 5 * time.Second,
		}, log)
		if err != nil {
			return fmt.Errorf("gateway: %w", err)
		}
		defer gw.Close()
		opts.Issuer, opts.TrustKeys = issuer, gw.Keys()
	}

	st, err := devstack.Start(ctx, opts)
	if err != nil {
		return err
	}
	gwErr := make(chan error, 1)
	if gw != nil {
		backends := map[string]string{}
		for name, v := range backendVar {
			backends[v] = st.Addrs[name]
		}
		// auth.events go onto the stack's bus, where the notifier consumes them.
		gw.UseBus(st.Bus)
		go func() { gwErr <- gw.Serve(ctx, backends) }()
	}

	store := "in-memory stores"
	if *db != "" {
		store = "Postgres"
	}
	fmt.Printf("flowforge %s dev stack (%s):\n", version, store)
	if gw != nil {
		fmt.Printf("  %-13s http://%s/v1\n", "gateway", *gwAddr)
	}
	names := make([]string, 0, len(st.Addrs))
	for n := range st.Addrs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("  %-13s %s\n", n, st.Addrs[n])
	}
	log.LogAttrs(ctx, slog.LevelInfo, "dev.ready")

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-gwErr:
		stop()
	}
	fmt.Println("shutting down")
	if gw != nil && serveErr == nil {
		serveErr = <-gwErr
	}
	return errors.Join(serveErr, st.Close())
}
