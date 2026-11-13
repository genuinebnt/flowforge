package alerts

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/core"
)

// DedupeKey computes sha256(rule_id || group_by values) (spec 15.3) as
// lower-case hex. The values are encoded unambiguously as
// rule_id, a NUL byte, then the canonical JSON array of the values (times
// as RFC 3339 strings, durations as Go duration strings, absent values as
// null). A rule without group_by therefore has one key.
//
// PORT: the bytes are a cross-language contract (docs/PORTING_RUST.md 4.4).
// They are core.CanonicalJSON's: no HTML escaping, U+2028/U+2029 escaped,
// invalid UTF-8 replaced by U+FFFD, int64 in decimal, floats in Go's
// encoding/json form.
func DedupeKey(ruleID string, values []any) string {
	buf := make([]byte, 0, len(ruleID)+64)
	buf = append(buf, ruleID...)
	buf = append(buf, 0)
	if enc, ok := appendScalarsJSON(buf, values); ok {
		buf = enc
	} else {
		if values == nil {
			values = []any{}
		}
		enc, err := core.CanonicalJSON(values)
		if err != nil {
			// Values come from value.export and are always encodable; fall
			// back to a deterministic textual form rather than fail.
			enc = fmt.Appendf(nil, "%#v", values)
		}
		buf = append(buf[:len(ruleID)+1], enc...)
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:])
}

// appendScalarsJSON is a fast path producing exactly the bytes
// core.CanonicalJSON gives for an array of strings, int64s, bools and
// nils. It reports false for anything else (floats, maps), which then
// take the general path.
func appendScalarsJSON(buf []byte, values []any) ([]byte, bool) {
	buf = append(buf, '[')
	for i, v := range values {
		if i > 0 {
			buf = append(buf, ',')
		}
		switch x := v.(type) {
		case nil:
			buf = append(buf, "null"...)
		case bool:
			buf = strconv.AppendBool(buf, x)
		case int64:
			buf = strconv.AppendInt(buf, x, 10)
		case string:
			buf = appendJSONString(buf, x)
		default:
			return buf, false
		}
	}
	return append(buf, ']'), true
}

// appendJSONString quotes s like encoding/json without HTML escaping.
func appendJSONString(buf []byte, s string) []byte {
	const hexDigits = "0123456789abcdef"
	buf = append(buf, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '"' || c == '\\':
				buf = append(buf, '\\', c)
			case c == '\n':
				buf = append(buf, '\\', 'n')
			case c == '\r':
				buf = append(buf, '\\', 'r')
			case c == '\t':
				buf = append(buf, '\\', 't')
			case c == '\b':
				buf = append(buf, '\\', 'b')
			case c == '\f':
				buf = append(buf, '\\', 'f')
			case c < 0x20:
				buf = append(buf, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			default:
				buf = append(buf, c)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			buf = append(buf, "\uFFFD"...) // what the canonical re-encoding yields
		case r == '\u2028' || r == '\u2029':
			buf = append(buf, '\\', 'u', '2', '0', '2', hexDigits[r&0xf])
		default:
			buf = append(buf, s[i:i+size]...)
		}
		i += size
	}
	return append(buf, '"')
}

// Throttle implements "throttle d suppresses further alerts for the same
// key within d" (spec 15.3). A key's window opens when an alert fires and
// lasts d; matches inside it are suppressed and counted, and the first
// match at or after the window end fires again and opens a new window.
// Throttle is not safe for concurrent use; Processor serializes access.
type Throttle struct {
	keys map[string]*throttleState
}

type throttleState struct {
	until      time.Time
	suppressed int // suppressed in the current window
}

// NewThrottle returns an empty throttle.
func NewThrottle() *Throttle { return &Throttle{keys: map[string]*throttleState{}} }

// ThrottleDecision is the outcome of Throttle.Admit.
type ThrottleDecision struct {
	Fire bool
	// SuppressedBefore is, when Fire is true, how many matches the
	// previous window suppressed.
	SuppressedBefore int
	// WindowEnd is when the current window closes.
	WindowEnd time.Time
}

// Admit decides whether a match for key at now fires (window d). d <= 0
// always fires.
func (t *Throttle) Admit(key string, d time.Duration, now time.Time) ThrottleDecision {
	if d <= 0 {
		return ThrottleDecision{Fire: true, WindowEnd: now}
	}
	st, ok := t.keys[key]
	if ok && now.Before(st.until) {
		st.suppressed++
		return ThrottleDecision{Fire: false, WindowEnd: st.until}
	}
	prev := 0
	if ok {
		prev = st.suppressed
	}
	t.keys[key] = &throttleState{until: now.Add(d)}
	return ThrottleDecision{Fire: true, SuppressedBefore: prev, WindowEnd: now.Add(d)}
}

// Suppressed returns the number of matches suppressed in key's current
// window.
func (t *Throttle) Suppressed(key string) int {
	if st, ok := t.keys[key]; ok {
		return st.suppressed
	}
	return 0
}

// Prune forgets windows that ended at or before now and reports how many
// it removed. Counts of suppressed matches in those windows are dropped.
func (t *Throttle) Prune(now time.Time) int {
	n := 0
	for k, st := range t.keys {
		if !now.Before(st.until) {
			delete(t.keys, k)
			n++
		}
	}
	return n
}

// Len returns the number of tracked keys.
func (t *Throttle) Len() int { return len(t.keys) }
