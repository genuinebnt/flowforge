package eventstore

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// The helpers in this file are shared by every Store implementation so
// that validation and normalization are identical everywhere (es_007: no
// per-implementation special cases).

// MaxSeq is the largest seq or fence counter a store accepts (Postgres
// stores them as bigint).
const MaxSeq = math.MaxInt64

// PrepareEvents validates events to be appended to run at head and returns
// normalized copies: RunID and Seq assigned (head+1, head+2, ...), At in
// UTC truncated to microseconds (zero stays zero for the store to fill),
// Payload in canonical JSON ({} for nil).
//
// PORT: the microsecond truncation matches Postgres timestamptz, so both
// stores return the same At; truncate toward the past (time.Truncate), not
// to the nearest microsecond.
func PrepareEvents(run RunID, head uint64, events []Event) ([]Event, error) {
	if head > MaxSeq-uint64(len(events)) {
		return nil, fmt.Errorf("%w: seq overflow", ErrInvalid)
	}
	out := make([]Event, len(events))
	for i, ev := range events {
		seq := head + uint64(i) + 1
		if !ev.RunID.IsZero() && ev.RunID != run {
			return nil, fmt.Errorf("%w: event %d has run %s, appending to %s", ErrInvalid, i, ev.RunID, run)
		}
		if ev.Seq != 0 && ev.Seq != seq {
			return nil, fmt.Errorf("%w: event %d has seq %d, next seq is %d", ErrInvalid, i, ev.Seq, seq)
		}
		if err := checkText("event type", ev.Type); err != nil {
			return nil, err
		}
		if err := checkText("event actor", ev.Actor); err != nil {
			return nil, err
		}
		payload, err := CanonicalPayload(ev.Payload)
		if err != nil {
			return nil, fmt.Errorf("event %d: %w", i, err)
		}
		at := ev.At
		if !at.IsZero() {
			at = at.UTC().Truncate(time.Microsecond)
		}
		out[i] = Event{RunID: run, Seq: seq, Type: ev.Type, At: at, Actor: ev.Actor, Payload: payload}
	}
	return out, nil
}

// CanonicalPayload validates a JSON payload and returns its canonical
// encoding, byte-identical to core.CanonicalJSON: keys sorted, no
// insignificant whitespace, no HTML escaping, numbers as written. nil or
// empty means {}. Strings and keys must not contain U+0000, which Postgres
// jsonb cannot store.
func CanonicalPayload(p json.RawMessage) (json.RawMessage, error) {
	if len(p) == 0 {
		return json.RawMessage("{}"), nil
	}
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: payload is not valid JSON: %v", ErrInvalid, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing data after the JSON payload", ErrInvalid)
	}
	if hasNUL(v) {
		return nil, fmt.Errorf("%w: payload contains U+0000", ErrInvalid)
	}
	var buf bytes.Buffer
	buf.Grow(len(p))
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, fmt.Errorf("%w: payload: %v", ErrInvalid, err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func hasNUL(v any) bool {
	switch x := v.(type) {
	case string:
		return strings.IndexByte(x, 0) >= 0
	case []any:
		for _, e := range x {
			if hasNUL(e) {
				return true
			}
		}
	case map[string]any:
		for k, e := range x {
			if strings.IndexByte(k, 0) >= 0 || hasNUL(e) {
				return true
			}
		}
	}
	return false
}

func checkText(what, s string) error {
	if s == "" {
		return fmt.Errorf("%w: empty %s", ErrInvalid, what)
	}
	if !utf8.ValidString(s) || strings.IndexByte(s, 0) >= 0 {
		return fmt.Errorf("%w: %s is not valid text", ErrInvalid, what)
	}
	return nil
}

// CheckWorker validates a worker id.
func CheckWorker(w WorkerID) error { return checkText("worker id", string(w)) }

// CheckTTL validates a lease TTL: positive, at least a microsecond (the
// database clock resolution).
func CheckTTL(ttl time.Duration) error {
	if ttl < time.Microsecond {
		return fmt.Errorf("%w: lease ttl %v must be at least 1µs", ErrInvalid, ttl)
	}
	return nil
}

// CheckSeq validates a seq or fence counter argument.
func CheckSeq(what string, v uint64) error {
	if v > MaxSeq {
		return fmt.Errorf("%w: %s %d out of range", ErrInvalid, what, v)
	}
	return nil
}

// PrepareRunSpec validates a RunSpec and returns a normalized copy
// (Input canonical, {} for nil; RequestHash non-nil; times in UTC
// truncated to microseconds).
func PrepareRunSpec(spec RunSpec) (RunSpec, error) {
	if spec.ID.IsZero() {
		return spec, fmt.Errorf("%w: run id is zero", ErrInvalid)
	}
	if spec.Tenant.IsZero() {
		return spec, fmt.Errorf("%w: tenant is zero", ErrInvalid)
	}
	if spec.WorkflowVersion.IsZero() {
		return spec, fmt.Errorf("%w: workflow version is zero", ErrInvalid)
	}
	if spec.Parent == spec.ID {
		return spec, fmt.Errorf("%w: run is its own parent", ErrInvalid)
	}
	if err := checkText("created_by", spec.CreatedBy); err != nil {
		return spec, err
	}
	if err := checkText("idempotency key", spec.IdempotencyKey); err != nil {
		return spec, err
	}
	in, err := CanonicalPayload(spec.Input)
	if err != nil {
		return spec, fmt.Errorf("run input: %w", err)
	}
	spec.Input = in
	if spec.RequestHash == nil {
		spec.RequestHash = []byte{}
	}
	if !spec.WakeAt.IsZero() {
		spec.WakeAt = spec.WakeAt.UTC().Truncate(time.Microsecond)
	}
	if !spec.Deadline.IsZero() {
		spec.Deadline = spec.Deadline.UTC().Truncate(time.Microsecond)
	}
	return spec, nil
}

// CheckNext validates the arguments of ReleaseLease.
func CheckNext(n Next) error {
	if n.Status != "" && !n.Status.Valid() {
		return fmt.Errorf("%w: unknown run status %q", ErrInvalid, n.Status)
	}
	return nil
}

// CheckShard validates a shard argument.
func CheckShard(s Shard) error {
	if !s.Valid() {
		return fmt.Errorf("%w: shard %d of %d", ErrInvalid, s.Index, s.Count)
	}
	return nil
}
