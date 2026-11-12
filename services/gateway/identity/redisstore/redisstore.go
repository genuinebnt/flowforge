// Package redisstore implements the identity revocation cache (spec 13.3:
// deny:jti:<jti> and sv:<user_id>) and the login lockout state over Redis,
// so every gateway instance shares them.
package redisstore

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/services/gateway/identity"
)

// Revocations is identity.Revocations over Redis.
type Revocations struct {
	c redis.Cmdable
}

var _ identity.Revocations = (*Revocations)(nil)

// NewRevocations returns a revocation cache on c.
func NewRevocations(c redis.Cmdable) *Revocations { return &Revocations{c: c} }

// IsRevoked implements auth.RevocationChecker.
func (r *Revocations) IsRevoked(ctx context.Context, jti string) (bool, error) {
	n, err := r.c.Exists(ctx, "deny:jti:"+jti).Result()
	return n > 0, err
}

// SessionVersion implements auth.RevocationChecker.
func (r *Revocations) SessionVersion(ctx context.Context, subject string) (int64, bool, error) {
	v, err := r.c.Get(ctx, "sv:"+subject).Int64()
	if errors.Is(err, redis.Nil) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
}

// DenyJTI implements identity.Revocations.
func (r *Revocations) DenyJTI(ctx context.Context, jti string, ttl time.Duration) error {
	return r.c.Set(ctx, "deny:jti:"+jti, "1", ttl).Err()
}

// setMax stores ARGV[1] unless a larger version is already cached.
var setMax = redis.NewScript(`
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
local v = tonumber(ARGV[1])
if v < cur then v = cur end
redis.call('SET', KEYS[1], v, 'PX', ARGV[2])
return v`)

// SetSessionVersion implements identity.Revocations. A version never moves
// backwards.
func (r *Revocations) SetSessionVersion(ctx context.Context, subject string, v int64, ttl time.Duration) error {
	return setMax.Run(ctx, r.c, []string{"sv:" + subject}, v, ttl.Milliseconds()).Err()
}

// Lockouts is auth.LockoutStore over Redis: a hash lock:<account key> with
// fields failures and until (Unix ms).
type Lockouts struct {
	c redis.Cmdable
	// Window bounds how long failure counts are kept (default 24h).
	Window time.Duration
}

var _ auth.LockoutStore = (*Lockouts)(nil)

// NewLockouts returns a lockout store on c.
func NewLockouts(c redis.Cmdable) *Lockouts { return &Lockouts{c: c, Window: 24 * time.Hour} }

// Get implements auth.LockoutStore.
func (l *Lockouts) Get(ctx context.Context, key string) (auth.Lockout, error) {
	vals, err := l.c.HMGet(ctx, "lock:"+key, "failures", "until").Result()
	if err != nil {
		return auth.Lockout{}, err
	}
	var out auth.Lockout
	if s, ok := vals[0].(string); ok {
		out.Failures, _ = strconv.Atoi(s)
	}
	if s, ok := vals[1].(string); ok {
		if ms, err := strconv.ParseInt(s, 10, 64); err == nil && ms > 0 {
			out.LockedUntil = time.UnixMilli(ms).UTC()
		}
	}
	return out, nil
}

var recordFailure = redis.NewScript(`
local n = redis.call('HINCRBY', KEYS[1], 'failures', 1)
if n >= tonumber(ARGV[1]) then
  redis.call('HSET', KEYS[1], 'failures', 0, 'until', ARGV[2])
  n = 0
end
redis.call('PEXPIRE', KEYS[1], ARGV[3])
return n`)

// RecordFailure implements auth.LockoutStore.
func (l *Lockouts) RecordFailure(ctx context.Context, key string, now time.Time, threshold int, cooldown time.Duration) (auth.Lockout, error) {
	until := now.Add(cooldown)
	window := max(l.Window, cooldown)
	n, err := recordFailure.Run(ctx, l.c, []string{"lock:" + key}, threshold, until.UnixMilli(), window.Milliseconds()).Int()
	if err != nil {
		return auth.Lockout{}, err
	}
	if n == 0 {
		return auth.Lockout{LockedUntil: until}, nil
	}
	return auth.Lockout{Failures: n}, nil
}

// Reset implements auth.LockoutStore.
func (l *Lockouts) Reset(ctx context.Context, key string) error {
	return l.c.Del(ctx, "lock:"+key).Err()
}
