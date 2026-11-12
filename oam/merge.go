package oam

import (
	"bytes"
	"cmp"
	"maps"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Stamp orders writes for last-writer-wins (spec 23.5): by observed_at,
// then source (tool name), then observation id, which makes the order
// total across distinct observations.
type Stamp struct {
	At          time.Time
	Source      string
	Observation string
}

// Compare returns -1, 0 or +1 as s orders before, equal to or after o.
func (s Stamp) Compare(o Stamp) int {
	if c := s.At.Compare(o.At); c != 0 {
		return c
	}
	if c := cmp.Compare(s.Source, o.Source); c != 0 {
		return c
	}
	return cmp.Compare(s.Observation, o.Observation)
}

// PatchOp is one RFC 6902 operation of an entity_changed patch. Paths are
// JSON pointers into the asset content, for example "/title".
type PatchOp struct {
	Op    string `json:"op"` // "add" or "replace"
	Path  string `json:"path"`
	Value any    `json:"value"`
}

// Content is an asset (or relation, or property) content object together
// with the stamp of the write that set each top-level field. Each field is
// a last-writer-wins register over non-empty values: an empty value (null,
// "", [] or {}) never overwrites, a newer stamp wins, and equal stamps
// with different values are settled by comparing the canonical JSON of
// the values. That makes merging commutative, associative and idempotent,
// so the result does not depend on arrival order or duplication. Nested
// objects and arrays are registers as a whole.
type Content struct {
	Values map[string]any
	Stamps map[string]Stamp
}

// NewContent returns content written entirely by one stamp.
func NewContent(values map[string]any, s Stamp) Content {
	c := Content{Values: map[string]any{}, Stamps: map[string]Stamp{}}
	c.Merge(values, s)
	return c
}

// Merge applies one write and returns the effective changes in field
// order. Merge mutates c.
func (c *Content) Merge(values map[string]any, s Stamp) []PatchOp {
	if c.Values == nil {
		c.Values = map[string]any{}
	}
	if c.Stamps == nil {
		c.Stamps = map[string]Stamp{}
	}
	var ops []PatchOp
	for _, k := range slices.Sorted(maps.Keys(values)) {
		if op, ok := c.mergeField(k, values[k], s); ok {
			ops = append(ops, op)
		}
	}
	return ops
}

func (c *Content) mergeField(k string, v any, s Stamp) (PatchOp, bool) {
	if isEmpty(v) {
		return PatchOp{}, false
	}
	cur, ok := c.Values[k]
	if !ok {
		c.Values[k], c.Stamps[k] = v, s
		return PatchOp{Op: "add", Path: pointer(k), Value: v}, true
	}
	switch s.Compare(c.Stamps[k]) {
	case -1:
		return PatchOp{}, false
	case 0:
		if reflect.DeepEqual(cur, v) || compareValues(v, cur) <= 0 {
			return PatchOp{}, false
		}
	}
	c.Stamps[k] = s
	if reflect.DeepEqual(cur, v) {
		return PatchOp{}, false
	}
	c.Values[k] = v
	return PatchOp{Op: "replace", Path: pointer(k), Value: v}, true
}

// JoinContent returns the merge of a and b without modifying either. It is
// the lattice join behind Merge: JoinContent(a, b) == JoinContent(b, a).
func JoinContent(a, b Content) Content {
	out := Content{Values: make(map[string]any, len(a.Values)), Stamps: make(map[string]Stamp, len(a.Stamps))}
	for k, v := range a.Values {
		out.Values[k], out.Stamps[k] = v, a.Stamps[k]
	}
	for _, k := range slices.Sorted(maps.Keys(b.Values)) {
		out.mergeField(k, b.Values[k], b.Stamps[k])
	}
	return out
}

// isEmpty reports whether a JSON value counts as "not reported".
func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// compareValues orders two JSON values by their canonical encoding.
func compareValues(a, b any) int {
	ab, errA := core.CanonicalJSON(a)
	bb, errB := core.CanonicalJSON(b)
	if errA != nil || errB != nil {
		return 0
	}
	return bytes.Compare(ab, bb)
}

// pointer returns the RFC 6901 JSON pointer of a top-level field.
func pointer(field string) string {
	if !strings.ContainsAny(field, "~/") {
		return "/" + field
	}
	return "/" + strings.ReplaceAll(strings.ReplaceAll(field, "~", "~0"), "/", "~1")
}

// MinTime returns the earlier of a and b (created_at merge rule).
func MinTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// MaxTime returns the later of a and b (last_seen merge rule).
func MaxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// EdgeKey is the upsert identity of an edge (spec 5.7): endpoints,
// relation type, label and discriminator, scoped to a tenant and program.
type EdgeKey struct {
	TenantID, ProgramID string
	From, To            string // entity ids
	RelationType        string
	Label               string
	Discriminator       string
}

// ID returns the deterministic edge id for k.
func (k EdgeKey) ID() string {
	return EdgeID(k.TenantID, k.ProgramID, k.From, k.To, k.RelationType, k.Label, k.Discriminator)
}
