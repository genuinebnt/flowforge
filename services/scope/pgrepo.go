package scope

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/services/internal/convert"
)

// PgRepo is Repo over the scope schema (migrations 0003, 0008, 0011). It
// must connect with a role that bypasses row-level security or is the table
// owner; every query also filters by tenant explicitly.
type PgRepo struct {
	DB bus.DB
}

var _ Repo = (*PgRepo)(nil)

// ActorUUID extracts the uuid of a "user:<uuid>" actor (the uuid columns
// created_by and set_by of spec 10.3); other actors are stored as the nil
// uuid.
func ActorUUID(actor string) core.UUID {
	if s, ok := strings.CutPrefix(actor, "user:"); ok {
		if u, err := core.ParseUUID(s); err == nil {
			return u
		}
	}
	return core.Nil
}

func actorFromUUID(u core.UUID) string {
	if u.IsZero() {
		return ""
	}
	return "user:" + u.String()
}

func mapPgErr(err error) error {
	var pe *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case errors.As(err, &pe) && pe.Code == "23505":
		return ErrExists
	}
	return err
}

func (r *PgRepo) emit(ctx context.Context, tx pgx.Tx, evs []Event) error {
	for _, e := range evs {
		_, err := tx.Exec(ctx, `INSERT INTO scope.outbox (event_id, topic, ordering_key, payload) VALUES ($1, $2, $3, $4::text::jsonb) ON CONFLICT (event_id) DO NOTHING`,
			e.ID.String(), convert.TopicScopeEvents, e.Program.String(), string(convert.GenericOutbox(e.ID, e.Type, e.Tenant, e.Program, e.At, e.Payload)))
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *PgRepo) tx(ctx context.Context, f func(pgx.Tx) error) error {
	tx, err := r.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := f(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func insertVersion(ctx context.Context, tx pgx.Tx, v Version) error {
	doc, err := json.Marshal(v.Document)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO scope.scope_versions (program_id, version, document, effective_from, created_by, created_at) VALUES ($1, $2, $3::text::jsonb, $4, $5, $6)`,
		v.Program.String(), v.Version, string(doc), v.EffectiveFrom, ActorUUID(v.CreatedBy).String(), v.CreatedAt)
	return err
}

// CreateProgram implements Repo.
func (r *PgRepo) CreateProgram(ctx context.Context, p Program, initial *Version, evs []Event) (Program, error) {
	if initial != nil {
		p.CurrentVersion = 1
	}
	err := r.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO scope.programs (id, tenant_id, name, platform, status, current_version, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			p.ID.String(), p.Tenant.String(), p.Name, p.Platform, p.Status, p.CurrentVersion, p.CreatedAt); err != nil {
			return err
		}
		if initial != nil {
			initial.Program, initial.Version = p.ID, 1
			if err := insertVersion(ctx, tx, *initial); err != nil {
				return err
			}
		}
		return r.emit(ctx, tx, evs)
	})
	return p, mapPgErr(err)
}

const programCols = `id::text, tenant_id::text, name, coalesce(platform, ''), status, current_version, created_at`

func scanProgram(row pgx.Row) (Program, error) {
	var p Program
	var id, tenant string
	if err := row.Scan(&id, &tenant, &p.Name, &p.Platform, &p.Status, &p.CurrentVersion, &p.CreatedAt); err != nil {
		return Program{}, mapPgErr(err)
	}
	p.ID, p.Tenant = core.MustParseUUID(id), core.MustParseUUID(tenant)
	return p, nil
}

// GetProgram implements Repo.
func (r *PgRepo) GetProgram(ctx context.Context, tenant, id core.UUID) (Program, error) {
	return scanProgram(r.DB.QueryRow(ctx, `SELECT `+programCols+` FROM scope.programs WHERE id = $1 AND tenant_id = $2`, id.String(), tenant.String()))
}

// ListPrograms implements Repo.
func (r *PgRepo) ListPrograms(ctx context.Context, tenant, after core.UUID, limit int) ([]Program, error) {
	rows, err := r.DB.Query(ctx, `SELECT `+programCols+` FROM scope.programs WHERE tenant_id = $1 AND id > $2 ORDER BY id LIMIT $3`, tenant.String(), after.String(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Program
	for rows.Next() {
		p, err := scanProgram(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PutVersion implements Repo.
func (r *PgRepo) PutVersion(ctx context.Context, tenant core.UUID, base int32, v Version, evs []Event) (Version, error) {
	v.Version = base + 1
	err := r.tx(ctx, func(tx pgx.Tx) error {
		var cur int32
		if err := tx.QueryRow(ctx, `SELECT current_version FROM scope.programs WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, v.Program.String(), tenant.String()).Scan(&cur); err != nil {
			return mapPgErr(err)
		}
		if cur != base {
			return ErrConflict
		}
		if err := insertVersion(ctx, tx, v); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE scope.programs SET current_version = $1 WHERE id = $2`, v.Version, v.Program.String()); err != nil {
			return err
		}
		return r.emit(ctx, tx, evs)
	})
	if err != nil {
		return Version{}, mapPgErr(err)
	}
	return v, nil
}

const versionCols = `v.program_id::text, v.version, v.document::text, v.effective_from, v.created_by::text, v.created_at`

func scanVersion(row pgx.Row) (Version, error) {
	var v Version
	var prog, doc, by string
	if err := row.Scan(&prog, &v.Version, &doc, &v.EffectiveFrom, &by, &v.CreatedAt); err != nil {
		return Version{}, mapPgErr(err)
	}
	v.Program = core.MustParseUUID(prog)
	v.CreatedBy = actorFromUUID(core.MustParseUUID(by))
	if err := json.Unmarshal([]byte(doc), &v.Document); err != nil {
		return Version{}, err
	}
	return v, nil
}

// GetVersion implements Repo.
func (r *PgRepo) GetVersion(ctx context.Context, tenant, program core.UUID, n int32) (Version, error) {
	return scanVersion(r.DB.QueryRow(ctx, `SELECT `+versionCols+` FROM scope.scope_versions v JOIN scope.programs p ON p.id = v.program_id
WHERE v.program_id = $1 AND p.tenant_id = $2 AND v.version = CASE WHEN $3::int = 0 THEN p.current_version ELSE $3::int END`,
		program.String(), tenant.String(), n))
}

// EffectiveVersion implements Repo. The scalar subquery yields 0 when no
// version is in effect, and the outer row is missing (ErrNotFound) when
// the program is not the tenant's.
func (r *PgRepo) EffectiveVersion(ctx context.Context, tenant, program core.UUID, at time.Time) (int32, error) {
	var v int32
	err := r.DB.QueryRow(ctx, `SELECT coalesce((SELECT max(v.version) FROM scope.scope_versions v WHERE v.program_id = p.id AND v.effective_from <= $3), 0)
FROM scope.programs p WHERE p.id = $1 AND p.tenant_id = $2`, program.String(), tenant.String(), at).Scan(&v)
	return v, mapPgErr(err)
}

// DueVersions implements Repo. It reads every tenant's programs, like
// KillSwitches, so the role must bypass row-level security (see PgRepo).
func (r *PgRepo) DueVersions(ctx context.Context, after, upTo time.Time, limit int) ([]DueVersion, error) {
	rows, err := r.DB.Query(ctx, `SELECT p.tenant_id::text, v.program_id::text, v.version, v.effective_from
FROM scope.scope_versions v JOIN scope.programs p ON p.id = v.program_id
WHERE v.effective_from > v.created_at AND v.effective_from > $1 AND v.effective_from <= $2
ORDER BY v.effective_from, v.program_id, v.version LIMIT $3`, after, upTo, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueVersion
	for rows.Next() {
		var d DueVersion
		var tenant, prog string
		if err := rows.Scan(&tenant, &prog, &d.Version, &d.EffectiveFrom); err != nil {
			return nil, err
		}
		d.Tenant, d.Program = core.MustParseUUID(tenant), core.MustParseUUID(prog)
		out = append(out, d)
	}
	return out, rows.Err()
}

// AppendEvents implements Repo.
func (r *PgRepo) AppendEvents(ctx context.Context, evs []Event) error {
	return r.tx(ctx, func(tx pgx.Tx) error { return r.emit(ctx, tx, evs) })
}

// ListVersions implements Repo.
func (r *PgRepo) ListVersions(ctx context.Context, tenant, program core.UUID, after int32, limit int) ([]Version, error) {
	if _, err := r.GetProgram(ctx, tenant, program); err != nil {
		return nil, err
	}
	rows, err := r.DB.Query(ctx, `SELECT `+versionCols+` FROM scope.scope_versions v WHERE v.program_id = $1 AND v.version > $2 ORDER BY v.version LIMIT $3`,
		program.String(), after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Version
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SetKillSwitch implements Repo.
func (r *PgRepo) SetKillSwitch(ctx context.Context, ks KillSwitch, evs []Event) (KillSwitch, error) {
	err := r.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO scope.kill_switches (level, target_id, enabled, set_by, set_at, reason) VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (level, target_id) DO UPDATE SET enabled = EXCLUDED.enabled, set_by = EXCLUDED.set_by, set_at = EXCLUDED.set_at, reason = EXCLUDED.reason`,
			ks.Level, ks.Target.String(), ks.Enabled, ActorUUID(ks.SetBy).String(), ks.SetAt, ks.Reason); err != nil {
			return err
		}
		return r.emit(ctx, tx, evs)
	})
	return ks, mapPgErr(err)
}

// KillSwitches implements Repo.
func (r *PgRepo) KillSwitches(ctx context.Context, tenant core.UUID) ([]KillSwitch, error) {
	rows, err := r.DB.Query(ctx, `SELECT level, target_id::text, enabled, coalesce(reason, ''), set_by::text, set_at FROM scope.kill_switches
WHERE level = 'global' OR (level = 'tenant' AND target_id = $1)
   OR (level = 'program' AND target_id IN (SELECT id FROM scope.programs WHERE tenant_id = $1)) ORDER BY level, target_id`, tenant.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KillSwitch
	for rows.Next() {
		var k KillSwitch
		var target, by string
		if err := rows.Scan(&k.Level, &target, &k.Enabled, &k.Reason, &by, &k.SetAt); err != nil {
			return nil, err
		}
		k.Target, k.SetBy = core.MustParseUUID(target), actorFromUUID(core.MustParseUUID(by))
		out = append(out, k)
	}
	return out, rows.Err()
}

// RecordDecisions implements Repo.
func (r *PgRepo) RecordDecisions(ctx context.Context, ds []DecisionRecord) error {
	b := &pgx.Batch{}
	for _, d := range ds {
		ips := make([]string, 0, len(d.Decision.PinnedIPs))
		for _, ip := range d.Decision.PinnedIPs {
			ips = append(ips, ip.String())
		}
		var run *string
		if !d.RunID.IsZero() {
			s := d.RunID.String()
			run = &s
		}
		class := d.Class.String()
		if d.Class == core.ClassNone || d.Class > core.ClassIntrusive {
			class = core.ClassPassive.String() // the column only takes target-facing classes
		}
		result := "deny"
		if d.Decision.Allowed() {
			result = "allow"
		}
		b.Queue(`INSERT INTO scope.scope_decisions (id, tenant_id, program_id, scope_version, run_id, effect_path, target, class, decision, reason, rule_id, pinned_ips, decided_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, nullif($11, ''), $12::text[]::inet[], $13)`,
			d.ID.String(), d.Tenant.String(), d.Program.String(), d.ScopeVersion, run, d.EffectPath, d.Decision.Target, class, result,
			d.Decision.Reason, d.Decision.RuleID, ips, d.Decision.DecidedAt)
	}
	return r.tx(ctx, func(tx pgx.Tx) error { return tx.SendBatch(ctx, b).Close() })
}

// GetDecision implements Repo.
func (r *PgRepo) GetDecision(ctx context.Context, tenant, id core.UUID) (DecisionRecord, error) {
	var d DecisionRecord
	var prog, run, class, result string
	var ips []string
	err := r.DB.QueryRow(ctx, `SELECT program_id::text, scope_version, coalesce(run_id::text, ''), coalesce(effect_path, ''), target, class, decision, reason,
coalesce(rule_id, ''), pinned_ips::text[], decided_at FROM scope.scope_decisions WHERE id = $1 AND tenant_id = $2 ORDER BY decided_at DESC LIMIT 1`,
		id.String(), tenant.String()).Scan(&prog, &d.ScopeVersion, &run, &d.EffectPath, &d.Decision.Target, &class, &result,
		&d.Decision.Reason, &d.Decision.RuleID, &ips, &d.Decision.DecidedAt)
	if err != nil {
		return DecisionRecord{}, mapPgErr(err)
	}
	d.ID, d.Tenant, d.Program = id, tenant, core.MustParseUUID(prog)
	if run != "" {
		d.RunID = core.MustParseUUID(run)
	}
	d.Class, _ = core.ParseActivityClass(class)
	d.Decision.ID = core.PublicID(core.PrefixDecision, id)
	d.Decision.ScopeVersion = int(d.ScopeVersion)
	d.Decision.Result = scope.ResultDeny
	if result == "allow" {
		d.Decision.Result = scope.ResultAllow
	}
	d.Decision.PinnedIPs = []netip.Addr{}
	for _, s := range ips {
		if p, err := netip.ParsePrefix(s); err == nil {
			d.Decision.PinnedIPs = append(d.Decision.PinnedIPs, p.Addr())
		} else if a, err := netip.ParseAddr(s); err == nil {
			d.Decision.PinnedIPs = append(d.Decision.PinnedIPs, a)
		}
	}
	return d, nil
}

// Outbox implements Repo.
func (r *PgRepo) Outbox() bus.OutboxSource { return bus.PgOutbox{DB: r.DB, Table: "scope.outbox"} }

// Ping implements Repo.
func (r *PgRepo) Ping(ctx context.Context) error {
	_, err := r.DB.Exec(ctx, `SELECT 1`)
	return err
}
