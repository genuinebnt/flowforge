package graphstore

import (
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/core"
)

// ID is a canonicalized identifier: its public text form (for example
// "ten_01JD...") and the UUID stored in Postgres. Every id that enters the
// store is canonicalized first, so a public id and its UUID spelling name
// the same row, and Mem and the Postgres store derive identical entity
// and edge ids from identical inputs.
type ID struct {
	Text string
	UUID core.UUID
}

// IsZero reports whether id is unset.
func (id ID) IsZero() bool { return id.Text == "" }

// ParseID canonicalizes s, which must be a public id with prefix p or a
// UUID in its 36-character form. The error is an INVALID_ARGUMENT
// *core.Error naming field.
func ParseID(p core.Prefix, field, s string) (ID, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ID{}, InvalidArgument(field, "%s is required", field)
	}
	if len(s) != 36 && !strings.HasPrefix(s, string(p)+"_") {
		return ID{}, InvalidArgument(field, "%s %q is not a %s_ identifier", field, s, p)
	}
	u, err := core.ParsePublicID(p, s)
	if err != nil {
		return ID{}, InvalidArgument(field, "%s %q is not a %s_ identifier", field, s, p)
	}
	return IDFromUUID(p, u), nil
}

// ParseOptionalID is ParseID that accepts the empty string (zero ID).
func ParseOptionalID(p core.Prefix, field, s string) (ID, error) {
	if strings.TrimSpace(s) == "" {
		return ID{}, nil
	}
	return ParseID(p, field, s)
}

// IDFromUUID returns the canonical ID of u with prefix p.
func IDFromUUID(p core.Prefix, u core.UUID) ID {
	return ID{Text: core.PublicID(p, u), UUID: u}
}

// DeriveUUID returns an RFC 9562 version 8 UUID built from SHA-256 of a
// domain-separated, length-prefixed tuple. It is the same construction
// oam uses for entity and edge ids, so every id the graph store mints is
// a pure function of its identity and replays produce the same ids.
//
// PORT: each part is written as unsigned LEB128 (binary.PutUvarint) of
// its byte length followed by its bytes, exactly as oam's derivation
// (docs/PORTING_RUST.md 4.1); these ids are primary keys shared with any
// Rust writer.
func DeriveUUID(domain string, parts ...string) core.UUID {
	h := sha256.New()
	var n [binary.MaxVarintLen64]byte
	write := func(s string) {
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(s)))])
		h.Write([]byte(s))
	}
	write(domain)
	for _, p := range parts {
		write(p)
	}
	var u core.UUID
	copy(u[:], h.Sum(nil))
	u[6] = (u[6] & 0x0f) | 0x80 // version 8
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 9562 variant
	return u
}

// ChangeUUID returns the id of the idx-th change event emitted by one
// graph operation (an observation id, or "run:<tool>:<run>" for a covering
// run). It reproduces oam's derivation, so Mem (built on oam.Graph) and
// the Postgres store assign identical change ids.
func ChangeUUID(tenantID, programID, origin string, idx int) core.UUID {
	return DeriveUUID("ff-oam-change", tenantID, programID, origin, strconv.Itoa(idx))
}

// TagUUID returns the id of the tag (ptype, name) on a target, where kind
// is TargetEntity or TargetEdge.
func TagUUID(tenantID string, kind TargetKind, targetID, ptype, name string) core.UUID {
	return DeriveUUID("ff-graph-tag", tenantID, string(kind), targetID, ptype, name)
}

// ObservationUUID returns the graph.observations row id of an observation.
// Observation ids are deduplicated per tenant (spec 6.1), so the row id is
// derived from the tenant and the id as sent: two tenants can never
// collide, and neither can suppress the other's observation.
func ObservationUUID(tenantID, observationID string) core.UUID {
	return DeriveUUID("ff-graph-observation", tenantID, observationID)
}

// CoveringRunUUID returns the row id under which a completed covering run
// is recorded, which makes SetStale idempotent per (tenant, program, tool,
// run).
func CoveringRunUUID(tenantID, programID, tool, runID string) core.UUID {
	return DeriveUUID("ff-graph-covering-run", tenantID, programID, tool, runID)
}

// SnapshotUUID returns the catalogue id of the snapshot of a program at a
// watermark.
func SnapshotUUID(tenantID, programID string, watermark uint64) core.UUID {
	return DeriveUUID("ff-graph-snapshot", tenantID, programID, strconv.FormatUint(watermark, 10))
}

// CoveringRunOrigin is the change-event origin of a covering run (see
// ChangeUUID); it matches oam.Graph.CompleteRun.
func CoveringRunOrigin(tool, runID string) string { return "run:" + tool + ":" + runID }
