// Package busconfig opens the bus.Bus adapter a deployment profile asks
// for (FF_BUS, FF_PUBSUB_PROJECT, FF_TOPIC_PREFIX; G-SVC-01). It is the
// one place that knows every adapter, so the services and the gateway
// select a bus the same way and bus itself stays free of the Pub/Sub SDK.
package busconfig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"cloud.google.com/go/pubsub/v2"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/bus/pubsubbus"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// Kinds of FF_BUS.
const (
	KindMem      = "mem"
	KindPostgres = "postgres"
	KindPubSub   = "pubsub"
)

// Config selects and configures the adapter.
type Config struct {
	// Kind is FF_BUS. Empty picks the default: pubsub when Project is set,
	// postgres when Pool is set, else mem.
	Kind string
	// Pool is the service's database (required for postgres).
	Pool *pgxpool.Pool
	// Project is FF_PUBSUB_PROJECT; Prefix is FF_TOPIC_PREFIX.
	Project, Prefix string
	// CreateMissing creates Pub/Sub topics and subscriptions (the emulator,
	// PUBSUB_EMULATOR_HOST, or a lab project without Terraform-made ones).
	CreateMissing bool
	Logger        *slog.Logger
	Metrics       *platform.Metrics
}

// Resolve returns the effective kind of c.
func Resolve(c Config) (string, error) {
	switch c.Kind {
	case KindMem, KindPostgres, KindPubSub:
		return c.Kind, nil
	case "":
		switch {
		case c.Project != "":
			return KindPubSub, nil
		case c.Pool != nil:
			return KindPostgres, nil
		}
		return KindMem, nil
	}
	return "", fmt.Errorf("FF_BUS must be mem, postgres or pubsub, got %q", c.Kind)
}

// Open returns the bus and a function that releases it.
func Open(ctx context.Context, c Config) (bus.Bus, func(), error) {
	kind, err := Resolve(c)
	if err != nil {
		return nil, nil, err
	}
	switch kind {
	case KindPostgres:
		if c.Pool == nil {
			return nil, nil, errors.New("FF_BUS=postgres needs FF_DB_DSN")
		}
		return bus.NewPgBus(c.Pool, c.Logger, c.Metrics), func() {}, nil
	case KindPubSub:
		if c.Project == "" {
			return nil, nil, errors.New("FF_BUS=pubsub needs FF_PUBSUB_PROJECT")
		}
		// NewClient honours PUBSUB_EMULATOR_HOST and otherwise uses
		// Application Default Credentials (the Cloud Run service account).
		client, err := pubsub.NewClient(ctx, c.Project)
		if err != nil {
			return nil, nil, fmt.Errorf("pubsub: %w", err)
		}
		b := pubsubbus.New(client, pubsubbus.Options{Prefix: c.Prefix, CreateMissing: c.CreateMissing, Logger: c.Logger, Metrics: c.Metrics})
		return b, func() { b.Close(); _ = client.Close() }, nil
	}
	return bus.NewMem(c.Logger, c.Metrics), func() {}, nil
}
