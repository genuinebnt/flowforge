package graphstore

import (
	"context"
	"sync"
	"time"
)

// Notifier wakes WatchChanges loops when this process committed new
// change events. The zero value is ready to use.
type Notifier struct {
	mu sync.Mutex
	ch chan struct{}
}

// Wait returns a channel that is closed by the next Broadcast. Take it
// before reading, so a commit between the read and the wait is not lost.
func (n *Notifier) Wait() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch == nil {
		n.ch = make(chan struct{})
	}
	return n.ch
}

// Broadcast wakes every current waiter.
func (n *Notifier) Broadcast() {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.ch != nil {
		close(n.ch)
		n.ch = nil
	}
}

// WatchLoop implements WatchChanges on top of a page reader: it delivers
// every event after `after` to fn in seq order, then waits for a wake-up
// (or poll, when positive, for writers in other processes) and reads
// again, until ctx ends or fn fails. A cancelled context returns the
// classified context error.
func WatchLoop(ctx context.Context, after int64, read func(ctx context.Context, after int64) ([]ChangeEvent, error),
	n *Notifier, poll time.Duration, fn func(ChangeEvent) error) error {
	for {
		wake := n.Wait()
		evs, err := read(ctx, after)
		if err != nil {
			return err
		}
		for _, ev := range evs {
			if err := fn(ev); err != nil {
				return err
			}
			after = ev.Seq
		}
		if len(evs) > 0 {
			continue
		}
		var tick <-chan time.Time
		var timer *time.Timer
		if poll > 0 {
			timer = time.NewTimer(poll)
			tick = timer.C
		}
		select {
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return Classify(ctx.Err())
		case <-wake:
		case <-tick:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}
