// Package convert holds every proto <-> domain conversion of the internal
// services in one place: timestamps and durations, google.protobuf.Struct
// and Value <-> JSON, public ids (prefix_...) <-> UUIDs, enums, diagnostics,
// pages, scope documents and decisions, tool specs, observations, graph
// records and event envelopes. Handlers stay thin by calling these.
package convert

import (
	"encoding/json"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
)

// TS converts a time to a Timestamp; the zero time becomes nil.
func TS(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// Time converts a Timestamp to a UTC time; nil becomes the zero time.
func Time(ts *timestamppb.Timestamp) time.Time {
	if ts == nil {
		return time.Time{}
	}
	return ts.AsTime().UTC()
}

// Dur converts a duration (0 becomes nil).
func Dur(d time.Duration) *durationpb.Duration {
	if d == 0 {
		return nil
	}
	return durationpb.New(d)
}

// Duration converts a proto Duration (nil is 0).
func Duration(d *durationpb.Duration) time.Duration {
	if d == nil {
		return 0
	}
	return d.AsDuration()
}

// Invalid returns an INVALID_ARGUMENT VALIDATION_FAILED error naming field.
func Invalid(field, format string, args ...any) error {
	e := core.Errorf(core.KindInvalidArgument, core.ReasonValidationFailed, format, args...)
	if field != "" {
		e.Meta = map[string]string{"field": field}
	}
	return e
}

// ParseID parses a required public id with prefix p (or a bare UUID).
func ParseID(p core.Prefix, field, s string) (core.UUID, error) {
	if s == "" {
		return core.UUID{}, Invalid(field, "%s is required", field)
	}
	u, err := core.ParsePublicID(p, s)
	if err != nil {
		return core.UUID{}, Invalid(field, "%s is not a valid %s_ id", field, p)
	}
	return u, nil
}

// OptionalID is ParseID that accepts "" as the zero UUID.
func OptionalID(p core.Prefix, field, s string) (core.UUID, error) {
	if s == "" {
		return core.UUID{}, nil
	}
	return ParseID(p, field, s)
}

// ID renders u as a public id; the zero UUID becomes "".
func ID(p core.Prefix, u core.UUID) string {
	if u.IsZero() {
		return ""
	}
	return core.PublicID(p, u)
}

// StructFromJSON converts a JSON object to a Struct (nil or "null" give nil).
func StructFromJSON(raw []byte) (*structpb.Struct, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	s := &structpb.Struct{}
	if err := s.UnmarshalJSON(raw); err != nil {
		return nil, err
	}
	return s, nil
}

// MustStruct is StructFromJSON that returns nil on error (for values that
// are known to be JSON objects).
func MustStruct(raw []byte) *structpb.Struct {
	s, _ := StructFromJSON(raw)
	return s
}

// StructFromMap converts a map (nil gives nil).
func StructFromMap(m map[string]any) *structpb.Struct {
	if m == nil {
		return nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return MustStruct(b)
}

// StructToJSON converts a Struct to a JSON object ({} for nil).
func StructToJSON(s *structpb.Struct) (json.RawMessage, error) {
	if s == nil {
		return json.RawMessage("{}"), nil
	}
	b, err := s.MarshalJSON()
	return b, err
}

// StructToMap converts a Struct to a map (nil for nil).
func StructToMap(s *structpb.Struct) map[string]any {
	if s == nil {
		return nil
	}
	b, err := s.MarshalJSON()
	if err != nil {
		return nil
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// ValueFromJSON converts any JSON value (nil or empty gives nil).
func ValueFromJSON(raw []byte) (*structpb.Value, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	v := &structpb.Value{}
	if err := v.UnmarshalJSON(raw); err != nil {
		return nil, err
	}
	return v, nil
}

// ValueToJSON converts a Value to JSON ("null" for nil).
func ValueToJSON(v *structpb.Value) (json.RawMessage, error) {
	if v == nil {
		return json.RawMessage("null"), nil
	}
	return v.MarshalJSON()
}

// Class converts a domain activity class.
func Class(c core.ActivityClass) pb.ActivityClass {
	switch c {
	case core.ClassPassive:
		return pb.ActivityClass_ACTIVITY_CLASS_PASSIVE
	case core.ClassActive:
		return pb.ActivityClass_ACTIVITY_CLASS_ACTIVE
	case core.ClassIntrusive:
		return pb.ActivityClass_ACTIVITY_CLASS_INTRUSIVE
	}
	return pb.ActivityClass_ACTIVITY_CLASS_UNSPECIFIED
}

// ClassFrom converts a proto activity class (UNSPECIFIED is ClassNone).
func ClassFrom(c pb.ActivityClass) core.ActivityClass {
	switch c {
	case pb.ActivityClass_ACTIVITY_CLASS_PASSIVE:
		return core.ClassPassive
	case pb.ActivityClass_ACTIVITY_CLASS_ACTIVE:
		return core.ClassActive
	case pb.ActivityClass_ACTIVITY_CLASS_INTRUSIVE:
		return core.ClassIntrusive
	}
	return core.ClassNone
}

// PageSize returns the effective page size (default 50, max 500, spec 11.1).
func PageSize(p *pb.PageRequest) (int, error) {
	n := int(p.GetPageSize())
	switch {
	case n == 0:
		return 50, nil
	case n < 0 || n > 500:
		return 0, Invalid("page.page_size", "page_size must be between 1 and 500")
	}
	return n, nil
}

// Page returns the size and token of a page request.
func Page(p *pb.PageRequest) (int, string, error) {
	n, err := PageSize(p)
	return n, p.GetPageToken(), err
}

// PageResp builds a PageResponse (nil when there is no next page).
func PageResp(next string) *pb.PageResponse {
	if next == "" {
		return nil
	}
	return &pb.PageResponse{NextPageToken: next}
}
