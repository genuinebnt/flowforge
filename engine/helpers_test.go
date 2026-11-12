package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

type (
	contextT = context.Context
	jsonRaw  = json.RawMessage
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// wantReason fails the test unless err is a *core.Error with the kind and
// reason.
func wantReason(t *testing.T, err error, kind core.Kind, reason string) {
	t.Helper()
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Kind != kind || ce.Reason != reason {
		t.Fatalf("error %v, want %s %s", err, kind, reason)
	}
}

func splitLines(s string) []string { return strings.Split(strings.TrimSpace(s), "\n") }

func containsStr(s, sub string) bool { return strings.Contains(s, sub) }

var seqRe = regexp.MustCompile(`"seq":\d+`)

// stripSeq removes the seq field of a normalized event line.
func stripSeq(line string) string { return strings.TrimSuffix(seqRe.ReplaceAllString(line, ""), ",") }
