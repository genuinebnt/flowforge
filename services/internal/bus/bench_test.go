package bus_test

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/bus/bustest"
	"github.com/genuinebnt/flowforge/services/internal/bus/pubsubbus"
)

// BenchmarkBusAlgo compares the Bus adapters (docs/ALGORITHMS.md, "event
// bus"): latency is one publish followed by waiting for its delivery
// (ns/op is the round trip); throughput publishes batches of 100 on 10
// ordering keys and waits for all of them (msgs/s). postgres needs
// FF_TEST_PG_DSN; pubsub-pstest is the in-memory Pub/Sub fake, so it
// measures the client library and gRPC, not the Pub/Sub service.
func BenchmarkBusAlgo(b *testing.B) {
	adapters := []struct {
		name string
		open func(b *testing.B) bus.Bus
	}{
		{"mem", func(*testing.B) bus.Bus { return bus.NewMem(nil, nil) }},
		{"postgres", func(b *testing.B) bus.Bus {
			if os.Getenv("FF_TEST_PG_DSN") == "" {
				b.Skip("FF_TEST_PG_DSN not set")
			}
			pg := newPgBus(b, pgPool(b))
			pg.Poll = time.Second // wakeups come from LISTEN/NOTIFY
			return pg
		}},
		{"pubsub-pstest", func(b *testing.B) bus.Bus {
			srv := pstest.NewServer()
			conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				b.Fatal(err)
			}
			c, err := pubsub.NewClient(context.Background(), "ff-bench", option.WithGRPCConn(conn))
			if err != nil {
				b.Fatal(err)
			}
			ps := pubsubbus.New(c, pubsubbus.Options{CreateMissing: true})
			b.Cleanup(func() { ps.Close(); _ = c.Close(); _ = conn.Close(); _ = srv.Close() })
			return ps
		}},
	}
	for _, a := range adapters {
		b.Run(a.name+"/latency", func(b *testing.B) {
			bb := a.open(b)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			got := make(chan struct{}, 1)
			if err := bb.Subscribe(ctx, "run.events", "bench", func(context.Context, *pb.EventEnvelope) error {
				got <- struct{}{}
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			i := 0
			for b.Loop() {
				i++
				if err := bb.Publish(ctx, "run.events", bustest.RunEnvelope(i, "run_bench", int64(i))); err != nil {
					b.Fatal(err)
				}
				select {
				case <-got:
				case <-time.After(30 * time.Second):
					b.Fatal("no delivery")
				}
			}
		})
		b.Run(a.name+"/throughput", func(b *testing.B) {
			bb := a.open(b)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var n atomic.Int64
			if err := bb.Subscribe(ctx, "run.events", "bench", func(context.Context, *pb.EventEnvelope) error {
				n.Add(1)
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			const batch = 100
			start := time.Now()
			id := 0
			for b.Loop() {
				for j := range batch {
					id++
					if err := bb.Publish(ctx, "run.events", bustest.RunEnvelope(id, fmt.Sprintf("run_%d", j%10), int64(id))); err != nil {
						b.Fatal(err)
					}
				}
				deadline := time.Now().Add(60 * time.Second)
				for n.Load() < int64(id) {
					if time.Now().After(deadline) {
						b.Fatalf("delivered %d of %d", n.Load(), id)
					}
					time.Sleep(50 * time.Microsecond)
				}
			}
			b.ReportMetric(float64(id)/time.Since(start).Seconds(), "msgs/s")
		})
	}
}
