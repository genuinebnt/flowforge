package graphstore

import (
	"context"
	"time"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/csr"
	"github.com/genuinebnt/flowforge/oam"
)

// Limits and defaults (spec 5.9, 11.2).
const (
	DefaultPageSize    = 50
	MaxPageSize        = 500
	MaxDepth           = csr.MaxPointDepth // neighbors and blast radius
	DefaultMaxNodes    = 1000
	MaxMaxNodes        = 10000
	DefaultBlastDepth  = 3
	DefaultSearchLimit = 20
	MaxSearchLimit     = 500
	// MaxPathNodes bounds the entities a path search settles.
	MaxPathNodes = 100_000
	// DefaultFuzzyThreshold is the trigram similarity a fuzzy search hit
	// needs when it is not an exact substring match (pg_trgm's default).
	DefaultFuzzyThreshold = 0.3
	// SourceAPI is the source recorded on tags added through AddTag.
	SourceAPI = "api"
)

// Caller is the verified identity an operation runs for. TenantID comes
// from the authenticated token (spec 11.6 "Authorization"), never from a
// request body; every read and write is filtered by it, and ids that
// belong to another tenant are reported as NOT_FOUND.
type Caller struct {
	TenantID string
}

// Config configures a store implementation.
type Config struct {
	// StaleAfterRuns defaults to oam.DefaultStaleAfterRuns.
	StaleAfterRuns int
	// Scope is the passive scope port (spec 5.7); nil leaves every entity
	// unknown. See PolicyScope.
	Scope oam.ScopeFunc
	// Clock timestamps covering runs, API tags and snapshots. Ingest uses
	// observed_at only. Nil means core.SystemClock.
	Clock core.Clock
	// FuzzyThreshold defaults to DefaultFuzzyThreshold.
	FuzzyThreshold float64
	// MaxPending bounds, per tenant, the observations waiting for entity
	// or edge ids they reference (IngestResult.Pending, G-CORE-03); zero
	// means oam.DefaultMaxPending, negative disables the buffer (such an
	// observation is rejected with oam.ErrUnknownEntity).
	MaxPending int
	// PendingTTL drops a waiting observation whose observed_at is older
	// than now minus PendingTTL (default DefaultPendingTTL). Expiry runs
	// on ingest, per tenant.
	PendingTTL time.Duration
}

// DefaultPendingTTL is the default Config.PendingTTL.
const DefaultPendingTTL = 7 * 24 * time.Hour

// WithDefaults returns c with zero fields replaced by their defaults.
func (c Config) WithDefaults() Config {
	if c.StaleAfterRuns <= 0 {
		c.StaleAfterRuns = oam.DefaultStaleAfterRuns
	}
	if c.Clock == nil {
		c.Clock = &core.SystemClock{}
	}
	if c.MaxPending == 0 {
		c.MaxPending = oam.DefaultMaxPending
	}
	if c.PendingTTL <= 0 {
		c.PendingTTL = DefaultPendingTTL
	}
	if c.FuzzyThreshold <= 0 || c.FuzzyThreshold > 1 {
		c.FuzzyThreshold = DefaultFuzzyThreshold
	}
	return c
}

// Now returns the clock reading truncated to the microsecond precision of
// Postgres timestamps.
func (c Config) Now() time.Time { return StoreTime(c.Clock.Now()) }

// StoreTime normalizes a timestamp to what the store keeps: UTC with
// microsecond precision (Postgres timestamptz). Observations are
// normalized on entry so Mem and Postgres hold identical times.
func StoreTime(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

// Store is the graph service API (GraphService, spec 11.6), plus the
// analyses of spec 5.9 and the snapshot builder of spec 21. Mem and
// graphstore/pg implement it with identical observable behaviour; the
// shared suite in graphstore/storetest pins that.
type Store interface {
	// IngestObservation applies one observation atomically, idempotent per
	// (tenant, observation_id).
	IngestObservation(ctx context.Context, req IngestRequest) (IngestResult, error)
	// GetEntity returns one entity of the caller's tenant.
	GetEntity(ctx context.Context, c Caller, req GetEntityRequest) (Entity, error)
	// FindEntities lists entities by filter, ordered by id, paged.
	FindEntities(ctx context.Context, c Caller, req FindEntitiesRequest) (FindEntitiesResult, error)
	// Neighbors is the bounded breadth-first traversal of spec 5.9.
	Neighbors(ctx context.Context, c Caller, req NeighborsRequest) (Subgraph, error)
	// FindPath is the most-likely path (Dijkstra, cost -ln(confidence)).
	FindPath(ctx context.Context, c Caller, req FindPathRequest) (Path, error)
	// Diff lists change events since a time or between two runs, paged.
	Diff(ctx context.Context, c Caller, req DiffRequest) (DiffResult, error)
	// Search is the trigram search over entity display text.
	Search(ctx context.Context, c Caller, req SearchRequest) (SearchResult, error)
	// ReadChanges returns change events after a seq (one page).
	ReadChanges(ctx context.Context, c Caller, req ChangesRequest) ([]ChangeEvent, error)
	// WatchChanges streams change events after req.AfterSeq to fn until
	// ctx ends or fn fails; resume with the last seq delivered.
	WatchChanges(ctx context.Context, c Caller, req WatchRequest, fn func(ChangeEvent) error) error
	// AddTag attaches a property to an entity or edge.
	AddTag(ctx context.Context, c Caller, req AddTagRequest) (Tag, error)
	// SetStale applies a covering run (spec 5.7 staleness).
	SetStale(ctx context.Context, c Caller, req SetStaleRequest) (SetStaleResult, error)
	// BlastRadius lists entities reachable over ownership and hosting
	// labels, ranked by distance.
	BlastRadius(ctx context.Context, c Caller, req BlastRadiusRequest) ([]Reach, error)
	// Components groups the active graph into weakly connected components.
	Components(ctx context.Context, c Caller, programID string) (Components, error)
	// Export returns a program's whole graph (every state, with tags).
	Export(ctx context.Context, c Caller, programID string) (Export, error)
	// BuildSnapshot builds the CSR snapshot of a program's active graph at
	// a consistent change-event seq and returns that watermark.
	BuildSnapshot(ctx context.Context, c Caller, programID string) (*csr.Graph, uint64, error)
	// WriteSnapshot builds a snapshot, writes the encoded file through
	// blobs and records it in the snapshot catalogue (graph.snapshots).
	WriteSnapshot(ctx context.Context, c Caller, programID string, blobs BlobStore) (SnapshotInfo, error)
	// LatestSnapshot returns the catalogue entry with the highest
	// watermark (NOT_FOUND when there is none).
	LatestSnapshot(ctx context.Context, c Caller, programID string) (SnapshotInfo, error)
	// RecordContentChange appends a content_changed event for an entity
	// (V2 §3.4). It changes no graph state and is idempotent per (entity,
	// snapshot_from, snapshot_to, diff_id): a repeat returns the first
	// event.
	RecordContentChange(ctx context.Context, c Caller, req ContentChange) (ChangeEvent, error)
	// GetEntityAsOf reconstructs an entity right after change event
	// req.Seq by replaying its change events: content, scope status,
	// state and (with IncludeTags) non-source tags. Confidence, times,
	// runs and SourceProperty tags are not reconstructed and are zero.
	// NOT_FOUND when the entity did not exist yet.
	GetEntityAsOf(ctx context.Context, c Caller, req GetEntityAsOfRequest) (Entity, error)
	// NeighborsAsOf runs Neighbors on the graph as of req.Seq (entity and
	// edge existence and state replayed from change events; entities
	// carry their content and scope status as of Seq, edges no content).
	NeighborsAsOf(ctx context.Context, c Caller, req NeighborsAsOfRequest) (Subgraph, error)
	// Commonness counts the distinct sources and holder hosts of an
	// entity (V2 §3.4).
	Commonness(ctx context.Context, c Caller, req CommonnessRequest) (Commonness, error)
}
