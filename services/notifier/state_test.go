package notifier_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/migrations"
	"github.com/genuinebnt/flowforge/services/internal/servicetest"
	"github.com/genuinebnt/flowforge/services/notifier"
	"github.com/genuinebnt/flowforge/testkit"
)

func TestRegress_GLIM06_ThrottleSurvivesRestart_Memory(t *testing.T) {
	throttleRestart(t, notifier.NewMemRepo())
}

func TestRegress_GLIM06_ThrottleSurvivesRestart_Postgres(t *testing.T) {
	dsn := servicetest.FreshDB(t)
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := migrations.Apply(context.Background(), pool); err != nil {
		t.Fatal(err)
	}
	throttleRestart(t, &notifier.PgRepo{DB: pool})
}

// throttleRestart: two events with the same dedupe key, a notifier restart
// in between (a new Service on the same repository). The second is
// suppressed: one alert per throttle window. A digest opened before the
// restart is flushed after it with both matches. A third event after the
// window fires again.
func throttleRestart(t *testing.T, repo notifier.Repo) {
	ctx := context.Background()
	clock := testkit.NewFakeClock(time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC))
	for i, src := range []string{
		`alert new_host when change.type == "entity_added" -> webhook("hook") group_by entity.key throttle 1h`,
		`alert host_digest when change.type == "entity_added" -> webhook("hook") digest 15m`,
	} {
		if _, err := repo.PutRule(ctx, notifier.Rule{ID: core.MustParseUUID(fmt.Sprintf("0190a1b2-0000-7000-8000-0000000000e%d", i)),
			Tenant: servicetest.TenantA, Name: fmt.Sprint("r", i), Source: src, Enabled: true,
			CreatedBy: servicetest.UserA, CreatedAt: clock.Now()}, true); err != nil {
			t.Fatal(err)
		}
	}
	newSvc := func() *notifier.Service {
		return notifier.NewService(notifier.Options{Repo: repo, Clock: clock, Senders: &notifier.Senders{}})
	}
	count := func(status string) int {
		evs, err := repo.ListAlertEvents(ctx, servicetest.TenantA, core.Nil, status, core.Nil, 100)
		if err != nil {
			t.Fatal(err)
		}
		return len(evs)
	}
	if err := newSvc().HandleEvent(ctx, change("0190a1b2-0000-7000-8000-00000000d001")); err != nil {
		t.Fatal(err)
	}
	clock.Advance(10 * time.Minute)
	restarted := newSvc()
	if err := restarted.HandleEvent(ctx, change("0190a1b2-0000-7000-8000-00000000d002")); err != nil {
		t.Fatal(err)
	}
	// Throttle rule: 1 pending + 1 suppressed; digest rule: 2 digested
	// (pending) matches, not yet delivered.
	if s := count("suppressed"); s != 1 {
		t.Fatalf("after restart: %d suppressed alerts, want 1 (pending %d)", s, count("pending"))
	}
	clock.Advance(6 * time.Minute) // the digest window (opened before the restart) has ended
	restarted.FlushDigests(ctx)
	evs, _ := repo.ListAlertEvents(ctx, servicetest.TenantA, core.MustParseUUID("0190a1b2-0000-7000-8000-0000000000e1"), "pending", core.Nil, 100)
	digests := 0
	for _, e := range evs {
		if e.TriggerEventID == e.ID {
			digests++
			var body struct {
				Digest struct {
					Count int `json:"count"`
				} `json:"digest"`
			}
			if err := json.Unmarshal(e.Payload, &body); err != nil || body.Digest.Count != 2 {
				t.Fatalf("digest payload %s: want count 2 (%v)", e.Payload, err)
			}
		}
	}
	if digests != 1 {
		t.Fatalf("%d digests flushed after restart, want 1", digests)
	}
	clock.Advance(time.Hour)
	if err := newSvc().HandleEvent(ctx, change("0190a1b2-0000-7000-8000-00000000d003")); err != nil {
		t.Fatal(err)
	}
	if s := count("suppressed"); s != 1 {
		t.Fatalf("after the window: %d suppressed, want still 1", s)
	}
	if n, err := repo.PruneAlertState(ctx, clock.Now().Add(2*time.Hour), 100); err != nil || n != 1 {
		t.Fatalf("prune %d %v", n, err)
	}
}
