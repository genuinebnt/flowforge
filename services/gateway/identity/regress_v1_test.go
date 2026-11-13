package identity_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/auth"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/notifier"
)

// TestRegress_G_LIM_07_SignupThrottledEvent: a throttled signup writes
// auth.signup_throttled (and a throttled login auth.login_throttled) to
// identity.outbox, and the default signup-abuse rule matches both events
// once the notifier has turned them into rule input.
func TestRegress_G_LIM_07_SignupThrottledEvent(t *testing.T) {
	ctx := context.Background()
	f, _ := newFixture(t)
	ci := identity.ClientInfo{IP: "198.51.100.7"}
	lim := identity.DefaultLimits().Signup.Count
	for i := 0; i <= lim; i++ {
		_, err := f.Service.Signup(ctx, identity.SignupInput{Email: "u" + string(rune('a'+i)) + "@example.com",
			Password: "correct horse battery staple!", TenantName: "x"}, ci)
		if i < lim && err != nil {
			t.Fatalf("signup %d: %v", i, err)
		}
		if i == lim && reason(err) != "RATE_LIMITED" {
			t.Fatalf("signup %d not throttled: %v", i, err)
		}
	}
	for range auth.DefaultLoginIPLimit().Count + 1 {
		_, _ = f.Service.Login(ctx, identity.LoginInput{Email: "nobody@example.com", Password: "wrong password!!"}, ci)
	}

	rule, ds := alerts.Compile(alerts.DefaultRuleSignupAbuse)
	if rule == nil {
		t.Fatalf("default rule: %v", ds)
	}
	rs := alerts.NewRuleSet(alerts.RenderOptions{})
	if err := rs.Add("ops_signup_abuse", rule); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, o := range f.Mem.Outbox() {
		if o.Topic != identity.Topic || identity.Topic != convert.TopicAuthEvents {
			t.Fatalf("topic %q", o.Topic)
		}
		var p struct {
			EventID string `json:"event_id"`
			Type    string `json:"type"`
		}
		if err := json.Unmarshal(o.Payload, &p); err != nil {
			t.Fatal(err)
		}
		if p.Type != "auth."+identity.EventSignupThrottled && p.Type != "auth."+identity.EventLoginThrottled {
			continue
		}
		seen[p.Type] = true
		env := &pb.EventEnvelope{EventId: p.EventID, Type: p.Type, SchemaVersion: 1}
		if ms := rs.Evaluate(notifier.EventFromEnvelope(env)).Matches; len(ms) != 1 {
			t.Fatalf("%s: default rule matches %+v", p.Type, ms)
		}
	}
	if !seen["auth.signup_throttled"] || !seen["auth.login_throttled"] {
		t.Fatalf("throttle events in outbox: %v", seen)
	}
}
