package storetest

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
	"github.com/genuinebnt/flowforge/testkit"
)

// Model timings for the random-interleavings property. Elapse steps are
// multiples of 3 s while the lease TTL (10 s) and the parked wake offset
// (4 s) are not, so model time never lands exactly on a deadline and the
// few milliseconds of real time a Postgres run takes cannot flip a result.
const (
	propTTL      = 10 * time.Second
	propWakeIn   = 4 * time.Second
	propStepUnit = 3 * time.Second
)

// propPayload is written into every event so the log can be audited.
type propPayload struct {
	W string `json:"w"` // writer
	T uint64 `json:"t"` // fence term
	F uint64 `json:"f"` // fence counter
	K int    `json:"k"` // index within the batch
}

type modelRun struct {
	id     eventstore.RunID
	events []propPayload
	cur    eventstore.Fence
	held   bool
	owner  eventstore.WorkerID
	exp    time.Duration // model time the lease expires
	wake   time.Duration // model time the run is due
	status eventstore.RunStatus
	woken  bool // Wake called while the current lease was held
}

func (r *modelRun) leased(now time.Duration) bool { return r.held && now <= r.exp }

func (r *modelRun) ready(now time.Duration) bool {
	return r.status.Schedulable() && r.wake <= now && !r.leased(now)
}

// PropRandomInterleavings is the eventstore property of spec 25.4: under
// random interleavings of append, lease and read from several simulated
// workers, per-run seq is contiguous and each fence value has one writer.
// Every operation's outcome is also checked against a reference model of
// the Store contract. A second phase runs real goroutines and checks the
// invariants only.
func PropRandomInterleavings(t *testing.T, nh NewHarness) {
	e := newEnv(t, nh)
	cases := e.h.PropCases
	if cases <= 0 {
		cases = testkit.Cases()
	}
	for i := range cases {
		propIteration(e, i)
		if t.Failed() {
			return
		}
	}
	t.Run("concurrent", func(t *testing.T) { propConcurrent(t, nh) })
}

func propIteration(e *env, iter int) {
	t := e.t
	rng := e.rng
	var (
		now     time.Duration
		runs    []*modelRun
		workers []eventstore.WorkerID
		leases  = map[eventstore.WorkerID]map[eventstore.RunID]eventstore.Lease{}
		history []string
	)
	fail := func(format string, args ...any) {
		t.Helper()
		t.Fatalf("iteration %d: %s\nhistory:\n  %s", iter, fmt.Sprintf(format, args...), strings.Join(history, "\n  "))
	}
	for range 1 + core.IntN(rng, 2) {
		runs = append(runs, &modelRun{id: e.newRun(), status: eventstore.StatusPending})
	}
	for w := range 2 + core.IntN(rng, 2) {
		id := eventstore.WorkerID(fmt.Sprintf("w%d", w))
		workers = append(workers, id)
		leases[id] = map[eventstore.RunID]eventstore.Lease{}
	}
	expectErr := func(op string, err error, want error) {
		t.Helper()
		if want == nil && err != nil {
			fail("%s: unexpected error %v", op, err)
		}
		if want != nil && !errors.Is(err, want) {
			fail("%s: got %v, want %v", op, err, want)
		}
	}
	acquired := func(r *modelRun, l eventstore.Lease, w eventstore.WorkerID) {
		if !r.cur.Less(l.Fence) {
			fail("acquire: fence %s not greater than previous %s (property 4)", l.Fence, r.cur)
		}
		r.cur, r.held, r.owner, r.exp, r.woken = l.Fence, true, w, now+propTTL, false
		leases[w][r.id] = l
	}
	readySet := func() []eventstore.RunID {
		var ids []eventstore.RunID
		for _, r := range runs {
			if r.ready(now) {
				ids = append(ids, r.id)
			}
		}
		return ids
	}
	ours := func(ids []eventstore.RunID) []eventstore.RunID {
		var out []eventstore.RunID
		for _, id := range ids {
			if slices.ContainsFunc(runs, func(r *modelRun) bool { return r.id == id }) {
				out = append(out, id)
			}
		}
		return out
	}
	sameSet := func(a, b []eventstore.RunID) bool {
		if len(a) != len(b) {
			return false
		}
		for _, x := range a {
			if !slices.Contains(b, x) {
				return false
			}
		}
		return true
	}

	steps := 20 + core.IntN(rng, 20)
	for range steps {
		r := runs[core.IntN(rng, len(runs))]
		w := workers[core.IntN(rng, len(workers))]
		l, hasLease := leases[w][r.id]
		p := core.IntN(rng, 100)
		switch {
		case p < 20: // acquire
			got, err := e.s.AcquireLease(e.ctx, r.id, w, propTTL)
			history = append(history, fmt.Sprintf("t=%v acquire %s r%d -> %s %v", now, w, slices.Index(runs, r), got.Fence, err))
			if r.leased(now) && r.owner != w {
				expectErr("acquire", err, eventstore.ErrLeaseHeld)
			} else {
				expectErr("acquire", err, nil)
				acquired(r, got, w)
			}
		case p < 50: // append
			// A worker only uses fences it was granted (zero when it never
			// leased this run): the fence is the write credential.
			fence := l.Fence
			expected := uint64(len(r.events))
			if core.IntN(rng, 4) == 0 {
				expected = uint64(core.IntN(rng, len(r.events)+3))
			}
			n := 1 + core.IntN(rng, 3)
			batch := make([]eventstore.Event, n)
			pays := make([]propPayload, n)
			for k := range batch {
				pays[k] = propPayload{W: string(w), T: fence.Term, F: fence.Counter, K: k}
				b, _ := json.Marshal(pays[k])
				batch[k] = eventstore.Event{Type: "log", Actor: "engine", Payload: b}
			}
			h, err := e.s.Append(e.ctx, r.id, expected, fence, batch)
			history = append(history, fmt.Sprintf("t=%v append %s r%d fence %s expected %d n %d -> %d %v",
				now, w, slices.Index(runs, r), fence, expected, n, h, err))
			switch {
			case !r.held || fence != r.cur:
				expectErr("append", err, eventstore.ErrFenced)
			case expected != uint64(len(r.events)):
				expectErr("append", err, eventstore.ErrSeqConflict)
			default:
				expectErr("append", err, nil)
				r.events = append(r.events, pays...)
				if h != uint64(len(r.events)) {
					fail("append returned head %d, want %d", h, len(r.events))
				}
			}
		case p < 60: // renew
			if !hasLease {
				continue
			}
			got, err := e.s.RenewLease(e.ctx, l, propTTL)
			history = append(history, fmt.Sprintf("t=%v renew %s r%d %s -> %v", now, w, slices.Index(runs, r), l.Fence, err))
			if r.held && l.Fence == r.cur {
				expectErr("renew", err, nil)
				if got.Fence != l.Fence {
					fail("renew changed the fence")
				}
				r.exp = now + propTTL
			} else {
				expectErr("renew", err, eventstore.ErrFenced)
			}
		case p < 70: // release
			if !hasLease {
				continue
			}
			var next eventstore.Next
			wakeAt := now
			switch core.IntN(rng, 10) {
			case 0:
				next.Status = eventstore.StatusSucceeded
			case 1, 2, 3, 4:
				next = eventstore.Next{Status: eventstore.StatusWaiting, WakeAt: e.now().Add(propWakeIn)}
				wakeAt = now + propWakeIn
			case 5, 6:
				next = eventstore.Next{Status: eventstore.StatusRunning}
			}
			err := e.s.ReleaseLease(e.ctx, l, next)
			history = append(history, fmt.Sprintf("t=%v release %s r%d %s %+v -> %v", now, w, slices.Index(runs, r), l.Fence, next, err))
			if r.held && l.Fence == r.cur {
				expectErr("release", err, nil)
				r.held, r.owner = false, ""
				if next.Status != "" {
					r.status = next.Status
				}
				r.wake = wakeAt
				if r.woken {
					r.wake = now
				}
			} else {
				expectErr("release", err, eventstore.ErrFenced)
			}
		case p < 80: // read
			from := uint64(core.IntN(rng, len(r.events)+2))
			limit := core.IntN(rng, 4)
			got, err := e.s.Read(e.ctx, r.id, from, limit)
			history = append(history, fmt.Sprintf("t=%v read r%d from %d limit %d -> %d events %v",
				now, slices.Index(runs, r), from, limit, len(got), err))
			expectErr("read", err, nil)
			start := max(from, 1)
			var want []propPayload
			if start <= uint64(len(r.events)) {
				want = r.events[start-1:]
			}
			if limit > 0 && len(want) > limit {
				want = want[:limit]
			}
			if len(got) != len(want) {
				fail("read returned %d events, want %d", len(got), len(want))
			}
			for k, x := range got {
				var pp propPayload
				if err := json.Unmarshal(x.Payload, &pp); err != nil || pp != want[k] || x.Seq != start+uint64(k) {
					fail("read event %d = seq %d %s, want seq %d %+v", k, x.Seq, x.Payload, start+uint64(k), want[k])
				}
			}
		case p < 88: // elapse
			d := propStepUnit
			if core.IntN(rng, 3) == 0 {
				d = 4 * propStepUnit
			}
			e.elapse(d)
			now += d
			history = append(history, fmt.Sprintf("elapse %v -> t=%v", d, now))
		case p < 92: // wake
			err := e.s.Wake(e.ctx, r.id)
			history = append(history, fmt.Sprintf("t=%v wake r%d -> %v", now, slices.Index(runs, r), err))
			expectErr("wake", err, nil)
			r.wake = now
			if r.held {
				r.woken = true
			}
		case p < 96: // list_ready
			got, err := e.s.ListReady(e.ctx, eventstore.AllShards, 10_000)
			expectErr("list_ready", err, nil)
			mine := ours(got)
			history = append(history, fmt.Sprintf("t=%v list_ready -> %d of ours", now, len(mine)))
			if want := readySet(); !sameSet(mine, want) {
				fail("list_ready = %v, want %v", mine, want)
			}
		default: // claim_ready
			want := readySet()
			got, err := e.s.ClaimReady(e.ctx, eventstore.AllShards, w, propTTL, 100)
			history = append(history, fmt.Sprintf("t=%v claim_ready %s -> %d %v", now, w, len(got), err))
			expectErr("claim_ready", err, nil)
			ids := make([]eventstore.RunID, len(got))
			for k, l := range got {
				ids[k] = l.Run
			}
			if !sameSet(ids, want) {
				fail("claim_ready = %v, want %v", ids, want)
			}
			for _, l := range got {
				r := runs[slices.IndexFunc(runs, func(r *modelRun) bool { return r.id == l.Run })]
				acquired(r, l, w)
			}
		}
	}

	// Audit the logs: contiguous seqs, one writer per fence, fences never
	// go backwards along a log.
	for _, r := range runs {
		evs := e.read(r.id, 1, 0)
		e.checkLog(r.id, evs)
		if len(evs) != len(r.events) {
			fail("run has %d events, model %d", len(evs), len(r.events))
		}
		auditFences(e.t, r.id, evs)
	}
	// Finish the runs so later iterations see only their own ready runs.
	e.elapse(time.Minute)
	for _, r := range runs {
		l := e.acquire(r.id, "cleanup", time.Second)
		if err := e.s.ReleaseLease(e.ctx, l, eventstore.Next{Status: eventstore.StatusSucceeded}); err != nil {
			fail("cleanup release: %v", err)
		}
	}
}

// auditFences checks that every fence value has exactly one writer and
// that fences are non-decreasing in seq order.
func auditFences(t testing.TB, run eventstore.RunID, evs []eventstore.Event) {
	t.Helper()
	writer := map[eventstore.Fence]string{}
	var last eventstore.Fence
	for _, x := range evs {
		var pp propPayload
		if err := json.Unmarshal(x.Payload, &pp); err != nil {
			t.Fatalf("run %s seq %d: payload %s: %v", run, x.Seq, x.Payload, err)
		}
		f := eventstore.Fence{Term: pp.T, Counter: pp.F}
		if w, ok := writer[f]; ok && w != pp.W {
			t.Fatalf("run %s: fence %s has two writers, %s and %s", run, f, w, pp.W)
		}
		writer[f] = pp.W
		if f.Less(last) {
			t.Fatalf("run %s seq %d: fence %s after %s", run, x.Seq, f, last)
		}
		last = f
	}
}

// propConcurrent runs workers on real goroutines and checks the
// invariants: contiguous seqs, one writer per fence, fences monotonic, and
// every acknowledged append present at the seqs it was acknowledged for.
func propConcurrent(t *testing.T, nh NewHarness) {
	e := newEnv(t, nh)
	const (
		nRuns    = 2
		nWorkers = 4
		nOps     = 150
	)
	runs := make([]eventstore.RunID, nRuns)
	for i := range runs {
		runs[i] = e.newRun()
	}
	type ack struct {
		run   eventstore.RunID
		w     string
		fence eventstore.Fence
		head  uint64
		n     int
	}
	var (
		mu   sync.Mutex
		acks []ack
		wg   sync.WaitGroup
	)
	for w := range nWorkers {
		rng := core.NewSplitMix64(e.rng.Uint64())
		wid := eventstore.WorkerID(fmt.Sprintf("cw%d", w))
		wg.Add(1)
		go func() {
			defer wg.Done()
			leases := map[eventstore.RunID]eventstore.Lease{}
			heads := map[eventstore.RunID]uint64{}
			for range nOps {
				run := runs[core.IntN(rng, nRuns)]
				switch p := core.IntN(rng, 100); {
				case p < 25:
					l, err := e.s.AcquireLease(e.ctx, run, wid, propTTL)
					if err == nil {
						leases[run] = l
					} else if !errors.Is(err, eventstore.ErrLeaseHeld) {
						t.Errorf("acquire: %v", err)
						return
					}
				case p < 75:
					l := leases[run]
					n := 1 + core.IntN(rng, 3)
					batch := make([]eventstore.Event, n)
					for k := range batch {
						b, _ := json.Marshal(propPayload{W: string(wid), T: l.Fence.Term, F: l.Fence.Counter, K: k})
						batch[k] = eventstore.Event{Type: "log", Actor: "engine", Payload: b}
					}
					h, err := e.s.Append(e.ctx, run, heads[run], l.Fence, batch)
					switch {
					case err == nil:
						heads[run] = h
						mu.Lock()
						acks = append(acks, ack{run, string(wid), l.Fence, h, n})
						mu.Unlock()
					case errors.Is(err, eventstore.ErrSeqConflict):
						info, err := e.s.GetRun(e.ctx, run)
						if err != nil {
							t.Errorf("get run: %v", err)
							return
						}
						heads[run] = info.Head
					case errors.Is(err, eventstore.ErrFenced):
						delete(leases, run) // a fenced worker stops
					default:
						t.Errorf("append: %v", err)
						return
					}
				case p < 85:
					if l, ok := leases[run]; ok {
						if err := e.s.ReleaseLease(e.ctx, l, eventstore.Next{}); err != nil && !errors.Is(err, eventstore.ErrFenced) {
							t.Errorf("release: %v", err)
							return
						}
						delete(leases, run)
					}
				case p < 90:
					if err := e.h.Elapse(e.ctx, propTTL+time.Second); err != nil {
						t.Errorf("elapse: %v", err)
						return
					}
				default:
					if _, err := e.s.Read(e.ctx, run, uint64(core.IntN(rng, 20)), 5); err != nil {
						t.Errorf("read: %v", err)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	total := map[eventstore.RunID]int{}
	logs := map[eventstore.RunID][]eventstore.Event{}
	for _, run := range runs {
		evs := e.read(run, 1, 0)
		e.checkLog(run, evs)
		auditFences(t, run, evs)
		logs[run] = evs
	}
	for _, a := range acks {
		total[a.run] += a.n
		evs := logs[a.run]
		for k := range a.n {
			seq := a.head - uint64(a.n) + uint64(k) + 1
			if seq > uint64(len(evs)) {
				t.Fatalf("acknowledged seq %d of run %s missing", seq, a.run)
			}
			var pp propPayload
			_ = json.Unmarshal(evs[seq-1].Payload, &pp)
			if pp.W != a.w || pp.F != a.fence.Counter || pp.T != a.fence.Term || pp.K != k {
				t.Fatalf("seq %d of run %s holds %+v, acknowledged to %s fence %s", seq, a.run, pp, a.w, a.fence)
			}
		}
	}
	for _, run := range runs {
		if total[run] != len(logs[run]) {
			t.Fatalf("run %s has %d events, %d acknowledged", run, len(logs[run]), total[run])
		}
	}
}
