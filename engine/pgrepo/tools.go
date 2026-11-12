package pgrepo

import (
	"context"
	"encoding/json"

	"github.com/genuinebnt/flowforge/engine"
)

// Tools is engine.ToolRegistry over engine.tools (spec 10.4). The spec
// column holds the ToolSpec JSON; spec_sha256 its canonical hash.
type Tools struct {
	db DB
}

var _ engine.ToolRegistry = (*Tools)(nil)

// NewTools returns a registry over db.
func NewTools(db DB) *Tools { return &Tools{db: db} }

// Register stores (or replaces) a tool version and returns it with its
// SpecSHA256.
func (t *Tools) Register(ctx context.Context, s engine.ToolSpec) (engine.ToolSpec, error) {
	s.SpecSHA256 = s.ComputeSHA256()
	b, err := json.Marshal(s)
	if err != nil {
		return s, err
	}
	_, err = t.db.Exec(ctx, `INSERT INTO engine.tools (name, version, spec, spec_sha256, enabled) VALUES ($1, $2, $3::text::jsonb, $4, true)
ON CONFLICT (name, version) DO UPDATE SET spec = EXCLUDED.spec, spec_sha256 = EXCLUDED.spec_sha256, enabled = true`,
		s.Name, s.Version, string(b), s.SpecSHA256[:])
	return s, mapErr("register tool", err)
}

// SetEnabled enables or disables a tool version.
func (t *Tools) SetEnabled(ctx context.Context, name, version string, enabled bool) error {
	_, err := t.db.Exec(ctx, `UPDATE engine.tools SET enabled = $3 WHERE name = $1 AND version = $2`, name, version, enabled)
	return mapErr("set tool enabled", err)
}

func scanTool(row interface{ Scan(...any) error }) (engine.ToolSpec, error) {
	var spec, sha []byte
	if err := row.Scan(&spec, &sha); err != nil {
		return engine.ToolSpec{}, err
	}
	var s engine.ToolSpec
	if err := json.Unmarshal(spec, &s); err != nil {
		return s, err
	}
	copy(s.SpecSHA256[:], sha)
	return s, nil
}

// ListTools implements engine.ToolRegistry: for each name, the enabled
// version with the greatest version text.
func (t *Tools) ListTools(ctx context.Context) ([]engine.ToolSpec, error) {
	rows, err := t.db.Query(ctx, `SELECT DISTINCT ON (name) spec, spec_sha256 FROM engine.tools WHERE enabled
ORDER BY name, version DESC`)
	if err != nil {
		return nil, mapErr("list tools", err)
	}
	defer rows.Close()
	var out []engine.ToolSpec
	for rows.Next() {
		s, err := scanTool(rows)
		if err != nil {
			return nil, mapErr("list tools", err)
		}
		out = append(out, s)
	}
	return out, mapErr("list tools", rows.Err())
}

// ResolveTool implements engine.ToolRegistry. A disabled version still
// resolves: runs already pinned to it keep working.
func (t *Tools) ResolveTool(ctx context.Context, name string, sha [32]byte) (engine.ToolSpec, error) {
	s, err := scanTool(t.db.QueryRow(ctx, `SELECT spec, spec_sha256 FROM engine.tools WHERE name = $1 AND spec_sha256 = $2 LIMIT 1`, name, sha[:]))
	return s, mapErr("resolve tool", err)
}
