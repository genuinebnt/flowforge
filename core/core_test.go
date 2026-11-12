package core_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time           { return c.t }
func (c fixedClock) Monotonic() time.Duration { return 0 }

func TestCore001UUIDv7Layout(t *testing.T) {
	clk := fixedClock{time.UnixMilli(0x0123456789ab)}
	u := core.NewUUIDv7(clk, core.NewSplitMix64(1))
	if got := u[6] >> 4; got != 7 {
		t.Fatalf("version nibble = %d, want 7", got)
	}
	if got := u[8] >> 6; got != 2 {
		t.Fatalf("variant bits = %b, want 10", got)
	}
	if !strings.HasPrefix(u.String(), "01234567-89ab-7") {
		t.Fatalf("timestamp prefix wrong: %s", u)
	}
	back, err := core.ParseUUID(u.String())
	if err != nil || back != u {
		t.Fatalf("round trip: %v %v", back, err)
	}
}

func TestCore002PublicIDRoundTripAndOrder(t *testing.T) {
	rng := core.NewSplitMix64(42)
	var prev string
	for i := range 1000 {
		u := core.NewUUIDv7(fixedClock{time.UnixMilli(int64(1_700_000_000_000 + i))}, rng)
		id := core.PublicID(core.PrefixRun, u)
		if !strings.HasPrefix(id, "run_") || len(id) != 30 {
			t.Fatalf("bad id %q", id)
		}
		back, err := core.ParsePublicID(core.PrefixRun, id)
		if err != nil || back != u {
			t.Fatalf("round trip %q: %v", id, err)
		}
		if prev != "" && id <= prev {
			t.Fatalf("ids not time ordered: %s <= %s", id, prev)
		}
		prev = id
	}
	if _, err := core.ParsePublicID(core.PrefixProgram, prev); !errors.Is(err, core.ErrInvalidID) {
		t.Fatalf("wrong prefix accepted: %v", err)
	}
}

func TestCore003CanonicalJSON(t *testing.T) {
	a, err := core.CanonicalJSON(map[string]any{"b": 1, "a": []any{"<x>", 2.5}, "c": map[string]any{"z": true, "y": nil}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":["<x>",2.5],"b":1,"c":{"y":null,"z":true}}`
	if string(a) != want {
		t.Fatalf("got %s want %s", a, want)
	}
}

func TestCore004DecodeStrict(t *testing.T) {
	var v struct {
		A int `json:"a"`
	}
	if err := core.DecodeStrictBytes([]byte(`{"a":1}`), &v); err != nil || v.A != 1 {
		t.Fatalf("valid decode failed: %v", err)
	}
	if err := core.DecodeStrictBytes([]byte(`{"a":1,"b":2}`), &v); err == nil {
		t.Fatal("unknown field accepted")
	}
	if err := core.DecodeStrictBytes([]byte(`{"a":1} {}`), &v); err == nil {
		t.Fatal("trailing data accepted")
	}
}

func TestCore005ErrorIs(t *testing.T) {
	err := core.Errorf(core.KindPermissionDenied, core.ReasonScopeDenied, "target %s", "x")
	if !errors.Is(err, core.ErrPermissionDenied) {
		t.Fatal("kind match failed")
	}
	if !errors.Is(err, &core.Error{Kind: core.KindPermissionDenied, Reason: core.ReasonScopeDenied}) {
		t.Fatal("reason match failed")
	}
	if errors.Is(err, &core.Error{Kind: core.KindPermissionDenied, Reason: core.ReasonForbiddenRole}) {
		t.Fatal("wrong reason matched")
	}
}

func TestCore006ActivityClassOrder(t *testing.T) {
	if !(core.ClassNone < core.ClassPassive && core.ClassPassive < core.ClassActive && core.ClassActive < core.ClassIntrusive) {
		t.Fatal("class order broken")
	}
	for _, s := range []string{"none", "passive", "active", "intrusive"} {
		c, err := core.ParseActivityClass(s)
		if err != nil || c.String() != s {
			t.Fatalf("%s: %v %v", s, c, err)
		}
	}
}

func TestCore007SplitMixDeterministic(t *testing.T) {
	a, b := core.NewSplitMix64(7), core.NewSplitMix64(7)
	for range 100 {
		if a.Uint64() != b.Uint64() {
			t.Fatal("same seed diverged")
		}
	}
}
