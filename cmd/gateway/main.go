// Command gateway runs the Flowforge public REST gateway and identity
// service (spec 12, 13). Configuration comes from the environment
// (spec 17.5); see gatewayd.LoadConfig.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/genuinebnt/flowforge/services/gateway/gatewayd"
)

func main() {
	// FF_LOG_LEVEL as for the internal services (default info); debug
	// includes the dev e-mail sender's messages (verification links).
	level := slog.LevelInfo
	if v := os.Getenv("FF_LOG_LEVEL"); v != "" {
		_ = level.UnmarshalText([]byte(v))
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	cfg, err := gatewayd.LoadConfig(os.Getenv)
	if err != nil {
		log.Error("gateway: configuration", "err", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	g, err := gatewayd.New(ctx, cfg, log)
	if err != nil {
		log.Error("gateway: start", "err", err)
		os.Exit(1)
	}
	defer g.Close()
	if err := g.Serve(ctx, cfg.Backends); err != nil {
		log.Error("gateway: exit", "err", err)
		os.Exit(1)
	}
}
