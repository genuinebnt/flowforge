package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
)

// sseEvent is one Server-Sent Event.
type sseEvent struct {
	id    int64
	event string
	data  any
}

// resumeCursor reads Last-Event-ID (reconnects) or after_seq.
func (c *call) resumeCursor() (int64, error) {
	if v := c.r.Header.Get("Last-Event-ID"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return 0, errValidation("Last-Event-ID: must be a non-negative event sequence number")
		}
		return n, nil
	}
	return c.queryInt("after_seq", 0, 0, 1<<62)
}

// stream bridges a gRPC server stream to SSE (spec 12.6): events in order
// with id = seq, a ": keep-alive" comment every KeepAlive, and a final
// "error" event if the upstream stream fails. recv returns io.EOF when the
// upstream ends.
func (s *Server) stream(c *call, recv func() (sseEvent, error)) error {
	rc := http.NewResponseController(c.w)
	h := c.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	c.wrote = true
	c.w.WriteHeader(http.StatusOK)
	write := func(b []byte) error {
		_ = rc.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout)) // socket deadline: wall clock, not the injected clock
		if _, err := c.w.Write(b); err != nil {
			return err
		}
		return rc.Flush()
	}
	if err := write([]byte(": stream open\n\n")); err != nil {
		return nil
	}

	type item struct {
		ev  sseEvent
		err error
	}
	ch := make(chan item)
	go func() {
		for {
			ev, err := recv()
			select {
			case ch <- item{ev, err}:
			case <-c.ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	tick := time.NewTicker(s.cfg.KeepAlive)
	defer tick.Stop()
	for {
		select {
		case <-c.ctx.Done():
			return nil
		case <-tick.C:
			if write([]byte(": keep-alive\n\n")) != nil {
				return nil
			}
		case it := <-ch:
			if errors.Is(it.err, io.EOF) {
				return nil
			}
			if it.err != nil {
				if c.ctx.Err() != nil {
					return nil
				}
				e := toAPIError(it.err)
				_ = write(sseFrame(sseEvent{event: "error", data: e.body(c.reqID)}))
				return nil
			}
			if write(sseFrame(it.ev)) != nil {
				return nil
			}
		}
	}
}

func sseFrame(ev sseEvent) []byte {
	var b bytes.Buffer
	if ev.event != "error" {
		b.WriteString("id: " + strconv.FormatInt(ev.id, 10) + "\n")
	}
	b.WriteString("event: " + ev.event + "\n")
	data, err := json.Marshal(ev.data)
	if err != nil {
		data = []byte(`{}`)
	}
	b.WriteString("data: ")
	b.Write(data) // JSON has no raw newlines
	b.WriteString("\n\n")
	return b.Bytes()
}

func (s *Server) watchRun(c *call) error {
	id, err := c.pathID("id", core.PrefixRun)
	if err != nil {
		return err
	}
	after, err := c.resumeCursor()
	if err != nil {
		return err
	}
	// Resolve the run first so a missing (or other tenant's) run is a plain
	// 404, not an event stream.
	if _, err := s.cfg.Backends.Engine.GetRun(c.ctx, &pb.GetRunRequest{RunId: id}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.ctx, StreamMaxDuration)
	defer cancel()
	c.ctx = ctx
	st, err := s.cfg.Backends.Engine.WatchRun(ctx, &pb.WatchRunRequest{RunId: id, AfterSeq: after})
	if err != nil {
		return err
	}
	return s.stream(c, func() (sseEvent, error) {
		ev, err := st.Recv()
		if err != nil {
			return sseEvent{}, err
		}
		return sseEvent{id: ev.GetSeq(), event: "run_event", data: runEventJSON(ev)}, nil
	})
}

func (s *Server) watchChanges(c *call) error {
	pid, err := c.programParam(true)
	if err != nil {
		return err
	}
	after, err := c.resumeCursor()
	if err != nil {
		return err
	}
	if _, err := s.cfg.Backends.Scope.GetProgram(c.ctx, &pb.GetProgramRequest{ProgramId: pid}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(c.ctx, StreamMaxDuration)
	defer cancel()
	c.ctx = ctx
	st, err := s.cfg.Backends.Graph.WatchChanges(ctx, &pb.WatchChangesRequest{ProgramId: pid, AfterSeq: after, Types: c.queryList("types")})
	if err != nil {
		return err
	}
	return s.stream(c, func() (sseEvent, error) {
		ev, err := st.Recv()
		if err != nil {
			return sseEvent{}, err
		}
		return sseEvent{id: ev.GetSeq(), event: "graph_change", data: changeJSON(ev)}, nil
	})
}
