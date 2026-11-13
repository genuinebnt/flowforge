package auth_test

import (
	"errors"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
)

// TestRegress_G_LIM_03_Calibrate: calibration raises iterations from the
// 13.1 floor until a hash reaches the 200-500 ms target, never goes below
// the configured parameters, and refuses a base below the floor.
func TestRegress_G_LIM_03_Calibrate(t *testing.T) {
	// A synthetic machine: 1 ms fixed plus 0.5 ms per MiB-iteration.
	measure := func(p auth.Params) time.Duration {
		return time.Millisecond + time.Duration(p.Time)*time.Duration(p.Memory/1024)*500*time.Microsecond
	}
	base := auth.PolicyParams() // 19 MiB, t=2: 20 ms here
	p, d, err := auth.Calibrate(base, 300*time.Millisecond, measure)
	if err != nil {
		t.Fatal(err)
	}
	if d < 300*time.Millisecond || d > auth.CalibrateMax || p.Memory != base.Memory || p.Threads != base.Threads || p.Time <= base.Time {
		t.Fatalf("calibrated %+v in %v", p, d)
	}
	if !p.MeetsPolicy() {
		t.Fatalf("calibrated below the floor: %+v", p)
	}
	// Already slow enough: unchanged.
	slow := base
	slow.Memory = 256 * 1024
	if q, _, _ := auth.Calibrate(slow, 200*time.Millisecond, measure); q != slow {
		t.Fatalf("slow base changed: %+v", q)
	}
	// Targets outside 200-500 ms are clamped.
	if _, d, _ := auth.Calibrate(base, 5*time.Second, measure); d > auth.CalibrateMax+50*time.Millisecond {
		t.Fatalf("target not clamped: %v", d)
	}
	weak := base
	weak.Time = 1
	if _, _, err := auth.Calibrate(weak, 300*time.Millisecond, measure); !errors.Is(err, auth.ErrWeakParams) {
		t.Fatalf("weak base: %v", err)
	}
}
