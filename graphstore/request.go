package graphstore

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
)

// PreparedIngest is an IngestRequest after the boundary checks every
// implementation applies: canonical ids, request and envelope fields
// cross-checked, observed_at normalized with StoreTime.
type PreparedIngest struct {
	Tenant, Program, Run ID
	EffectPath           string
	ToolVersion          string
	// Observation carries the canonical tenant, program and run ids and
	// the effective tool name, so oam derives the same entity ids in every
	// implementation.
	Observation oam.Observation
}

// PrepareIngest normalizes and cross-checks req. The observation itself is
// validated by the implementation (oam rules), not here.
func PrepareIngest(req IngestRequest) (PreparedIngest, error) {
	o := req.Observation
	tenant, err := ParseID(core.PrefixTenant, "tenant_id", req.TenantID)
	if err != nil {
		return PreparedIngest{}, err
	}
	if o.TenantID != "" {
		ot, err := ParseID(core.PrefixTenant, "observation.tenant_id", o.TenantID)
		if err != nil {
			return PreparedIngest{}, err
		}
		if ot != tenant {
			return PreparedIngest{}, InvalidArgument("observation.tenant_id", "observation tenant_id does not match the caller's tenant")
		}
	}
	program, err := agreeID(core.PrefixProgram, "program_id", req.ProgramID, o.ProgramID, true)
	if err != nil {
		return PreparedIngest{}, err
	}
	run, err := agreeID(core.PrefixRun, "run_id", req.RunID, o.RunID, false)
	if err != nil {
		return PreparedIngest{}, err
	}
	tool, err := agree("tool_name", req.ToolName, o.Tool.Name)
	if err != nil {
		return PreparedIngest{}, err
	}
	version, err := agree("tool_version", req.ToolVersion, o.Tool.Version)
	if err != nil {
		return PreparedIngest{}, err
	}
	o.TenantID, o.ProgramID, o.RunID = tenant.Text, program.Text, run.Text
	o.Tool = oam.Tool{Name: tool, Version: version}
	o.ObservedAt = StoreTime(o.ObservedAt)
	if err := rejectNUL(o); err != nil {
		return PreparedIngest{}, err
	}
	return PreparedIngest{Tenant: tenant, Program: program, Run: run, EffectPath: req.EffectPath, ToolVersion: version, Observation: o}, nil
}

func agreeID(p core.Prefix, field, a, b string, required bool) (ID, error) {
	ia, err := ParseOptionalID(p, field, a)
	if err != nil {
		return ID{}, err
	}
	ib, err := ParseOptionalID(p, "observation."+field, b)
	if err != nil {
		return ID{}, err
	}
	switch {
	case !ia.IsZero() && !ib.IsZero() && ia != ib:
		return ID{}, InvalidArgument(field, "%s %q does not match the observation's %q", field, a, b)
	case !ia.IsZero():
		return ia, nil
	case !ib.IsZero():
		return ib, nil
	case required:
		return ID{}, InvalidArgument(field, "%s is required", field)
	}
	return ID{}, nil
}

func agree(field, a, b string) (string, error) {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a != "" && b != "" && a != b {
		return "", InvalidArgument(field, "%s %q does not match the observation's %q", field, a, b)
	}
	if a != "" {
		return a, nil
	}
	return b, nil
}

// rejectNUL refuses NUL characters anywhere in the envelope: Postgres
// text and jsonb cannot store them, so both implementations reject them.
func rejectNUL(o oam.Observation) error {
	b, err := oam.EncodeObservation(o)
	if err != nil {
		return Classify(err)
	}
	if !bytes.Contains(b, []byte(`\u0000`)) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return Classify(err)
	}
	if hasNUL(v) {
		return InvalidArgument("observation", "observation contains a NUL character")
	}
	return nil
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

// pageSize applies the PageRequest defaults and bounds.
func pageSize(n int) (int, error) {
	switch {
	case n == 0:
		return DefaultPageSize, nil
	case n < 0 || n > MaxPageSize:
		return 0, InvalidArgument("page_size", "page_size %d not in 1..%d", n, MaxPageSize)
	}
	return n, nil
}

// PageSize returns the effective page size of p (DefaultPageSize when
// zero) or INVALID_ARGUMENT outside 1..MaxPageSize.
func PageSize(p PageRequest) (int, error) { return pageSize(p.PageSize) }
