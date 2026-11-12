package eventstore_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/eventstore/storetest"
	"github.com/genuinebnt/flowforge/testkit"
)

func memHarness(testing.TB) *storetest.Harness {
	clk := testkit.NewFakeClock(time.Date(2026, 11, 12, 9, 0, 0, 0, time.UTC))
	clk.SetMonotonic(time.Hour)
	return &storetest.Harness{
		Store: eventstore.NewMemStore(clk),
		Now:   func(context.Context) (time.Time, error) { return clk.Now(), nil },
		Elapse: func(_ context.Context, d time.Duration) error {
			clk.Advance(d)
			return nil
		},
	}
}

func newMem(t *testing.T) *storetest.Harness { return memHarness(t) }

func TestES001_Append(t *testing.T)                  { storetest.ES001(t, newMem) }
func TestES002_StaleSeq(t *testing.T)                { storetest.ES002(t, newMem) }
func TestES003_StaleFence(t *testing.T)              { storetest.ES003(t, newMem) }
func TestES004_LeaseExpiry(t *testing.T)             { storetest.ES004(t, newMem) }
func TestES005_Race(t *testing.T)                    { storetest.ES005(t, newMem) }
func TestES006_ReadYourWritesSubscribe(t *testing.T) { storetest.ES006(t, newMem) }
func TestES007_AllImplementations(t *testing.T)      { storetest.Run(t, newMem) }
func TestESProp_RandomInterleavings(t *testing.T)    { storetest.PropRandomInterleavings(t, newMem) }
func TestESScripts_Conformance(t *testing.T)         { storetest.Scripts(t, newMem) }

func TestFence_CompareLexicographic(t *testing.T) {
	cases := []struct {
		a, b eventstore.Fence
		want int
	}{
		{eventstore.Fence{}, eventstore.Fence{}, 0},
		{eventstore.Fence{Term: 0, Counter: 9}, eventstore.Fence{Term: 1, Counter: 0}, -1},
		{eventstore.Fence{Term: 2, Counter: 0}, eventstore.Fence{Term: 1, Counter: 99}, 1},
		{eventstore.Fence{Term: 1, Counter: 3}, eventstore.Fence{Term: 1, Counter: 4}, -1},
	}
	for _, c := range cases {
		if got := c.a.Compare(c.b); got != c.want {
			t.Errorf("%s.Compare(%s) = %d, want %d", c.a, c.b, got, c.want)
		}
		if got := c.b.Compare(c.a); got != -c.want {
			t.Errorf("%s.Compare(%s) = %d, want %d", c.b, c.a, got, -c.want)
		}
	}
}

func TestShardOf_UniformAndStable(t *testing.T) {
	rng := testkit.Rng(t)
	clk := testkit.NewFakeClock(time.Unix(1_700_000_000, 0))
	const n, shards = 8000, 8
	counts := make([]int, shards)
	for range n {
		id := core.NewUUIDv7(clk, rng)
		s := eventstore.ShardOf(id, shards)
		if s != eventstore.ShardOf(id, shards) || !(eventstore.Shard{Index: s, Count: shards}).Contains(id) {
			t.Fatal("ShardOf is not stable")
		}
		counts[s]++
	}
	for i, c := range counts {
		if c < n/shards*8/10 || c > n/shards*12/10 {
			t.Errorf("shard %d has %d of %d runs (counts %v)", i, c, n, counts)
		}
	}
	id := core.MustParseUUID("0190a1b2-c3d4-7e5f-8a6b-1c2d3e4f5a6b")
	// Pinned so the SQL expression in pgstore stays in step:
	// ('x' || right(replace(id::text, '-', ''), 8))::bit(32)::bigint % 7
	if got := eventstore.ShardOf(id, 7); got != 0x3e4f5a6b%7 {
		t.Errorf("ShardOf = %d, want %d", got, 0x3e4f5a6b%7)
	}
	if eventstore.ShardOf(id, 0) != 0 || !eventstore.AllShards.Contains(id) {
		t.Error("the zero shard must select every run")
	}
}

func TestPrepareEvents_Canonical(t *testing.T) {
	run := core.MustParseUUID("0190a1b2-c3d4-7e5f-8a6b-1c2d3e4f5a6b")
	at := time.Date(2026, 1, 2, 3, 4, 5, 6789, time.FixedZone("x", 3600))
	out, err := eventstore.PrepareEvents(run, 7, []eventstore.Event{
		{Type: "log", Actor: "engine", At: at, Payload: []byte(` { "b" : 1, "a" : "<&>" } `)},
		{Type: "log", Actor: "engine"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if string(out[0].Payload) != `{"a":"<&>","b":1}` || string(out[1].Payload) != `{}` {
		t.Errorf("payloads %s %s", out[0].Payload, out[1].Payload)
	}
	if out[0].Seq != 8 || out[1].Seq != 9 || out[0].RunID != run {
		t.Errorf("seq/run not assigned: %+v", out)
	}
	if !out[0].At.Equal(at.Truncate(time.Microsecond)) || out[0].At.Location() != time.UTC || !out[1].At.IsZero() {
		t.Errorf("times %v %v", out[0].At, out[1].At)
	}
}

func TestCanonicalPayload_MatchesCore(t *testing.T) {
	for _, in := range []string{
		`{"b":1,"a":{"d":[1,2.50,"<x>"],"c":null}}`, ` [ 1e3 , -0 , "é" ] `, `"s"`, `12345678901234567890`, `{}`,
	} {
		got, err := eventstore.CanonicalPayload([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		want, err := core.CanonicalJSON(json.RawMessage(in))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("CanonicalPayload(%s) = %s, core.CanonicalJSON = %s", in, got, want)
		}
	}
	for _, bad := range []string{`{} {}`, `{"a":`, `nul`, `{"a":"\u0000"}`, `{"\u0000":1}`} {
		if _, err := eventstore.CanonicalPayload([]byte(bad)); !errors.Is(err, eventstore.ErrInvalid) {
			t.Errorf("CanonicalPayload(%s) error %v, want ErrInvalid", bad, err)
		}
	}
}
