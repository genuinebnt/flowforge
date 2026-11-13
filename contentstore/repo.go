package contentstore

import (
	"context"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Repo is the snapshot repository port (V2 §6.2): the content schema's
// rows. Implementations: MemRepo (in memory, the fake every test and the
// dev stack use) and contentstore/pg (Postgres, row-level security set
// per transaction).
type Repo interface {
	// InTx runs fn in one transaction scoped to tenant: every read and
	// write of fn sees only that tenant's rows (Postgres: app.tenant_id is
	// set in the transaction, so RLS enforces it). fn's error rolls back
	// everything fn wrote.
	InTx(ctx context.Context, tenant core.UUID, fn func(Tx) error) error
	// Tenants lists the tenants that hold content (background retention
	// and indexing sweeps).
	Tenants(ctx context.Context) ([]core.UUID, error)
	// Ping checks the backing store.
	Ping(ctx context.Context) error
}

// SnapshotFilter selects snapshots for ListSnapshots. Program is
// required; URL and Profile narrow to one key. Results are ordered by
// (ValidFrom, ID) descending; Before continues after that position.
type SnapshotFilter struct {
	Program  core.UUID
	URL      string
	Profile  string
	Before   *Cursor
	Limit    int
	OpenOnly bool
}

// Cursor is a position in snapshot order.
type Cursor struct {
	ValidFrom time.Time
	ID        core.UUID
}

// Tx is the transactional view of one tenant. Every method acts on the
// transaction's tenant only.
type Tx interface {
	// Programs lists the programs with content.
	Programs(ctx context.Context) ([]core.UUID, error)
	// EnsureProgram records a program and returns it (idempotent).
	EnsureProgram(ctx context.Context, program core.UUID) error

	// OpenVersion returns the key's open version, locking it against
	// concurrent ingests until the transaction ends.
	OpenVersion(ctx context.Context, k Key) (Snapshot, bool, error)
	// SnapshotBySignature returns a snapshot of k with this signature and
	// fetch time (the exact re-send of a stored capture).
	SnapshotBySignature(ctx context.Context, k Key, signature string, fetchedAt time.Time) (Snapshot, bool, error)
	// GetSnapshot returns a snapshot by id.
	GetSnapshot(ctx context.Context, id core.UUID) (Snapshot, bool, error)
	// SnapshotAsOf returns the version of k valid at t: valid_from <= t <
	// valid_to (or open). One index probe in Postgres (V2 §12.12).
	SnapshotAsOf(ctx context.Context, k Key, t time.Time) (Snapshot, bool, error)
	// ListSnapshots pages a program's snapshots in SnapshotFilter order.
	ListSnapshots(ctx context.Context, f SnapshotFilter) ([]Snapshot, error)
	// KeySnapshots returns every snapshot of k, newest first.
	KeySnapshots(ctx context.Context, k Key) ([]Snapshot, error)
	// Keys lists a program's keys.
	Keys(ctx context.Context, program core.UUID) ([]Key, error)
	// HostOpenVersions returns up to limit open versions on host other
	// than url (catch-all detection, lineage candidates), newest first.
	HostOpenVersions(ctx context.Context, program core.UUID, host, profile, url string, limit int) ([]Snapshot, error)
	// OpenVersionsByURL returns the open versions of the given URLs.
	OpenVersionsByURL(ctx context.Context, program core.UUID, profile string, urls []string) ([]Snapshot, error)
	// InsertSnapshot stores a new snapshot row.
	InsertSnapshot(ctx context.Context, s Snapshot) error
	// CloseVersion sets valid_to of an open version.
	CloseVersion(ctx context.Context, id core.UUID, validTo time.Time) error
	// Touch moves last_seen (never backwards).
	Touch(ctx context.Context, id core.UUID, lastSeen time.Time) error
	// SetPinned pins or unpins a snapshot (pinned versions survive
	// retention and are GC roots), recording why.
	SetPinned(ctx context.Context, id core.UUID, pinned bool, reason string) error
	// DeltaDependents returns the snapshots whose delta base is one of ids.
	DeltaDependents(ctx context.Context, ids []core.UUID) ([]Snapshot, error)
	// RewriteBlobs replaces a snapshot's blob refs and delta base (a
	// keyframe rewrite before its base is deleted); the plain content is
	// unchanged.
	RewriteBlobs(ctx context.Context, id core.UUID, b BlobRefs) error
	// StaleSnapshots returns up to limit snapshots of program whose
	// DeriveStamp is not stamp, in (ValidFrom, ID) ascending order, after
	// the position after (nil: from the start). Re-derive jobs page with
	// it; the stamps themselves record their progress.
	StaleSnapshots(ctx context.Context, program core.UUID, stamp string, after *Cursor, limit int) ([]Snapshot, error)
	// LockSnapshot returns a snapshot and locks it against concurrent
	// writers until the transaction ends (Postgres: SELECT ... FOR UPDATE):
	// a re-derive and an ingest whose delta base or predecessor it is
	// serialize on it.
	LockSnapshot(ctx context.Context, id core.UUID) (Snapshot, bool, error)
	// UpdateDerived replaces a snapshot's derived columns from s (re-derive):
	// the blob refs and encoding (HeadersRef, BodyManifest,
	// NormalizedManifest, FeaturesRef, Codec, DeltaBase, DeltaDepth), Kind,
	// NormalizerVersion, Coverage, SimHash and DeriveStamp. Identity,
	// validity, lineage and the raw columns are unchanged.
	UpdateDerived(ctx context.Context, s Snapshot) error
	// DeleteSnapshots deletes snapshots with their findings and every
	// diff that touches them (diffs follow snapshots, V2 §6.2); children
	// keep living with parent links cleared.
	DeleteSnapshots(ctx context.Context, ids []core.UUID) error

	// NextDiffSeq allocates the program's next diff sequence number.
	NextDiffSeq(ctx context.Context, program core.UUID) (int64, error)
	// InsertDiff stores a new diff row; GetDiff returns one by id.
	InsertDiff(ctx context.Context, d Diff) error
	GetDiff(ctx context.Context, id core.UUID) (Diff, bool, error)
	// RewriteDiff replaces a stored diff's derived columns from d (Levels,
	// Algorithm, NormalizerVersion, Stats, HunksRef, SemanticRef,
	// StoredSize, IndexDocs), keeping its id, seq, sides and times
	// (re-derive: a recomputed diff is the same diff).
	RewriteDiff(ctx context.Context, d Diff) error
	// DiffInto returns the stored diff whose new side is snapshot to.
	DiffInto(ctx context.Context, to core.UUID) (Diff, bool, error)
	// DiffsAfter lists a program's diffs with Seq > after, by Seq, at
	// most limit.
	DiffsAfter(ctx context.Context, program core.UUID, after int64, limit int) ([]Diff, error)
	// DiffsTouching lists the diffs whose either side is one of ids.
	DiffsTouching(ctx context.Context, ids []core.UUID) ([]Diff, error)

	// InsertFindings stores encrypted secret findings; Findings returns a
	// snapshot's.
	InsertFindings(ctx context.Context, fs []Finding) error
	Findings(ctx context.Context, snapshot core.UUID) ([]Finding, error)

	// Volatility returns the rows of one (url template, profile).
	Volatility(ctx context.Context, program core.UUID, urlTemplate, profile string) ([]VolatilityRow, error)
	// PutVolatility upserts volatility rows by (program, url template,
	// profile, feature path).
	PutVolatility(ctx context.Context, rows []VolatilityRow) error

	// Usage returns the tenant's usage row (zero when none).
	Usage(ctx context.Context) (Usage, error)
	// PutUsage replaces the tenant's usage row.
	PutUsage(ctx context.Context, u Usage) error
	// Limits returns the tenant's quota override, if any.
	Limits(ctx context.Context) (Limits, bool, error)
	// PutLimits sets the tenant's quota override.
	PutLimits(ctx context.Context, l Limits) error

	// IndexState returns a program's text-index progress (zero when
	// none); PutIndexState replaces it.
	IndexState(ctx context.Context, program core.UUID) (IndexState, error)
	PutIndexState(ctx context.Context, s IndexState) error

	// DataKeys returns the tenant's wrapped data keys.
	DataKeys(ctx context.Context) ([]DataKey, error)
	// InsertDataKey stores a new wrapped key; a key with the same
	// (purpose, version) already present is not replaced and the call
	// reports false.
	InsertDataKey(ctx context.Context, k DataKey) (bool, error)

	// AppendOutbox writes an event in the transaction (published after
	// commit); an event id already in the outbox is ignored.
	AppendOutbox(ctx context.Context, e OutboxEvent) error

	// Manifests returns every chunk-store manifest the tenant's rows
	// reference, and those of pinned snapshots (chunk GC roots).
	Manifests(ctx context.Context) (live, pinned []string, err error)
}
