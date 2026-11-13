package migrations

import (
	"context"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

//go:embed sql/*.sql
var sqlFS embed.FS

// lockKey is the pg_advisory_xact_lock key that serializes migrators
// ("flowforg" in ASCII).
const lockKey int64 = 0x666c6f77666f7267

// Errors returned by the runner.
var (
	// ErrChecksumMismatch reports that an applied migration file was edited
	// afterwards. Migrations are forward-only (spec 10.7): fix forward with
	// a new file instead.
	ErrChecksumMismatch = errors.New("migrations: applied migration was modified")
	// ErrBadFile reports an embedded file that does not follow the naming
	// rules (NNNN_name.sql, versions unique and positive).
	ErrBadFile = errors.New("migrations: malformed migration file set")
)

// Migration is one embedded, numbered SQL file.
type Migration struct {
	Version int    // 1-based, from the NNNN prefix
	Name    string // the part after the prefix, without ".sql"
	SQL     string
	// Checksum is the SHA-256 of the file. A re-run refuses to proceed
	// when an applied migration's recorded checksum differs.
	Checksum [32]byte
	// RequiresExtension names an extension that must be available on the
	// server (pg_available_extensions) for the file to be applied. It is
	// declared with a first-line comment "-- requires-extension: <name>".
	RequiresExtension string
}

// File returns the file name, for example "0004_engine.sql".
func (m Migration) File() string { return fmt.Sprintf("%04d_%s.sql", m.Version, m.Name) }

// Status is the recorded outcome of a migration.
type Status string

// Migration outcomes recorded in public.schema_migrations.
const (
	StatusApplied Status = "applied"
	StatusSkipped Status = "skipped" // a required extension is not available
)

// Result reports what Apply did with one migration.
type Result struct {
	Migration Migration
	Status    Status
	// Changed is true when this call applied the file or recorded the
	// skip, false when an earlier run already had.
	Changed bool
}

// Beginner starts transactions. *pgx.Conn satisfies it, and so do
// *pgxpool.Pool and *pgxpool.Conn: every migration runs in its own
// transaction holding a transaction-level advisory lock, so a pool may hand
// out a different connection each time.
type Beginner interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

var fileRE = regexp.MustCompile(`^([0-9]{4})_([a-z0-9_]+)\.sql$`)
var requiresRE = regexp.MustCompile(`^--\s*requires-extension:\s*([a-z0-9_]+)\s*$`)

// All returns the embedded migrations in version order.
//
// Versions must be unique, but gaps are allowed: 0015-0029 are reserved for
// v1 fixes made on feat_v1 and merged forward, while v2 migrations start at
// 0030 (docs/V2_DESIGN.md "Migration numbering"). Apply applies every
// unapplied file in version order, so a reserved-range fix that arrives
// after a higher version was applied still runs; migrations in the reserved
// range must therefore not depend on any later migration.
func All() ([]Migration, error) { return load(sqlFS) }

// load reads the migration files under sql/ in fsys.
func load(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, "sql")
	if err != nil {
		return nil, fmt.Errorf("migrations: read embedded files: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := fileRE.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("%w: unexpected file %q", ErrBadFile, e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		b, err := fs.ReadFile(fsys, path.Join("sql", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("migrations: read %s: %w", e.Name(), err)
		}
		mig := Migration{Version: v, Name: m[2], SQL: string(b), Checksum: sha256.Sum256(b)}
		first, _, _ := strings.Cut(mig.SQL, "\n")
		if r := requiresRE.FindStringSubmatch(strings.TrimSpace(first)); r != nil {
			mig.RequiresExtension = r[1]
		}
		out = append(out, mig)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version < 1 {
			return nil, fmt.Errorf("%w: version must be positive in %s", ErrBadFile, m.File())
		}
		if i > 0 && out[i-1].Version == m.Version {
			return nil, fmt.Errorf("%w: duplicate version %04d (%s, %s)", ErrBadFile, m.Version, out[i-1].File(), m.File())
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: no migrations embedded", ErrBadFile)
	}
	return out, nil
}

// Apply brings the database up to date: it applies, in order, every
// embedded migration that is not yet recorded in public.schema_migrations.
// It is safe to call concurrently from several processes and idempotent.
func Apply(ctx context.Context, db Beginner) error {
	_, err := ApplyReport(ctx, db)
	return err
}

// ApplyReport is Apply that also returns the outcome for every migration.
func ApplyReport(ctx context.Context, db Beginner) ([]Result, error) {
	all, err := All()
	if err != nil {
		return nil, err
	}
	if err := inTx(ctx, db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS public.schema_migrations (
  version    integer PRIMARY KEY,
  name       text NOT NULL,
  checksum   bytea NOT NULL,
  status     text NOT NULL CHECK (status IN ('applied','skipped')),
  applied_at timestamptz NOT NULL DEFAULT now()
)`)
		return err
	}); err != nil {
		return nil, fmt.Errorf("migrations: create schema_migrations: %w", err)
	}
	results := make([]Result, 0, len(all))
	resync := false
	for _, m := range all {
		var res Result
		err := inTx(ctx, db, func(tx pgx.Tx) error {
			r, err := applyOne(ctx, tx, m)
			res = r
			return err
		})
		if err != nil {
			return results, fmt.Errorf("migrations: %s: %w", m.File(), err)
		}
		results = append(results, res)
		// 0018 secures what exists when it runs; a later file's new tables
		// need the same pass (FORCE RLS, policies, grants) before any
		// service uses them.
		resync = resync || (res.Changed && res.Status == StatusApplied && m.Version > rolesVersion)
	}
	if resync {
		if err := SyncRoles(ctx, db); err != nil {
			return results, err
		}
	}
	return results, nil
}

// Deploy is what the migrations job (cmd/migrate, spec 17.1 ff-migrate)
// runs: ApplyReport, then SyncRoles, then a RoleProblems check that fails
// with ErrRoleProblems when any service role is over-privileged (for
// example still in cloudsqlsuperuser because the migrator could not revoke
// it): a deploy must not continue with roles that defeat the per-schema
// grants.
func Deploy(ctx context.Context, db Beginner) ([]Result, error) {
	res, err := ApplyReport(ctx, db)
	if err != nil {
		return res, err
	}
	if err := SyncRoles(ctx, db); err != nil {
		return res, err
	}
	problems, err := RoleProblems(ctx, db)
	if err != nil {
		return res, err
	}
	if len(problems) > 0 {
		return res, fmt.Errorf("%w: %s", ErrRoleProblems, strings.Join(problems, "; "))
	}
	return res, nil
}

// rolesVersion is the migration that defines public.ff_sync_roles.
const rolesVersion = 18

// ErrRoleProblems reports a service role with more privilege than its own
// schema needs (see RoleProblems).
var ErrRoleProblems = errors.New("migrations: service database roles are over-privileged")

// SyncRoles re-runs public.ff_sync_roles() (migration 0018, spec 10.7 and
// 17.2): it creates the missing service roles (NOLOGIN), forces row-level
// security with the tenant, parent and role-scoped policies on every
// tenant-scoped table and partition, grants each role its own schema only,
// revokes PUBLIC, and removes the roles from cloudsqlsuperuser where that
// role exists. Idempotent; cmd/migrate calls it after every Apply.
func SyncRoles(ctx context.Context, db Beginner) error {
	if err := inTx(ctx, db, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT public.ff_sync_roles()`)
		return err
	}); err != nil {
		return fmt.Errorf("migrations: sync roles: %w", err)
	}
	return nil
}

// RoleProblems lists, as "role: problem" lines, every service role that is
// a superuser, has BYPASSRLS, CREATEROLE or CREATEDB, is still a member of
// cloudsqlsuperuser, or owns a service table (public.ff_role_problems).
// An empty list means the roles hold exactly what SyncRoles granted.
func RoleProblems(ctx context.Context, db Beginner) ([]string, error) {
	var out []string
	err := inTx(ctx, db, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT public.ff_role_problems()`)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("migrations: role problems: %w", err)
	}
	return out, nil
}

// applyOne runs inside a transaction that holds the migration lock.
func applyOne(ctx context.Context, tx pgx.Tx, m Migration) (Result, error) {
	res := Result{Migration: m}
	var (
		status   string
		checksum []byte
	)
	err := tx.QueryRow(ctx, `SELECT status, checksum FROM public.schema_migrations WHERE version = $1`, m.Version).
		Scan(&status, &checksum)
	recorded := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return res, err
	}
	if recorded && Status(status) == StatusApplied {
		if string(checksum) != string(m.Checksum[:]) {
			return res, ErrChecksumMismatch
		}
		res.Status = StatusApplied
		return res, nil
	}
	if m.RequiresExtension != "" {
		var available bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = $1)`,
			m.RequiresExtension).Scan(&available); err != nil {
			return res, err
		}
		if !available {
			res.Status = StatusSkipped
			if recorded {
				return res, nil
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO public.schema_migrations (version, name, checksum, status) VALUES ($1, $2, $3, 'skipped')`,
				m.Version, m.Name, m.Checksum[:])
			res.Changed = err == nil
			return res, err
		}
	}
	// Exec without arguments uses the simple protocol, which accepts a
	// multi-statement script.
	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return res, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO public.schema_migrations (version, name, checksum, status)
VALUES ($1, $2, $3, 'applied')
ON CONFLICT (version) DO UPDATE SET name = EXCLUDED.name, checksum = EXCLUDED.checksum,
  status = 'applied', applied_at = now()`, m.Version, m.Name, m.Checksum[:]); err != nil {
		return res, err
	}
	res.Status, res.Changed = StatusApplied, true
	return res, nil
}

// inTx runs fn in a transaction holding the migration advisory lock.
func inTx(ctx context.Context, db Beginner, fn func(pgx.Tx) error) (err error) {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback(ctx)
		}
	}()
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, lockKey); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
