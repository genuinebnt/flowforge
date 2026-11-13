package storetest

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/eventstore"
)

const ttl = 30 * time.Second

// Run runs the whole conformance suite against the implementation that nh
// builds (es_007). Each subtest gets a fresh harness.
func Run(t *testing.T, nh NewHarness) {
	t.Run("ES001_Append", func(t *testing.T) { ES001(t, nh) })
	t.Run("ES002_StaleSeq", func(t *testing.T) { ES002(t, nh) })
	t.Run("ES003_StaleFence", func(t *testing.T) { ES003(t, nh) })
	t.Run("ES004_LeaseExpiry", func(t *testing.T) { ES004(t, nh) })
	t.Run("ES005_Race", func(t *testing.T) { ES005(t, nh) })
	t.Run("ES006_ReadYourWritesSubscribe", func(t *testing.T) { ES006(t, nh) })
	t.Run("CreateRun", func(t *testing.T) { CreateRun(t, nh) })
	t.Run("Snapshots", func(t *testing.T) { Snapshots(t, nh) })
	t.Run("ReadyAndClaim", func(t *testing.T) { ReadyAndClaim(t, nh) })
	t.Run("RestartFences", func(t *testing.T) { RestartFences(t, nh) })
	t.Run("Scripts", func(t *testing.T) { Scripts(t, nh) })
	t.Run("PropRandomInterleavings", func(t *testing.T) { PropRandomInterleavings(t, nh) })
}

// ES001: append with the correct expected_seq and fence returns the new
// head; read returns the events in order.
func ES001(t *testing.T, nh NewHarness) {
	e := newEnv(t, nh)
	run := e.newRun()
	l := e.acquire(run, "w1", ttl)
	if l.Fence.IsZero() {
		t.Fatal("lease fence is zero")
	}
	e.append(run, 0, l.Fence, logEv(1), logEv(2), logEv(3))
	at := time.Date(2026, 11, 1, 12, 0, 0, 123456789, time.UTC)
	withTime := ev("effect_scheduled", "vm", map[string]any{"path": "14/3", "kind": "tool_call", "args": []any{1.5, "x", nil}})
	withTime.At = at
	e.append(run, 3, l.Fence, withTime, ev("log", "tool:subfinder", map[string]any{"message": "é ✓"}))

	got := e.read(run, 0, 0)
	if len(got) != 5 {
		t.Fatalf("read %d events, want 5", len(got))
	}
	e.checkLog(run, got)
	wantTypes := []string{"log", "log", "log", "effect_scheduled", "log"}
	for i, x := range got {
		if x.Type != wantTypes[i] {
			t.Errorf("event %d type %q, want %q", i+1, x.Type, wantTypes[i])
		}
	}
	if !jsonEqual(got[3].Payload, withTime.Payload) || got[3].Actor != "vm" {
		t.Errorf("event 4 = %s by %s, want %s by vm", got[3].Payload, got[3].Actor, withTime.Payload)
	}
	if !got[3].At.Equal(at.Truncate(time.Microsecond)) {
		t.Errorf("event 4 at %v, want %v (microsecond precision)", got[3].At, at.Truncate(time.Microsecond))
	}
	if !jsonEqual(got[4].Payload, []byte(`{"message":"é ✓"}`)) {
		t.Errorf("event 5 payload %s", got[4].Payload)
	}
	if r := e.read(run, 2, 2); len(r) != 2 || r[0].Seq != 2 || r[1].Seq != 3 {
		t.Errorf("read(from 2, limit 2) = %v", seqs(r))
	}
	if r := e.read(run, 6, 0); len(r) != 0 {
		t.Errorf("read past head returned %v", seqs(r))
	}
	if h := e.head(run); h != 5 {
		t.Errorf("head %d, want 5", h)
	}
	// Property (1): the next append must name expected_seq = h + n.
	if _, err := e.s.Append(e.ctx, run, 3, l.Fence, []eventstore.Event{logEv(9)}); !errors.Is(err, eventstore.ErrSeqConflict) {
		t.Errorf("append at old head: %v, want ErrSeqConflict", err)
	}
	// Seq and RunID may be given if they match.
	x := logEv(6)
	x.Seq, x.RunID = 6, run
	e.append(run, 5, l.Fence, x)
	x.Seq = 8
	_, err := e.s.Append(e.ctx, run, 6, l.Fence, []eventstore.Event{x})
	e.wantErr(err, eventstore.ErrInvalid, "append with wrong explicit seq")
	_, err = e.s.Append(e.ctx, e.ids.next(), 0, l.Fence, []eventstore.Event{logEv(1)})
	e.wantErr(err, eventstore.ErrNotFound, "append to unknown run")
	_, err = e.s.Read(e.ctx, e.ids.next(), 1, 0)
	e.wantErr(err, eventstore.ErrNotFound, "read unknown run")
}

func seqs(evs []eventstore.Event) []uint64 {
	out := make([]uint64, len(evs))
	for i, x := range evs {
		out[i] = x.Seq
	}
	return out
}

// ES002: an append with a stale expected_seq fails with SeqConflict and
// writes nothing.
func ES002(t *testing.T, nh NewHarness) {
	e := newEnv(t, nh)
	run := e.newRun()
	l := e.acquire(run, "w1", ttl)
	e.append(run, 0, l.Fence, logEv(1), logEv(2))
	for _, expected := range []uint64{0, 1, 3, 100} {
		_, err := e.s.Append(e.ctx, run, expected, l.Fence, []eventstore.Event{logEv(3), logEv(4)})
		e.wantErr(err, eventstore.ErrSeqConflict, "append with stale expected seq")
	}
	if got := e.read(run, 1, 0); len(got) != 2 {
		t.Fatalf("log has %d events after failed appends, want 2", len(got))
	}
	if h := e.head(run); h != 2 {
		t.Fatalf("head %d, want 2", h)
	}
	e.append(run, 2, l.Fence, logEv(3))
}

// ES003: an append with an older fence but the correct expected_seq fails
// with Fenced and writes nothing (property (2)).
func ES003(t *testing.T, nh NewHarness) {
	e := newEnv(t, nh)
	run := e.newRun()
	old := e.acquire(run, "w1", ttl)
	e.append(run, 0, old.Fence, logEv(1))
	// Re-acquiring yields a new fence; the old one is stale.
	cur := e.acquire(run, "w1", ttl)
	if !old.Fence.Less(cur.Fence) {
		t.Fatalf("re-acquired fence %s not greater than %s", cur.Fence, old.Fence)
	}
	_, err := e.s.Append(e.ctx, run, 1, old.Fence, []eventstore.Event{logEv(2)})
	e.wantErr(err, eventstore.ErrFenced, "append with older fence")
	// Fenced wins over a seq conflict: a stale worker must stop.
	_, err = e.s.Append(e.ctx, run, 0, old.Fence, []eventstore.Event{logEv(2)})
	e.wantErr(err, eventstore.ErrFenced, "append with older fence and stale seq")
	// A fence from the future is not current either.
	future := cur.Fence
	future.Term++
	_, err = e.s.Append(e.ctx, run, 1, future, []eventstore.Event{logEv(2)})
	e.wantErr(err, eventstore.ErrFenced, "append with a fence never issued")
	_, err = e.s.Append(e.ctx, run, 1, eventstore.Fence{}, []eventstore.Event{logEv(2)})
	e.wantErr(err, eventstore.ErrFenced, "append with the zero fence")
	if got := e.read(run, 1, 0); len(got) != 1 {
		t.Fatalf("log has %d events, want 1", len(got))
	}
	e.append(run, 1, cur.Fence, logEv(2))
	// After release the fence cannot append any more.
	if err := e.s.ReleaseLease(e.ctx, cur, eventstore.Next{}); err != nil {
		t.Fatal(err)
	}
	_, err = e.s.Append(e.ctx, run, 2, cur.Fence, []eventstore.Event{logEv(3)})
	e.wantErr(err, eventstore.ErrFenced, "append after release")
}

// ES004: lease acquire, expire, acquire by another worker: the fence
// strictly increases and the first holder's next append fails.
func ES004(t *testing.T, nh NewHarness) {
	e := newEnv(t, nh)
	run := e.newRun()
	l1 := e.acquire(run, "w1", 10*time.Second)
	e.append(run, 0, l1.Fence, logEv(1))
	_, err := e.s.AcquireLease(e.ctx, run, "w2", ttl)
	e.wantErr(err, eventstore.ErrLeaseHeld, "acquire of a held lease")
	e.elapse(5 * time.Second)
	_, err = e.s.AcquireLease(e.ctx, run, "w2", ttl)
	e.wantErr(err, eventstore.ErrLeaseHeld, "acquire before expiry")
	e.elapse(6 * time.Second)
	l2 := e.acquire(run, "w2", ttl)
	if !l1.Fence.Less(l2.Fence) {
		t.Fatalf("fence after takeover %s not greater than %s", l2.Fence, l1.Fence)
	}
	_, err = e.s.Append(e.ctx, run, 1, l1.Fence, []eventstore.Event{logEv(2)})
	e.wantErr(err, eventstore.ErrFenced, "append by the expired holder")
	_, err = e.s.RenewLease(e.ctx, l1, ttl)
	e.wantErr(err, eventstore.ErrFenced, "renew by the expired holder")
	e.wantErr(e.s.ReleaseLease(e.ctx, l1, eventstore.Next{}), eventstore.ErrFenced, "release by the expired holder")
	e.append(run, 1, l2.Fence, logEv(2))

	info, err := e.s.GetRun(e.ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if info.LeaseOwner != "w2" || info.Fence != l2.Fence {
		t.Fatalf("GetRun lease = %s/%s, want w2/%s", info.LeaseOwner, info.Fence, l2.Fence)
	}
	// Renewal keeps the lease alive past its original expiry.
	e.elapse(20 * time.Second)
	l2r, err := e.s.RenewLease(e.ctx, l2, ttl)
	if err != nil {
		t.Fatalf("renew: %v", err)
	}
	if l2r.Fence != l2.Fence {
		t.Fatalf("renew changed the fence: %s -> %s", l2.Fence, l2r.Fence)
	}
	e.elapse(20 * time.Second)
	_, err = e.s.AcquireLease(e.ctx, run, "w3", ttl)
	e.wantErr(err, eventstore.ErrLeaseHeld, "acquire of a renewed lease")
	// Release, then anyone may acquire, with a still larger fence.
	if err := e.s.ReleaseLease(e.ctx, l2r, eventstore.Next{Status: eventstore.StatusWaiting}); err != nil {
		t.Fatal(err)
	}
	l3 := e.acquire(run, "w3", ttl)
	if !l2.Fence.Less(l3.Fence) {
		t.Fatalf("fence after release %s not greater than %s", l3.Fence, l2.Fence)
	}
	_, err = e.s.AcquireLease(e.ctx, e.ids.next(), "w1", ttl)
	e.wantErr(err, eventstore.ErrNotFound, "acquire on unknown run")
	_, err = e.s.AcquireLease(e.ctx, run, "w3", 0)
	e.wantErr(err, eventstore.ErrInvalid, "acquire with zero ttl")
	_, err = e.s.AcquireLease(e.ctx, run, "", ttl)
	e.wantErr(err, eventstore.ErrInvalid, "acquire with empty worker")
}

// ES005: two writers race for the same expected_seq, 100 iterations on
// real goroutines: exactly one wins per iteration and the log has no gaps.
// A second phase races a stale fence against the current one: the current
// fence always wins.
func ES005(t *testing.T, nh NewHarness) {
	e := newEnv(t, nh)
	const iterations = 100
	run := e.newRun()
	l := e.acquire(run, "w1", time.Hour)
	race := func(head uint64, fences [2]eventstore.Fence) [2]error {
		var (
			wg   sync.WaitGroup
			errs [2]error
			gate = make(chan struct{})
		)
		for i := range 2 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-gate
				_, errs[i] = e.s.Append(e.ctx, run, head, fences[i],
					[]eventstore.Event{ev("log", "engine", map[string]any{"writer": i, "head": head})})
			}()
		}
		close(gate)
		wg.Wait()
		return errs
	}
	for i := range iterations {
		errs := race(uint64(i), [2]eventstore.Fence{l.Fence, l.Fence})
		wins := 0
		for _, err := range errs {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, eventstore.ErrSeqConflict):
			default:
				t.Fatalf("iteration %d: unexpected error %v", i, err)
			}
		}
		if wins != 1 {
			t.Fatalf("iteration %d: %d winners, want exactly 1 (%v)", i, wins, errs)
		}
	}
	got := e.read(run, 1, 0)
	if len(got) != iterations {
		t.Fatalf("log has %d events, want %d", len(got), iterations)
	}
	e.checkLog(run, got)

	stale := l
	cur := e.acquire(run, "w1", time.Hour)
	for i := range iterations / 2 {
		head := uint64(iterations + i)
		errs := race(head, [2]eventstore.Fence{stale.Fence, cur.Fence})
		if !errors.Is(errs[0], eventstore.ErrFenced) || errs[1] != nil {
			t.Fatalf("fenced race %d: stale=%v current=%v, want Fenced and success", i, errs[0], errs[1])
		}
	}
	got = e.read(run, 1, 0)
	e.checkLog(run, got)
	if len(got) != iterations+iterations/2 {
		t.Fatalf("log has %d events, want %d", len(got), iterations+iterations/2)
	}
}

// ES006: read-your-writes; Subscribe delivers every event once, in order,
// and resumes without gaps or duplicates from a given seq.
func ES006(t *testing.T, nh NewHarness) {
	e := newEnv(t, nh)
	run := e.newRun()
	l := e.acquire(run, "w1", time.Hour)
	head := uint64(0)
	for i := range 10 {
		head = e.append(run, head, l.Fence, logEv(i))
		if got := e.read(run, head, 0); len(got) != 1 || got[0].Seq != head {
			t.Fatalf("read-your-writes: read(%d) = %v", head, seqs(got))
		}
	}

	// Subscriber that stops after 25 events while a writer appends 40.
	ctx, cancel := context.WithTimeout(e.ctx, 30*time.Second)
	defer cancel()
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		h := head
		for i := range 15 {
			var err error
			h, err = e.s.Append(ctx, run, h, l.Fence, []eventstore.Event{logEv(100 + 2*i), logEv(101 + 2*i)})
			if err != nil {
				t.Errorf("writer: %v", err)
				return
			}
		}
	}()
	var seen []uint64
	for x, err := range e.s.Subscribe(ctx, run, 1) {
		if err != nil {
			t.Fatalf("subscribe: %v after %v", err, seen)
		}
		seen = append(seen, x.Seq)
		if len(seen) == 25 {
			break
		}
	}
	writer.Wait()
	if t.Failed() {
		return
	}
	wantSeqs(t, seen, 1, 25)

	// Reconnect from 26: the rest, then live events.
	head = 40
	seen = seen[:0]
	done := make(chan struct{})
	go func() {
		defer close(done)
		for x, err := range e.s.Subscribe(ctx, run, 26) {
			if err != nil {
				t.Errorf("resubscribe: %v after %v", err, seen)
				return
			}
			seen = append(seen, x.Seq)
			if x.Seq == 45 {
				return
			}
		}
	}()
	for i := range 5 {
		head = e.append(run, head, l.Fence, logEv(200+i))
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("subscription did not deliver the live events; saw %v", seen)
	}
	wantSeqs(t, seen, 26, 45)

	// Subscribing past the head waits; cancellation ends the stream with
	// the context error.
	cctx, ccancel := context.WithCancel(e.ctx)
	errc := make(chan error, 1)
	go func() {
		for x, err := range e.s.Subscribe(cctx, run, 46) {
			if err == nil {
				errc <- errors.New("unexpected event " + x.Type)
				return
			}
			errc <- err
			return
		}
		errc <- errors.New("stream ended without an error")
	}()
	ccancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled subscription yielded %v, want context.Canceled", err)
		}
	case <-ctx.Done():
		t.Fatal("cancelled subscription did not end")
	}

	for _, err := range e.s.Subscribe(ctx, e.ids.next(), 1) {
		e.wantErr(err, eventstore.ErrNotFound, "subscribe to unknown run")
		break
	}
}

func wantSeqs(t *testing.T, got []uint64, from, to uint64) {
	t.Helper()
	if uint64(len(got)) != to-from+1 {
		t.Fatalf("got seqs %v, want %d..%d", got, from, to)
	}
	for i, s := range got {
		if s != from+uint64(i) {
			t.Fatalf("got seqs %v, want %d..%d exactly once in order", got, from, to)
		}
	}
}

// CreateRun covers stream creation and its errors.
func CreateRun(t *testing.T, nh NewHarness) {
	e := newEnv(t, nh)
	sp := e.spec()
	initial := []eventstore.Event{ev("run_submitted", "engine", map[string]any{"idempotency_key": sp.IdempotencyKey})}
	if err := e.s.CreateRun(e.ctx, sp, initial); err != nil {
		t.Fatal(err)
	}
	info, err := e.s.GetRun(e.ctx, sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Head != 1 || info.Status != eventstore.StatusPending || info.LeaseOwner != "" || !info.Fence.IsZero() {
		t.Fatalf("new run info %+v", info)
	}
	if got := e.read(sp.ID, 1, 0); len(got) != 1 || got[0].Type != "run_submitted" {
		t.Fatalf("initial events %v", got)
	}
	e.wantErr(e.s.CreateRun(e.ctx, sp, nil), eventstore.ErrExists, "duplicate run id")
	dup := e.spec()
	dup.IdempotencyKey = sp.IdempotencyKey
	e.wantErr(e.s.CreateRun(e.ctx, dup, nil), eventstore.ErrExists, "duplicate idempotency key")
	other := dup
	other.Tenant[15] ^= 0xff // same key, other tenant: allowed
	if err := e.s.CreateRun(e.ctx, other, nil); err != nil {
		t.Fatalf("same key in another tenant: %v", err)
	}
	child := e.spec()
	child.Parent = sp.ID
	if err := e.s.CreateRun(e.ctx, child, nil); err != nil {
		t.Fatalf("child run: %v", err)
	}
	orphan := e.spec()
	orphan.Parent = e.ids.next()
	e.wantErr(e.s.CreateRun(e.ctx, orphan, nil), eventstore.ErrInvalid, "unknown parent run")

	bad := e.spec()
	bad.IdempotencyKey = ""
	e.wantErr(e.s.CreateRun(e.ctx, bad, nil), eventstore.ErrInvalid, "empty idempotency key")
	bad = e.spec()
	e.wantErr(e.s.CreateRun(e.ctx, bad, []eventstore.Event{{Type: "", Actor: "engine"}}), eventstore.ErrInvalid,
		"initial event without type")
	_, err = e.s.GetRun(e.ctx, bad.ID)
	e.wantErr(err, eventstore.ErrNotFound, "run whose creation failed")
	bad.Input = []byte(`{"x":`)
	e.wantErr(e.s.CreateRun(e.ctx, bad, nil), eventstore.ErrInvalid, "invalid input JSON")
	_, err = e.s.GetRun(e.ctx, e.ids.next())
	e.wantErr(err, eventstore.ErrNotFound, "GetRun on unknown run")

	l := e.acquire(sp.ID, "w1", ttl)
	for _, p := range []string{`{"a":`, `{"s":"\u0000"}`} {
		_, err := e.s.Append(e.ctx, sp.ID, 1, l.Fence, []eventstore.Event{{Type: "log", Actor: "engine", Payload: []byte(p)}})
		e.wantErr(err, eventstore.ErrInvalid, "payload "+p)
	}
	_, err = e.s.Append(e.ctx, sp.ID, 1, l.Fence, []eventstore.Event{{Type: "log", Actor: ""}})
	e.wantErr(err, eventstore.ErrInvalid, "empty actor")
	if h, err := e.s.Append(e.ctx, sp.ID, 1, l.Fence, nil); err != nil || h != 1 {
		t.Fatalf("empty append = %d, %v; want 1, nil", h, err)
	}
	h, err := e.s.Append(e.ctx, sp.ID, 1, l.Fence, []eventstore.Event{{Type: "log", Actor: "engine"}})
	if err != nil || h != 2 {
		t.Fatalf("append with nil payload = %d, %v", h, err)
	}
	if got := e.read(sp.ID, 2, 1); len(got) != 1 || !jsonEqual(got[0].Payload, []byte(`{}`)) {
		t.Fatalf("nil payload stored as %v", got)
	}
}

// Snapshots: newest wins, only SnapshotsKept are retained, AtSeq <= head,
// and DeleteSnapshots drops them all.
func Snapshots(t *testing.T, nh NewHarness) {
	e := newEnv(t, nh)
	run := e.newRun()
	if _, ok, err := e.s.LoadSnapshot(e.ctx, run); err != nil || ok {
		t.Fatalf("LoadSnapshot on fresh run = %v, %v", ok, err)
	}
	l := e.acquire(run, "w1", ttl)
	for i := range 10 {
		e.append(run, uint64(i), l.Fence, logEv(i))
	}
	mod := []byte{0xde, 0xad, 0xbe, 0xef}
	err := e.s.SaveSnapshot(e.ctx, run, eventstore.Snapshot{AtSeq: 11, ModuleSHA256: mod, State: []byte{1}})
	e.wantErr(err, eventstore.ErrInvalid, "snapshot beyond head")
	for _, at := range []uint64{2, 8, 4, 6, 10} {
		if err := e.s.SaveSnapshot(e.ctx, run, eventstore.Snapshot{AtSeq: at, ModuleSHA256: mod, State: []byte{byte(at)}}); err != nil {
			t.Fatalf("SaveSnapshot(%d): %v", at, err)
		}
	}
	// The same seq again keeps the first.
	if err := e.s.SaveSnapshot(e.ctx, run, eventstore.Snapshot{AtSeq: 10, ModuleSHA256: mod, State: []byte{99}}); err != nil {
		t.Fatal(err)
	}
	s, ok, err := e.s.LoadSnapshot(e.ctx, run)
	if err != nil || !ok {
		t.Fatalf("LoadSnapshot = %v, %v", ok, err)
	}
	if s.AtSeq != 10 || !slices.Equal(s.State, []byte{10}) || !slices.Equal(s.ModuleSHA256, mod) {
		t.Fatalf("LoadSnapshot = %+v, want at 10 with state [10]", s)
	}
	// Only the newest three remain: an older one saved now is the oldest
	// and is dropped immediately; the newest is unchanged.
	if err := e.s.SaveSnapshot(e.ctx, run, eventstore.Snapshot{AtSeq: 1, ModuleSHA256: mod, State: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if s, _, _ := e.s.LoadSnapshot(e.ctx, run); s.AtSeq != 10 {
		t.Fatalf("after saving an old snapshot the newest is %d", s.AtSeq)
	}
	_, _, err = e.s.LoadSnapshot(e.ctx, e.ids.next())
	e.wantErr(err, eventstore.ErrNotFound, "LoadSnapshot on unknown run")
	err = e.s.SaveSnapshot(e.ctx, e.ids.next(), eventstore.Snapshot{ModuleSHA256: mod})
	e.wantErr(err, eventstore.ErrNotFound, "SaveSnapshot on unknown run")
	// DeleteSnapshots (spec 10.7, G-ENG-04): every snapshot goes, the
	// events stay, a second call is a no-op and an unknown run is
	// ErrNotFound.
	if err := e.s.DeleteSnapshots(e.ctx, run); err != nil {
		t.Fatalf("DeleteSnapshots: %v", err)
	}
	if _, ok, err := e.s.LoadSnapshot(e.ctx, run); err != nil || ok {
		t.Fatalf("LoadSnapshot after DeleteSnapshots = %v, %v", ok, err)
	}
	if err := e.s.DeleteSnapshots(e.ctx, run); err != nil {
		t.Fatalf("second DeleteSnapshots: %v", err)
	}
	if evs, err := e.s.Read(e.ctx, run, 1, 0); err != nil || len(evs) == 0 {
		t.Fatalf("events after DeleteSnapshots: %d, %v", len(evs), err)
	}
	e.wantErr(e.s.DeleteSnapshots(e.ctx, e.ids.next()), eventstore.ErrNotFound, "DeleteSnapshots on unknown run")
}

// ReadyAndClaim covers ListReady ordering, sharding, concurrent claims and
// wakeups.
func ReadyAndClaim(t *testing.T, nh NewHarness) {
	e := newEnv(t, nh)
	const n = 40
	all := map[eventstore.RunID]bool{}
	for i := range n {
		all[e.newRun(func(s *eventstore.RunSpec) { s.Priority = int16(i % 3) })] = true
	}
	ready, err := e.s.ListReady(e.ctx, eventstore.AllShards, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != n {
		t.Fatalf("ListReady returned %d runs, want %d", len(ready), n)
	}
	prev := int16(1 << 14)
	for _, id := range ready {
		info, err := e.s.GetRun(e.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if info.Priority > prev {
			t.Fatalf("ListReady not ordered by priority descending")
		}
		prev = info.Priority
	}
	if got, _ := e.s.ListReady(e.ctx, eventstore.AllShards, 5); len(got) != 5 || !slices.Equal(got, ready[:5]) {
		t.Fatalf("ListReady(limit 5) = %v, want prefix %v", got, ready[:5])
	}
	_, err = e.s.ListReady(e.ctx, eventstore.Shard{Index: 4, Count: 4}, 10)
	e.wantErr(err, eventstore.ErrInvalid, "shard index out of range")

	// Shards partition the ready set.
	seen := map[eventstore.RunID]int{}
	for i := range uint32(4) {
		ids, err := e.s.ListReady(e.ctx, eventstore.Shard{Index: i, Count: 4}, 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range ids {
			if eventstore.ShardOf(id, 4) != i {
				t.Fatalf("run %s listed in shard %d, belongs to %d", id, i, eventstore.ShardOf(id, 4))
			}
			seen[id]++
		}
	}
	if len(seen) != n {
		t.Fatalf("shards cover %d runs, want %d", len(seen), n)
	}

	// Eight workers claim concurrently: every run is claimed exactly once.
	var (
		mu      sync.Mutex
		claimed = map[eventstore.RunID]eventstore.Lease{}
		wg      sync.WaitGroup
	)
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner := eventstore.WorkerID("claimer-" + string(rune('a'+w)))
			for {
				ls, err := e.s.ClaimReady(e.ctx, eventstore.AllShards, owner, ttl, 3)
				if err != nil {
					t.Errorf("ClaimReady: %v", err)
					return
				}
				if len(ls) == 0 {
					return
				}
				mu.Lock()
				for _, l := range ls {
					if prev, dup := claimed[l.Run]; dup {
						t.Errorf("run %s claimed twice (%s and %s)", l.Run, prev.Owner, l.Owner)
					}
					claimed[l.Run] = l
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(claimed) != n {
		t.Fatalf("%d runs claimed, want %d", len(claimed), n)
	}
	if got, _ := e.s.ListReady(e.ctx, eventstore.AllShards, 100); len(got) != 0 {
		t.Fatalf("leased runs still listed as ready: %d", len(got))
	}

	// Park every run: half wait for a timer an hour out, half finish.
	now := e.now()
	var waiting []eventstore.RunID
	i := 0
	for id, l := range claimed {
		next := eventstore.Next{Status: eventstore.StatusWaiting, WakeAt: now.Add(time.Hour)}
		if i%2 == 1 {
			next = eventstore.Next{Status: eventstore.StatusSucceeded}
		} else {
			waiting = append(waiting, id)
		}
		i++
		if err := e.s.ReleaseLease(e.ctx, l, next); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := e.s.ListReady(e.ctx, eventstore.AllShards, 100); len(got) != 0 {
		t.Fatalf("parked and finished runs listed as ready: %d", len(got))
	}
	// An out-of-band wake makes one waiting run due now.
	if err := e.s.Wake(e.ctx, waiting[0]); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.s.ListReady(e.ctx, eventstore.AllShards, 100); !slices.Equal(got, waiting[:1]) {
		t.Fatalf("after Wake ListReady = %v, want %v", got, waiting[:1])
	}
	// The timer comes due: every waiting run, never the finished ones.
	e.elapse(time.Hour + time.Second)
	got, _ := e.s.ListReady(e.ctx, eventstore.AllShards, 100)
	if len(got) != len(waiting) {
		t.Fatalf("after the wake time %d runs ready, want %d", len(got), len(waiting))
	}
	e.wantErr(e.s.Wake(e.ctx, e.ids.next()), eventstore.ErrNotFound, "Wake on unknown run")

	// Lost-wakeup protection: a wake during the lease survives a release
	// that asks for a later wake time.
	run := waiting[1]
	l := e.acquire(run, "w1", ttl)
	if err := e.s.Wake(e.ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := e.s.ReleaseLease(e.ctx, l, eventstore.Next{Status: eventstore.StatusWaiting, WakeAt: e.now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if ids, _ := e.s.ListReady(e.ctx, eventstore.AllShards, 100); !slices.Contains(ids, run) {
		t.Fatalf("wake during the lease was lost")
	}
	e.elapse(time.Millisecond)
	l = e.acquire(run, "w1", ttl)
	if err := e.s.ReleaseLease(e.ctx, l, eventstore.Next{Status: eventstore.StatusWaiting, WakeAt: e.now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if ids, _ := e.s.ListReady(e.ctx, eventstore.AllShards, 100); slices.Contains(ids, run) {
		t.Fatalf("a wake from before the lease woke the run again")
	}
	err = e.s.ReleaseLease(e.ctx, l, eventstore.Next{Status: "bogus"})
	e.wantErr(err, eventstore.ErrInvalid, "release with unknown status")
}

// RestartFences checks property (4) across a store restart (skipped when
// the harness cannot restart).
func RestartFences(t *testing.T, nh NewHarness) {
	e := newEnv(t, nh)
	if e.h.Restart == nil {
		t.Skip("harness has no Restart: the store keeps no durable state")
	}
	run := e.newRun()
	l1 := e.acquire(run, "w1", ttl)
	e.append(run, 0, l1.Fence, logEv(1))
	s2 := e.h.Restart(t)
	l2, err := s2.AcquireLease(e.ctx, run, "w1", ttl)
	if err != nil {
		t.Fatal(err)
	}
	if !l1.Fence.Less(l2.Fence) {
		t.Fatalf("fence after restart %s not greater than %s", l2.Fence, l1.Fence)
	}
	if _, err := s2.Append(e.ctx, run, 1, l1.Fence, []eventstore.Event{logEv(2)}); !errors.Is(err, eventstore.ErrFenced) {
		t.Fatalf("append with pre-restart fence: %v, want ErrFenced", err)
	}
	if got, err := s2.Read(e.ctx, run, 1, 0); err != nil || len(got) != 1 {
		t.Fatalf("read after restart: %d events, %v", len(got), err)
	}
}
