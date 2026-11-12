package redisstore_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/genuinebnt/flowforge/services/gateway/identity/redisstore"
)

func client(t *testing.T) *redis.Client {
	addr := os.Getenv("FF_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("FF_TEST_REDIS_ADDR not set")
	}
	c := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { _ = c.Close() })
	if err := c.FlushDB(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestIdentityRedisRevocations(t *testing.T) {
	ctx := context.Background()
	r := redisstore.NewRevocations(client(t))
	if ok, _ := r.IsRevoked(ctx, "j1"); ok {
		t.Fatal("revoked before deny")
	}
	if err := r.DenyJTI(ctx, "j1", time.Minute); err != nil {
		t.Fatal(err)
	}
	if ok, _ := r.IsRevoked(ctx, "j1"); !ok {
		t.Fatal("deny lost")
	}
	if _, ok, _ := r.SessionVersion(ctx, "usr_x"); ok {
		t.Fatal("version before set")
	}
	_ = r.SetSessionVersion(ctx, "usr_x", 3, time.Minute)
	_ = r.SetSessionVersion(ctx, "usr_x", 2, time.Minute)
	if v, ok, _ := r.SessionVersion(ctx, "usr_x"); !ok || v != 3 {
		t.Fatalf("version %d %v, want 3 (never backwards)", v, ok)
	}
}

func TestIdentityRedisLockouts(t *testing.T) {
	ctx := context.Background()
	l := redisstore.NewLockouts(client(t))
	now := time.Unix(1_800_000_000, 0).UTC()
	for i := 1; i < 3; i++ {
		got, err := l.RecordFailure(ctx, "acct", now, 3, 15*time.Minute)
		if err != nil || got.Failures != i {
			t.Fatalf("failure %d: %+v %v", i, got, err)
		}
	}
	got, _ := l.RecordFailure(ctx, "acct", now, 3, 15*time.Minute)
	if !got.LockedUntil.Equal(now.Add(15 * time.Minute)) {
		t.Fatalf("third failure: %+v", got)
	}
	if g, _ := l.Get(ctx, "acct"); !g.LockedUntil.Equal(now.Add(15 * time.Minute)) {
		t.Fatalf("get: %+v", g)
	}
	_ = l.Reset(ctx, "acct")
	if g, _ := l.Get(ctx, "acct"); g.Failures != 0 || !g.LockedUntil.IsZero() {
		t.Fatalf("after reset: %+v", g)
	}
}
