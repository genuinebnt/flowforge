package pgstore

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/eventstore"
)

// listener holds one LISTEN connection per Store and fans notifications
// out to the subscriptions of the named run. It reconnects after errors;
// while disconnected, subscriptions fall back to polling.
type listener struct {
	cfg     *pgx.ConnConfig
	channel string
	backoff time.Duration
	done    chan struct{}

	mu   sync.Mutex
	subs map[eventstore.RunID]map[chan struct{}]struct{}
}

func newListener(ctx context.Context, cfg *pgx.ConnConfig, channel string, backoff time.Duration) *listener {
	l := &listener{cfg: cfg, channel: channel, backoff: backoff, done: make(chan struct{}),
		subs: map[eventstore.RunID]map[chan struct{}]struct{}{}}
	go l.loop(ctx)
	return l
}

func (l *listener) wait() { <-l.done }

// register returns a channel that receives a token when the run may have
// new events, and a function that unregisters it.
func (l *listener) register(run eventstore.RunID) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	l.mu.Lock()
	m := l.subs[run]
	if m == nil {
		m = map[chan struct{}]struct{}{}
		l.subs[run] = m
	}
	m[ch] = struct{}{}
	l.mu.Unlock()
	return ch, func() {
		l.mu.Lock()
		delete(m, ch)
		if len(m) == 0 {
			delete(l.subs, run)
		}
		l.mu.Unlock()
	}
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (l *listener) notify(run eventstore.RunID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for ch := range l.subs[run] {
		signal(ch)
	}
}

func (l *listener) notifyAll() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, m := range l.subs {
		for ch := range m {
			signal(ch)
		}
	}
}

func (l *listener) loop(ctx context.Context) {
	defer close(l.done)
	for ctx.Err() == nil {
		l.session(ctx)
		select {
		case <-ctx.Done():
		case <-time.After(l.backoff):
		}
	}
}

// session runs one LISTEN connection until it fails or ctx ends.
func (l *listener) session(ctx context.Context) {
	conn, err := pgx.ConnectConfig(ctx, l.cfg)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "LISTEN "+pgx.Identifier{l.channel}.Sanitize()); err != nil {
		return
	}
	// Anything appended while we were not listening is caught by this
	// round of reads.
	l.notifyAll()
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return
		}
		run, err := core.ParseUUID(n.Payload)
		if err != nil {
			continue
		}
		l.notify(run)
	}
}
