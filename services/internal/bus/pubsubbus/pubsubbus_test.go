package pubsubbus_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"cloud.google.com/go/pubsub/v2/apiv1/pubsubpb"
	"cloud.google.com/go/pubsub/v2/pstest"
	"google.golang.org/api/option"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/bus/bustest"
	"github.com/genuinebnt/flowforge/services/internal/bus/pubsubbus"
)

var projects atomic.Int64

// newBus returns an adapter on a fresh pstest server (the in-memory fake
// of the Pub/Sub API; ordering, ack, nack and dead-letter policies are
// implemented by it, retry backoff is not: a nack redelivers at once).
func newBus(t testing.TB, o pubsubbus.Options) (*pubsubbus.Bus, *pubsub.Client, *pstest.Server) {
	t.Helper()
	srv := pstest.NewServer()
	conn, err := grpc.NewClient(srv.Addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	c, err := pubsub.NewClient(context.Background(), fmt.Sprintf("ff-test-%d", projects.Add(1)), option.WithGRPCConn(conn))
	if err != nil {
		t.Fatal(err)
	}
	b := pubsubbus.New(c, o)
	t.Cleanup(func() {
		b.Close()
		_ = c.Close()
		_ = conn.Close()
		_ = srv.Close()
	})
	return b, c, srv
}

func TestBusContract_PubSub(t *testing.T) {
	bustest.Run(t, bustest.Options{MaxAttempts: 5, New: func(t *testing.T) bus.Bus {
		b, _, _ := newBus(t, pubsubbus.Options{Prefix: "ff-test-", CreateMissing: true, MaxAttempts: 5})
		return b
	}})
}

// TestRegress_GSVC01_PubSubAttributesAndNames pins what Terraform and
// operators rely on: the topic, dead-letter topic and subscription names of
// infra/modules/pubsub-topic, the ordering key of spec 11.7, the tenant_id
// and type attributes, and the dead-letter policy of created subscriptions.
func TestRegress_GSVC01_PubSubAttributesAndNames(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, c, srv := newBus(t, pubsubbus.Options{Prefix: "ff-lab-", CreateMissing: true, MaxAttempts: 7})
	if err := b.Subscribe(ctx, "approval.events", "notifier", func(context.Context, *pb.EventEnvelope) error { return nil }); err != nil {
		t.Fatal(err)
	}
	sub, err := c.SubscriptionAdminClient.GetSubscription(ctx, &pubsubpb.GetSubscriptionRequest{Subscription: b.SubscriptionName("approval.events", "notifier")})
	if err != nil {
		t.Fatal(err)
	}
	if want := "projects/" + c.Project() + "/subscriptions/ff-lab-approval.events.notifier"; sub.Name != want {
		t.Fatalf("subscription %s, want %s", sub.Name, want)
	}
	if !sub.EnableMessageOrdering || sub.DeadLetterPolicy.GetMaxDeliveryAttempts() != 7 ||
		sub.DeadLetterPolicy.GetDeadLetterTopic() != "projects/"+c.Project()+"/topics/ff-lab-approval.events.dead-letter" {
		t.Fatalf("subscription policy %+v", sub)
	}
	env := &pb.EventEnvelope{EventId: bustest.EventID(1), Type: "approval.requested", TenantId: "ten_x",
		Payload: &pb.EventEnvelope_ApprovalEvent{ApprovalEvent: &pb.ApprovalEventPayload{ApprovalId: "apr_1", Role: "admin", Status: "pending"}}}
	if err := b.Publish(ctx, "approval.events", env); err != nil {
		t.Fatal(err)
	}
	var m *pstest.Message
	for _, x := range srv.Messages() {
		m = x
	}
	if m == nil || m.OrderingKey != "apr_1" || m.Attributes["tenant_id"] != "ten_x" || m.Attributes["type"] != "approval.requested" ||
		m.Attributes["role"] != "admin" || m.Attributes["status"] != "pending" {
		t.Fatalf("published message %+v", m)
	}
}

// TestRegress_GSVC01_PubSubNoCreateWithoutTerraform: without CreateMissing
// (prod), subscribing to a subscription Terraform did not create fails
// instead of silently consuming nothing.
func TestRegress_GSVC01_PubSubNoCreateWithoutTerraform(t *testing.T) {
	b, _, _ := newBus(t, pubsubbus.Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := b.Subscribe(ctx, "run.events", "notifier", func(context.Context, *pb.EventEnvelope) error { return nil }); err == nil {
		t.Fatal("subscribe to a missing subscription succeeded")
	}
}
