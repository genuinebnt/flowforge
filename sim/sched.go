package sim

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// simClock is the simulation's true clock: the store's clock (the
// "database clock" of spec 23.4) and the time base of every event. Only
// the scheduler moves it. It is safe for concurrent reads.
type simClock struct {
	start time.Time
	mono  atomic.Int64 // nanoseconds since start
}

func newSimClock(start time.Time) *simClock { return &simClock{start: start} }

// Now implements core.Clock.
func (c *simClock) Now() time.Time { return c.start.Add(time.Duration(c.mono.Load())) }

// Monotonic implements core.Clock.
func (c *simClock) Monotonic() time.Duration { return time.Duration(c.mono.Load()) }

// set moves the clock to t; it never goes backwards.
func (c *simClock) set(t time.Time) {
	if d := t.Sub(c.start); d > time.Duration(c.mono.Load()) {
		c.mono.Store(int64(d))
	}
}

// skewClock is a worker's view of time: the true wall time plus a fixed
// offset (spec 23.1 "wall clocks skew"). The monotonic reading is not
// skewed: monotonic origins are per process and the engine only ever
// subtracts two readings of its own clock.
type skewClock struct {
	base *simClock
	skew time.Duration
}

// Now implements core.Clock.
func (c skewClock) Now() time.Time { return c.base.Now().Add(c.skew) }

// Monotonic implements core.Clock.
func (c skewClock) Monotonic() time.Duration { return c.base.Monotonic() }

// event is a timed action of the scheduler.
type event struct {
	at       time.Time
	seq      uint64
	fn       func()
	canceled bool
	index    int
}

type eventQueue []*event

func (q eventQueue) Len() int { return len(q) }
func (q eventQueue) Less(i, j int) bool {
	if !q[i].at.Equal(q[j].at) {
		return q[i].at.Before(q[j].at)
	}
	return q[i].seq < q[j].seq
}
func (q eventQueue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index, q[j].index = i, j
}
func (q *eventQueue) Push(x any) {
	e := x.(*event)
	e.index = len(*q)
	*q = append(*q, e)
}
func (q *eventQueue) Pop() any {
	old := *q
	n := len(old)
	e := old[n-1]
	old[n-1] = nil
	*q = old[:n-1]
	return e
}

// errCrashed is returned to a worker by every call after its simulated
// process died.
var errCrashed = errors.New("sim: worker crashed")

// actor is one logical thread of the simulated cluster: a worker advancing
// a run (one Worker.RunOnce) or one API call. Actors run on goroutines,
// but only one at a time: an actor runs until it reaches a yield point (a
// store, scope, ingest or publish call through the fault adapter), parks
// there with the simulated time it is ready at, and the scheduler resumes
// the earliest ready actor or event. Goroutines the engine starts inside
// an actor (one per tool execution of a par_map round) touch only the tool
// executor, whose behaviour does not depend on their order.
type actor struct {
	id     uint64
	name   string
	w      *simWorker // nil for API and operator actors
	grant  chan grantMsg
	cancel context.CancelFunc

	crashed atomic.Bool
	mu      sync.Mutex
	busy    time.Duration // simulated time spent in tool executions since the last yield
	parked  *event
}

type grantMsg struct{ crash bool }

type actorMsg struct {
	a       *actor
	done    bool
	readyAt time.Time
}

type actorKey struct{}

func actorOf(ctx context.Context) *actor {
	a, _ := ctx.Value(actorKey{}).(*actor)
	return a
}

// addBusy records simulated time spent by a tool execution of the actor.
// Executions of one par_map round run concurrently, so the actor is busy
// for the longest of them.
func (a *actor) addBusy(d time.Duration) {
	a.mu.Lock()
	a.busy = max(a.busy, d)
	a.mu.Unlock()
}

func (a *actor) takeBusy() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.busy
	a.busy = 0
	return d
}

// scheduler is the cooperative, step-driven executor of the simulation.
// All nondeterminism comes from its seeded rng; a seed therefore
// reproduces a scenario exactly.
type scheduler struct {
	clock   *simClock
	q       eventQueue
	seq     uint64
	notify  chan actorMsg
	nextID  uint64
	steps   uint64
	running *actor
	onDone  map[*actor]func()
}

func newScheduler(clock *simClock) *scheduler {
	return &scheduler{clock: clock, notify: make(chan actorMsg), onDone: map[*actor]func(){}}
}

func (s *scheduler) now() time.Time { return s.clock.Now() }

// at schedules fn at time t (not before now).
func (s *scheduler) at(t time.Time, fn func()) *event {
	if t.Before(s.now()) {
		t = s.now()
	}
	s.seq++
	e := &event{at: t, seq: s.seq, fn: fn}
	heap.Push(&s.q, e)
	return e
}

// after schedules fn d from now.
func (s *scheduler) after(d time.Duration, fn func()) *event { return s.at(s.now().Add(max(d, 0)), fn) }

// next runs the earliest event; it reports false when none is left or the
// earliest is after limit.
func (s *scheduler) next(limit time.Time) bool {
	for s.q.Len() > 0 {
		e := s.q[0]
		if e.at.After(limit) {
			return false
		}
		heap.Pop(&s.q)
		if e.canceled {
			continue
		}
		s.clock.set(e.at)
		s.steps++
		e.fn()
		return true
	}
	return false
}

// spawn starts an actor running body and drives it to its first yield.
// done, if set, runs on the scheduler when body returns.
func (s *scheduler) spawn(base context.Context, name string, w *simWorker, body func(ctx context.Context), done func()) *actor {
	s.nextID++
	ctx, cancel := context.WithCancel(base)
	a := &actor{id: s.nextID, name: name, w: w, grant: make(chan grantMsg), cancel: cancel}
	ctx = context.WithValue(ctx, actorKey{}, a)
	if done != nil {
		s.onDone[a] = done
	}
	go func() {
		g := <-a.grant
		if g.crash {
			a.crashed.Store(true)
		} else {
			body(ctx)
		}
		cancel()
		s.notify <- actorMsg{a: a, done: true}
	}()
	s.resume(a, grantMsg{})
	return a
}

// resume lets a run until it yields again or finishes.
func (s *scheduler) resume(a *actor, g grantMsg) {
	a.parked = nil
	s.running = a
	a.grant <- g
	m := <-s.notify
	s.running = nil
	if m.done {
		if f := s.onDone[a]; f != nil {
			delete(s.onDone, a)
			f()
		}
		return
	}
	a.parked = s.at(m.readyAt, func() { s.resume(a, grantMsg{}) })
}

// kill crashes a parked actor: its pending call fails and so does every
// later call; the scheduler waits until its goroutine has returned, so a
// dead process never runs concurrently with a live one.
func (s *scheduler) kill(a *actor) {
	a.crashed.Store(true)
	a.cancel()
	if a.parked != nil {
		a.parked.canceled = true
		s.resume(a, grantMsg{crash: true})
	}
}

// yield is called on an actor's goroutine at every call through a fault
// adapter: the actor parks until the simulated time now+latency (plus the
// time its tool executions took) and returns when the scheduler resumes
// it. It reports errCrashed if the actor's process died meanwhile.
func (s *scheduler) yield(a *actor, latency time.Duration) error {
	if a.crashed.Load() {
		return errCrashed
	}
	s.notify <- actorMsg{a: a, readyAt: s.now().Add(latency + a.takeBusy())}
	g := <-a.grant
	if g.crash || a.crashed.Load() {
		a.crashed.Store(true)
		return errCrashed
	}
	return nil
}
