// Command flowforge is the developer CLI. "flowforge dev" runs every
// internal service in one process on loopback ports with in-memory stores
// (or Postgres when FF_DB_DSN or -db is set) and prints their addresses.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sort"
	"syscall"

	"github.com/genuinebnt/flowforge/services/app"
	"github.com/genuinebnt/flowforge/services/devstack"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 || os.Args[1] != "dev" {
		fmt.Fprintln(os.Stderr, "usage: flowforge dev [-db DSN] [-host 127.0.0.1] [-base-port 0] [-log info]")
		os.Exit(2)
	}
	if err := dev(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "flowforge dev:", err)
		os.Exit(1)
	}
}

func dev(args []string) error {
	fs := flag.NewFlagSet("dev", flag.ExitOnError)
	db := fs.String("db", os.Getenv("FF_DB_DSN"), "Postgres DSN (empty: in-memory stores)")
	host := fs.String("host", "127.0.0.1", "listen host")
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
	st, err := devstack.Start(ctx, devstack.Options{DBDSN: *db, Host: *host, Ports: ports, Logger: log})
	if err != nil {
		return err
	}
	store := "in-memory stores"
	if *db != "" {
		store = "Postgres"
	}
	fmt.Printf("flowforge %s dev stack (%s), internal tokens signed by an ephemeral dev key:\n", version, store)
	names := make([]string, 0, len(st.Addrs))
	for n := range st.Addrs {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Printf("  %-13s %s\n", n, st.Addrs[n])
	}
	log.LogAttrs(ctx, slog.LevelInfo, "dev.ready")
	<-ctx.Done()
	fmt.Println("shutting down")
	return st.Close()
}
