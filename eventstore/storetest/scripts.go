package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/testkit"
)

// Suite is the conformance data directory: conformance/eventstore.
const Suite = "eventstore"

// Script is one scripted scenario from conformance/eventstore/*.json: a
// sequence of store operations, each with its expected outcome. Runs and
// leases are named symbolically; a run name that was never created stands
// for a run that does not exist.
type Script struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Spec        string `json:"spec"` // spec reference, informational
	Steps       []Step `json:"steps"`
}

// Step is one operation of a Script.
//
// Ops: create, get_run, append, read, acquire, renew, release, elapse,
// wake, list_ready, claim_ready, save_snapshot, load_snapshot.
type Step struct {
	Op     string `json:"op"`
	Run    string `json:"run,omitempty"`
	Worker string `json:"worker,omitempty"`
	TTLms  int64  `json:"ttl_ms,omitempty"` // acquire, renew, claim_ready (default 30000)
	// As names the lease an acquire or renew returns (claim_ready names
	// them "<as>:<run>").
	As string `json:"as,omitempty"`
	// Lease names the lease used by append, renew and release.
	Lease string `json:"lease,omitempty"`
	// Fence overrides the lease's fence for append (or supplies one when
	// Lease is empty).
	Fence       *eventstore.Fence `json:"fence,omitempty"`
	ExpectedSeq uint64            `json:"expected_seq,omitempty"`
	Events      []ScriptEvent     `json:"events,omitempty"`
	From        uint64            `json:"from,omitempty"`  // read
	Limit       int               `json:"limit,omitempty"` // read, list_ready, claim_ready
	Ms          int64             `json:"ms,omitempty"`    // elapse
	Status      string            `json:"status,omitempty"`
	// WakeInMs sets RunSpec.WakeAt (create) or Next.WakeAt (release)
	// relative to the store's now.
	WakeInMs int64 `json:"wake_in_ms,omitempty"`
	Priority int16 `json:"priority,omitempty"`
	// IdempotencyKey for create (default: derived from the run name).
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	AtSeq          uint64 `json:"at_seq,omitempty"` // save_snapshot
	State          string `json:"state,omitempty"`  // save_snapshot
	Expect         Expect `json:"expect"`
}

// ScriptEvent is an event in a Script.
type ScriptEvent struct {
	Seq     uint64          `json:"seq,omitempty"`
	Type    string          `json:"type"`
	Actor   string          `json:"actor"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// Expect is the expected outcome of a Step. Unset fields are not checked.
type Expect struct {
	// Error is "", "seq_conflict", "fenced", "lease_held", "not_found",
	// "exists" or "invalid".
	Error string  `json:"error,omitempty"`
	Head  *uint64 `json:"head,omitempty"` // append, get_run
	// FenceGreaterThan names a lease whose fence the result must exceed.
	FenceGreaterThan string        `json:"fence_greater_than,omitempty"`
	Events           []ScriptEvent `json:"events,omitempty"` // read
	Count            *int          `json:"count,omitempty"`  // read: number of events
	// Runs is the exact, ordered result of list_ready or claim_ready.
	Runs   []string `json:"runs,omitempty"`
	Status string   `json:"status,omitempty"` // get_run
	Owner  *string  `json:"owner,omitempty"`  // get_run ("" = no lease)
	Found  *bool    `json:"found,omitempty"`  // load_snapshot
	AtSeq  *uint64  `json:"at_seq,omitempty"` // load_snapshot
	State  *string  `json:"state,omitempty"`  // load_snapshot
}

var errNames = map[string]error{
	"seq_conflict": eventstore.ErrSeqConflict,
	"fenced":       eventstore.ErrFenced,
	"lease_held":   eventstore.ErrLeaseHeld,
	"not_found":    eventstore.ErrNotFound,
	"exists":       eventstore.ErrExists,
	"invalid":      eventstore.ErrInvalid,
}

// Scripts runs every scripted scenario in conformance/eventstore against a
// fresh harness each.
func Scripts(t *testing.T, nh NewHarness) {
	for _, f := range testkit.LoadCases[Script](t, Suite) {
		t.Run(f.Name, func(t *testing.T) {
			if len(f.Data.Steps) == 0 {
				t.Fatalf("%s: no steps", f.Name)
			}
			RunScript(t, nh, f.Data)
		})
	}
}

// RunScript executes one Script.
func RunScript(t *testing.T, nh NewHarness, sc Script) {
	e := newEnv(t, nh)
	runs := map[string]eventstore.RunID{}
	names := map[eventstore.RunID]string{}
	leases := map[string]eventstore.Lease{}
	runID := func(name string) eventstore.RunID {
		id, ok := runs[name]
		if !ok {
			id = e.ids.next()
			runs[name] = id
			names[id] = name
		}
		return id
	}
	dur := func(ms int64) time.Duration {
		if ms == 0 {
			return ttl
		}
		return time.Duration(ms) * time.Millisecond
	}
	for i, st := range sc.Steps {
		where := fmt.Sprintf("%s step %d (%s)", sc.Name, i+1, st.Op)
		var err error
		check := func() bool {
			t.Helper()
			want := errNames[st.Expect.Error]
			if st.Expect.Error != "" && want == nil {
				t.Fatalf("%s: unknown expected error %q", where, st.Expect.Error)
			}
			if want == nil && err != nil {
				t.Fatalf("%s: unexpected error %v", where, err)
			}
			if want != nil && !errors.Is(err, want) {
				t.Fatalf("%s: got error %v, want %s", where, err, st.Expect.Error)
			}
			return err == nil
		}
		checkFence := func(f eventstore.Fence) {
			t.Helper()
			if n := st.Expect.FenceGreaterThan; n != "" {
				prev, ok := leases[n]
				if !ok {
					t.Fatalf("%s: unknown lease %q", where, n)
				}
				if !prev.Fence.Less(f) {
					t.Fatalf("%s: fence %s not greater than %s's %s", where, f, n, prev.Fence)
				}
			}
		}
		checkRuns := func(ids []eventstore.RunID) {
			t.Helper()
			if st.Expect.Runs == nil {
				return
			}
			got := make([]string, len(ids))
			for j, id := range ids {
				got[j] = names[id]
			}
			if !slices.Equal(got, st.Expect.Runs) {
				t.Fatalf("%s: runs %v, want %v", where, got, st.Expect.Runs)
			}
		}
		switch st.Op {
		case "create":
			sp := e.spec()
			if _, ok := runs[st.Run]; ok {
				sp.ID = runs[st.Run] // re-create: expect exists
			} else {
				runs[st.Run], names[sp.ID] = sp.ID, st.Run
			}
			sp.Priority = st.Priority
			sp.IdempotencyKey = "idem-" + st.Run
			if st.IdempotencyKey != "" {
				sp.IdempotencyKey = st.IdempotencyKey
			}
			if st.WakeInMs != 0 {
				sp.WakeAt = e.now().Add(time.Duration(st.WakeInMs) * time.Millisecond)
			}
			err = e.s.CreateRun(e.ctx, sp, toEvents(st.Events))
			check()
		case "get_run":
			var info eventstore.RunInfo
			info, err = e.s.GetRun(e.ctx, runID(st.Run))
			if check() {
				if st.Expect.Head != nil && info.Head != *st.Expect.Head {
					t.Fatalf("%s: head %d, want %d", where, info.Head, *st.Expect.Head)
				}
				if st.Expect.Status != "" && string(info.Status) != st.Expect.Status {
					t.Fatalf("%s: status %s, want %s", where, info.Status, st.Expect.Status)
				}
				if st.Expect.Owner != nil && string(info.LeaseOwner) != *st.Expect.Owner {
					t.Fatalf("%s: lease owner %q, want %q", where, info.LeaseOwner, *st.Expect.Owner)
				}
			}
		case "append":
			var f eventstore.Fence
			if st.Lease != "" {
				l, ok := leases[st.Lease]
				if !ok {
					t.Fatalf("%s: unknown lease %q", where, st.Lease)
				}
				f = l.Fence
			}
			if st.Fence != nil {
				f = *st.Fence
			}
			var h uint64
			h, err = e.s.Append(e.ctx, runID(st.Run), st.ExpectedSeq, f, toEvents(st.Events))
			if check() && st.Expect.Head != nil && h != *st.Expect.Head {
				t.Fatalf("%s: head %d, want %d", where, h, *st.Expect.Head)
			}
		case "read":
			var evs []eventstore.Event
			evs, err = e.s.Read(e.ctx, runID(st.Run), st.From, st.Limit)
			if check() {
				if st.Expect.Count != nil && len(evs) != *st.Expect.Count {
					t.Fatalf("%s: %d events, want %d", where, len(evs), *st.Expect.Count)
				}
				if st.Expect.Events != nil {
					compareEvents(t, where, evs, st.Expect.Events)
				}
			}
		case "acquire":
			var l eventstore.Lease
			l, err = e.s.AcquireLease(e.ctx, runID(st.Run), eventstore.WorkerID(st.Worker), dur(st.TTLms))
			if check() {
				checkFence(l.Fence)
				if st.As != "" {
					leases[st.As] = l
				}
			}
		case "renew":
			var l eventstore.Lease
			l, err = e.s.RenewLease(e.ctx, leases[st.Lease], dur(st.TTLms))
			if check() {
				if l.Fence != leases[st.Lease].Fence {
					t.Fatalf("%s: renew changed the fence", where)
				}
				if st.As != "" {
					leases[st.As] = l
				}
			}
		case "release":
			next := eventstore.Next{Status: eventstore.RunStatus(st.Status)}
			if st.WakeInMs != 0 {
				next.WakeAt = e.now().Add(time.Duration(st.WakeInMs) * time.Millisecond)
			}
			err = e.s.ReleaseLease(e.ctx, leases[st.Lease], next)
			check()
		case "elapse":
			e.elapse(time.Duration(st.Ms) * time.Millisecond)
		case "wake":
			err = e.s.Wake(e.ctx, runID(st.Run))
			check()
		case "list_ready":
			var ids []eventstore.RunID
			limit := st.Limit
			if limit == 0 {
				limit = 100
			}
			ids, err = e.s.ListReady(e.ctx, eventstore.AllShards, limit)
			if check() {
				checkRuns(ids)
			}
		case "claim_ready":
			var ls []eventstore.Lease
			limit := st.Limit
			if limit == 0 {
				limit = 100
			}
			ls, err = e.s.ClaimReady(e.ctx, eventstore.AllShards, eventstore.WorkerID(st.Worker), dur(st.TTLms), limit)
			if check() {
				ids := make([]eventstore.RunID, len(ls))
				for j, l := range ls {
					ids[j] = l.Run
					leases[st.As+":"+names[l.Run]] = l
				}
				checkRuns(ids)
			}
		case "save_snapshot":
			err = e.s.SaveSnapshot(e.ctx, runID(st.Run), eventstore.Snapshot{
				AtSeq: st.AtSeq, ModuleSHA256: []byte{0xab}, State: []byte(st.State),
			})
			check()
		case "load_snapshot":
			var (
				s  eventstore.Snapshot
				ok bool
			)
			s, ok, err = e.s.LoadSnapshot(e.ctx, runID(st.Run))
			if check() {
				if st.Expect.Found != nil && ok != *st.Expect.Found {
					t.Fatalf("%s: found %v, want %v", where, ok, *st.Expect.Found)
				}
				if st.Expect.AtSeq != nil && s.AtSeq != *st.Expect.AtSeq {
					t.Fatalf("%s: at_seq %d, want %d", where, s.AtSeq, *st.Expect.AtSeq)
				}
				if st.Expect.State != nil && string(s.State) != *st.Expect.State {
					t.Fatalf("%s: state %q, want %q", where, s.State, *st.Expect.State)
				}
			}
		default:
			t.Fatalf("%s: unknown op", where)
		}
	}
}

func toEvents(in []ScriptEvent) []eventstore.Event {
	out := make([]eventstore.Event, len(in))
	for i, x := range in {
		out[i] = eventstore.Event{Seq: x.Seq, Type: x.Type, Actor: x.Actor, Payload: x.Payload}
	}
	return out
}

func compareEvents(t *testing.T, where string, got []eventstore.Event, want []ScriptEvent) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: read %d events %v, want %d", where, len(got), seqs(got), len(want))
	}
	for i, w := range want {
		g := got[i]
		if w.Seq != 0 && g.Seq != w.Seq {
			t.Fatalf("%s: event %d seq %d, want %d", where, i, g.Seq, w.Seq)
		}
		if g.Type != w.Type || g.Actor != w.Actor {
			t.Fatalf("%s: event %d is %s by %s, want %s by %s", where, i, g.Type, g.Actor, w.Type, w.Actor)
		}
		wp := w.Payload
		if len(wp) == 0 {
			wp = json.RawMessage("{}")
		}
		if !jsonEqual(g.Payload, wp) {
			t.Fatalf("%s: event %d payload %s, want %s", where, i, g.Payload, wp)
		}
	}
}
