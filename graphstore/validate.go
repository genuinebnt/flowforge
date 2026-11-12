package graphstore

import (
	"slices"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
)

// FindQuery is a validated FindEntitiesRequest.
type FindQuery struct {
	Program     ID
	AssetTypes  []string
	ScopeStatus oam.ScopeStatus
	State       oam.State
	SeenAfter   time.Time
	KeyPrefix   string
	Size        int
	After       string // entity id cursor
}

// ValidateFind checks a FindEntitiesRequest.
func ValidateFind(req FindEntitiesRequest) (FindQuery, error) {
	prog, err := ParseID(core.PrefixProgram, "program_id", req.ProgramID)
	if err != nil {
		return FindQuery{}, err
	}
	switch req.ScopeStatus {
	case "", oam.ScopeInScope, oam.ScopeOutOfScope, oam.ScopeUnknown:
	default:
		return FindQuery{}, InvalidArgument("scope_status", "unknown scope_status %q", req.ScopeStatus)
	}
	switch req.State {
	case "", oam.StateActive, oam.StateStale:
	default:
		return FindQuery{}, InvalidArgument("state", "unknown state %q", req.State)
	}
	size, err := PageSize(req.Page)
	if err != nil {
		return FindQuery{}, err
	}
	after, err := DecodeIDToken(req.Page.PageToken)
	if err != nil {
		return FindQuery{}, err
	}
	q := FindQuery{Program: prog, ScopeStatus: req.ScopeStatus, State: req.State, KeyPrefix: req.KeyPrefix, Size: size, After: after}
	for _, t := range req.AssetTypes {
		if t = strings.TrimSpace(t); t != "" && !slices.Contains(q.AssetTypes, t) {
			q.AssetTypes = append(q.AssetTypes, t)
		}
	}
	if !req.SeenAfter.IsZero() {
		q.SeenAfter = StoreTime(req.SeenAfter)
	}
	return q, nil
}

// Match reports whether e passes the filters (not the cursor).
func (q FindQuery) Match(e Entity) bool {
	return (len(q.AssetTypes) == 0 || slices.Contains(q.AssetTypes, e.AssetType)) &&
		(q.ScopeStatus == "" || e.ScopeStatus == q.ScopeStatus) &&
		(q.State == "" || e.State == q.State) &&
		(q.SeenAfter.IsZero() || !e.LastSeen.Before(q.SeenAfter)) &&
		strings.HasPrefix(e.Key, q.KeyPrefix)
}

// DiffQuery is a validated DiffRequest.
type DiffQuery struct {
	Program    ID
	Since      time.Time
	RunA, RunB ID
	Size       int
	After      int64 // seq cursor
}

// ValidateDiff checks a DiffRequest: exactly one of Since and RunIDA, and
// RunIDB only together with RunIDA.
func ValidateDiff(req DiffRequest) (DiffQuery, error) {
	prog, err := ParseID(core.PrefixProgram, "program_id", req.ProgramID)
	if err != nil {
		return DiffQuery{}, err
	}
	runA, err := ParseOptionalID(core.PrefixRun, "run_id_a", req.RunIDA)
	if err != nil {
		return DiffQuery{}, err
	}
	runB, err := ParseOptionalID(core.PrefixRun, "run_id_b", req.RunIDB)
	if err != nil {
		return DiffQuery{}, err
	}
	switch {
	case req.Since.IsZero() == runA.IsZero():
		return DiffQuery{}, InvalidArgument("since", "exactly one of since and run_id_a is required")
	case runA.IsZero() && !runB.IsZero():
		return DiffQuery{}, InvalidArgument("run_id_b", "run_id_b requires run_id_a")
	}
	size, err := PageSize(req.Page)
	if err != nil {
		return DiffQuery{}, err
	}
	after, err := DecodeSeqToken(req.Page.PageToken)
	if err != nil {
		return DiffQuery{}, err
	}
	q := DiffQuery{Program: prog, RunA: runA, RunB: runB, Size: size, After: after}
	if !req.Since.IsZero() {
		q.Since = StoreTime(req.Since)
	}
	return q, nil
}

// ValidateChanges checks a ChangesRequest and returns its program and
// effective limit.
func ValidateChanges(req ChangesRequest) (ID, int, error) {
	prog, err := ParseID(core.PrefixProgram, "program_id", req.ProgramID)
	if err != nil {
		return ID{}, 0, err
	}
	if req.AfterSeq < 0 {
		return ID{}, 0, InvalidArgument("after_seq", "after_seq must not be negative")
	}
	if err := ValidateChangeTypes(req.Types); err != nil {
		return ID{}, 0, err
	}
	limit, err := pageSize(req.Limit)
	return prog, limit, err
}

// ValidateSetStale checks a SetStaleRequest.
func ValidateSetStale(req SetStaleRequest) (program, run ID, tool string, err error) {
	if program, err = ParseID(core.PrefixProgram, "program_id", req.ProgramID); err != nil {
		return
	}
	if run, err = ParseID(core.PrefixRun, "run_id", req.RunID); err != nil {
		return
	}
	if tool = strings.TrimSpace(req.ToolName); tool == "" {
		err = InvalidArgument("tool_name", "tool_name is required")
		return
	}
	if req.StaleAfterRuns < 0 {
		err = InvalidArgument("stale_after_runs", "stale_after_runs must not be negative")
	}
	return
}

// TenantOf canonicalizes the caller's tenant.
func TenantOf(c Caller) (ID, error) { return ParseID(core.PrefixTenant, "tenant_id", c.TenantID) }

// EntityIDOf canonicalizes an entity id argument.
func EntityIDOf(field, s string) (ID, error) { return ParseID(core.PrefixEntity, field, s) }
