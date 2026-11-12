package notifier_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/devstack"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	"github.com/genuinebnt/flowforge/services/notifier"
	"github.com/genuinebnt/flowforge/testkit"
)

const rule = `alert new_host when change.type == "entity_added" && entity.asset_type == "FQDN" -> webhook("hook") severity = High`

func st(m map[string]any) *structpb.Struct { s, _ := structpb.NewStruct(m); return s }

func change(id string) *pb.EventEnvelope {
	at := timestamppb.New(time.Date(2026, 11, 12, 14, 2, 12, 0, time.UTC))
	return &pb.EventEnvelope{EventId: id, Type: "graph.entity_added", SchemaVersion: 1,
		TenantId: core.PublicID(core.PrefixTenant, servicetest.TenantA), OccurredAt: at,
		Payload: &pb.EventEnvelope_GraphChange{GraphChange: &pb.ChangeEvent{Id: id, Seq: 1, Type: "entity_added", EntityId: "ent_01JD8K3F00000000000000000", At: at,
			Entity: &pb.Entity{Id: "ent_01JD8K3F00000000000000000", AssetType: "FQDN", Key: "fqdn:jenkins.acme.com", Content: st(map[string]any{"name": "jenkins.acme.com"}), ScopeStatus: "in_scope"}}}}
}

func TestNotifierService_Memory(t *testing.T)   { runNotifier(t, "") }
func TestNotifierService_Postgres(t *testing.T) { runNotifier(t, servicetest.FreshDB(t)) }

func runNotifier(t *testing.T, dsn string) {
	hook := servicetest.NewWebhook(t, "s3cret")
	s, a, b := servicetest.Stack(t, devstack.Options{DBDSN: dsn, AllowInsecureWebhooks: true,
		Secrets: notifier.NewMapSecrets(map[string]string{"test://hook": "s3cret"})})

	// A rule with errors cannot be enabled; disabled it is stored with its
	// diagnostics.
	_, err := s.Notifier.PutRule(a, &pb.PutRuleRequest{Name: "broken", Source: `alert x when entity.nope == 1 -> webhook("hook")`, Enabled: true})
	servicetest.IsKind(t, err, core.KindInvalidArgument, core.ReasonDSLDiagnostics)
	if len(err.(*core.Error).Details) == 0 {
		t.Fatal("diagnostics missing from the error")
	}
	broken, err := s.Notifier.PutRule(a, &pb.PutRuleRequest{Name: "broken", Source: `alert x when entity.nope == 1 -> webhook("hook")`})
	if err != nil || len(broken.GetDiagnostics()) == 0 || broken.GetEnabled() {
		t.Fatalf("disabled broken rule: %v %v", broken, err)
	}
	r, err := s.Notifier.PutRule(a, &pb.PutRuleRequest{Source: rule, Enabled: true})
	if err != nil || r.GetName() != "new_host" || r.GetVersion() != 1 {
		t.Fatalf("rule %v %v", r, err)
	}
	r2, err := s.Notifier.PutRule(a, &pb.PutRuleRequest{Id: r.GetId(), Source: rule, Enabled: true})
	if err != nil || r2.GetVersion() != 2 {
		t.Fatalf("update %v %v", r2, err)
	}
	if _, err := s.Notifier.PutChannel(a, &pb.PutChannelRequest{Kind: "webhook", Name: "hook", Enabled: true,
		Config: st(map[string]any{"url": hook.URL + "/x", "secret_ref": "test://hook"})}); err != nil {
		t.Fatal(err)
	}

	tr, err := s.Notifier.TestRule(a, &pb.TestRuleRequest{Source: rule, Sample: &pb.TestRuleRequest_Change{Change: change("chg_1").GetGraphChange()}})
	if err != nil || !tr.GetMatched() || tr.GetRenderedPayload().GetFields()["rule"].GetStringValue() != "new_host" {
		t.Fatalf("test rule %v %v", tr, err)
	}

	// Redelivered events create one alert (processed_events inbox).
	env := change("0190a1b2-0000-7000-8000-00000000c001")
	_ = s.Bus.Publish(a, "graph.changes", env)
	_ = s.Bus.Publish(a, "graph.changes", env)
	servicetest.Eventually(t, 20*time.Second, "delivery", func() bool { return len(hook.Received()) > 0 })
	time.Sleep(300 * time.Millisecond)
	evs, err := s.Notifier.ListAlertEvents(a, &pb.ListAlertEventsRequest{RuleId: r.GetId()})
	if err != nil || len(evs.GetEvents()) != 1 || len(hook.Received()) != 1 {
		t.Fatalf("alerts %v (%d deliveries) %v", evs, len(hook.Received()), err)
	}
	servicetest.Eventually(t, 5*time.Second, "alert sent", func() bool {
		evs, _ := s.Notifier.ListAlertEvents(a, &pb.ListAlertEventsRequest{Status: "sent"})
		return len(evs.GetEvents()) == 1
	})

	// Tenant isolation.
	_, err = s.Notifier.GetRule(b, &pb.GetRuleRequest{Id: r.GetId()})
	servicetest.IsKind(t, err, core.KindNotFound, "")
	_, err = s.Notifier.DeleteRule(b, &pb.DeleteRuleRequest{Id: r.GetId()})
	servicetest.IsKind(t, err, core.KindNotFound, "")
	if l, _ := s.Notifier.ListRules(b, &pb.ListRulesRequest{}); len(l.GetRules()) != 0 {
		t.Fatalf("B lists rules %v", l)
	}
	if l, _ := s.Notifier.ListChannels(b, &pb.ListChannelsRequest{}); len(l.GetChannels()) != 0 {
		t.Fatalf("B lists channels %v", l)
	}
	if l, _ := s.Notifier.ListAlertEvents(b, &pb.ListAlertEventsRequest{}); len(l.GetEvents()) != 0 {
		t.Fatalf("B lists alerts %v", l)
	}
	if _, err := s.Notifier.DeleteRule(a, &pb.DeleteRuleRequest{Id: r.GetId()}); err != nil {
		t.Fatal(err)
	}
}

type resolver map[string]string

func (r resolver) LookupAddrs(_ context.Context, h string) ([]netip.Addr, error) {
	a, ok := r[h]
	if !ok {
		return nil, scope.ErrNoSuchHost
	}
	return []netip.Addr{netip.MustParseAddr(a)}, nil
}

// TestWebhookSSRF: only https to public addresses (resolved and pinned).
func TestWebhookSSRF(t *testing.T) {
	s := &notifier.Senders{Secrets: notifier.NewMapSecrets(map[string]string{"k": "v"}), Resolver: resolver{"internal.example": "10.0.0.5", "meta.example": "169.254.169.254"}}
	for _, u := range []string{"http://hooks.example.com/x", "https://127.0.0.1/x", "https://internal.example/x", "https://meta.example/x", "https://user@hooks.example.com/"} {
		ch := notifier.Channel{Kind: notifier.KindWebhook, Config: map[string]any{"url": u, "secret_ref": "k"}}
		if _, err := s.Send(context.Background(), ch, "", notifier.Message{Body: []byte("{}"), At: time.Now()}); err == nil {
			t.Errorf("sent to %s", u)
		}
	}
}

// TestDeliveryRetry: a failing destination is retried on the spec 15.1
// schedule without blocking, then succeeds.
func TestDeliveryRetry(t *testing.T) {
	var fails atomic.Int32
	fails.Store(1)
	var ok atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fails.Add(-1) >= 0 {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		ok.Add(1)
	}))
	defer srv.Close()
	clock := testkit.NewFakeClock(time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC))
	repo := notifier.NewMemRepo()
	svc := notifier.NewService(notifier.Options{Repo: repo, Clock: clock,
		Senders: &notifier.Senders{Secrets: notifier.NewMapSecrets(map[string]string{"k": "v"}), AllowInsecureLoopback: true}})
	srvAPI := &notifier.Server{S: svc}
	ctx := platform.WithIdentity(context.Background(), platform.Identity{Tenant: servicetest.TenantA, Actor: servicetest.UserA, Role: "admin"})
	if _, err := srvAPI.PutRule(ctx, &pb.PutRuleRequest{Source: rule, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := srvAPI.PutChannel(ctx, &pb.PutChannelRequest{Kind: "webhook", Name: "hook", Enabled: true, Config: st(map[string]any{"url": srv.URL, "secret_ref": "k"})}); err != nil {
		t.Fatal(err)
	}
	if err := svc.HandleEvent(ctx, change("0190a1b2-0000-7000-8000-00000000c002")); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.DeliverDue(ctx); n != 1 || ok.Load() != 0 {
		t.Fatalf("first attempt: %d tried, %d ok", n, ok.Load())
	}
	if n, _ := svc.DeliverDue(ctx); n != 0 {
		t.Fatal("retried before the backoff")
	}
	clock.Advance(11 * time.Second)
	if n, _ := svc.DeliverDue(ctx); n != 1 || ok.Load() != 1 {
		t.Fatalf("retry: %d tried, %d ok", n, ok.Load())
	}
}
