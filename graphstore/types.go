package graphstore

import (
	"time"

	"github.com/genuinebnt/flowforge/oam"
)

// TargetKind names what a tag is attached to.
type TargetKind string

// Tag targets.
const (
	TargetEntity TargetKind = "entity"
	TargetEdge   TargetKind = "edge"
)

// Direction selects which edges a traversal follows (proto Direction).
type Direction int

// Directions. DirectionUnspecified is rejected.
const (
	DirectionUnspecified Direction = iota
	DirectionOut                   // from -> to ("from has label to")
	DirectionIn                    // to -> from
	DirectionBoth
)

// String returns the lower-case name used by the REST API.
func (d Direction) String() string {
	switch d {
	case DirectionOut:
		return "out"
	case DirectionIn:
		return "in"
	case DirectionBoth:
		return "both"
	}
	return "unspecified"
}

// Entity is one graph node (proto Entity, spec 5.3).
type Entity struct {
	ID           string          `json:"id"`
	TenantID     string          `json:"tenant_id"`
	ProgramID    string          `json:"program_id"`
	AssetType    string          `json:"asset_type"`
	Key          string          `json:"key"`
	Content      map[string]any  `json:"content"`
	ScopeStatus  oam.ScopeStatus `json:"scope_status"`
	Confidence   float64         `json:"confidence"`
	State        oam.State       `json:"state"`
	CreatedAt    time.Time       `json:"created_at"`
	LastSeen     time.Time       `json:"last_seen"`
	FirstSeenRun string          `json:"first_seen_run,omitempty"`
	LastSeenRun  string          `json:"last_seen_run,omitempty"`
	// MissedRuns and Tags are filled only where tags are requested
	// (GetEntity with IncludeTags, Export).
	MissedRuns map[string]int `json:"missed_runs,omitempty"`
	Tags       []Tag          `json:"tags,omitempty"`
}

// Edge is one directed relation (proto Edge, spec 5.3).
type Edge struct {
	ID            string         `json:"id"`
	TenantID      string         `json:"tenant_id"`
	ProgramID     string         `json:"program_id"`
	RelationType  string         `json:"relation_type"`
	Label         string         `json:"label"`
	Discriminator string         `json:"discriminator"`
	Content       map[string]any `json:"content"`
	FromEntityID  string         `json:"from_entity_id"`
	ToEntityID    string         `json:"to_entity_id"`
	Confidence    float64        `json:"confidence"`
	State         oam.State      `json:"state"`
	CreatedAt     time.Time      `json:"created_at"`
	LastSeen      time.Time      `json:"last_seen"`
	// MissedRuns and Tags are filled only by Export.
	MissedRuns map[string]int `json:"missed_runs,omitempty"`
	Tags       []Tag          `json:"tags,omitempty"`
}

// Tag is a property attached to an entity or an edge (proto Tag). One
// SourceProperty tag per source carries provenance and confidence.
type Tag struct {
	ID           string         `json:"id"`
	TargetKind   TargetKind     `json:"target_kind"`
	TargetID     string         `json:"target_id"`
	PropertyType string         `json:"property_type"`
	Name         string         `json:"name"`
	Content      map[string]any `json:"content"`
	CreatedAt    time.Time      `json:"created_at"`
	LastSeen     time.Time      `json:"last_seen"`
}

// ChangeEvent is one effective graph change (proto ChangeEvent). Entity
// is a denormalized snapshot (without tags) of the entity the event is
// about, for alert evaluation: the state right after the change in an
// IngestResult or SetStaleResult, the current state when read later.
type ChangeEvent struct {
	ID        string         `json:"id"`
	Seq       int64          `json:"seq"`
	TenantID  string         `json:"tenant_id"`
	ProgramID string         `json:"program_id"`
	Type      oam.ChangeType `json:"type"`
	EntityID  string         `json:"entity_id,omitempty"`
	EdgeID    string         `json:"edge_id,omitempty"`
	RunID     string         `json:"run_id,omitempty"`
	Patch     []oam.PatchOp  `json:"patch,omitempty"`
	At        time.Time      `json:"at"`
	Entity    *Entity        `json:"entity,omitempty"`
	// TagType and TagName name the tag of a tag_changed event.
	TagType string `json:"tag_type,omitempty"`
	TagName string `json:"tag_name,omitempty"`
	// Base is the record's state at creation (entity_added, and the
	// edge_added that created an edge); AsOf reads replay from it.
	Base map[string]any `json:"base,omitempty"`
	// ContentChange is the patch of a content_changed event.
	ContentChange *oam.ContentChangePatch `json:"content_change,omitempty"`
	// Cause ("" = target, "parser", "normalizer") and Baseline come from
	// the observation or content change that produced the event.
	Cause    string `json:"cause,omitempty"`
	Baseline bool   `json:"baseline,omitempty"`
}

// ContentChange is the RecordContentChange request (V2 §3.4): the
// content service reports that two snapshots of an entity differ. At
// zero means the store clock.
type ContentChange struct {
	EntityID string
	RunID    string
	At       time.Time
	Patch    oam.ContentChangePatch
}

// GetEntityAsOfRequest reads an entity as it was right after the change
// event with seq Seq of its program (Seq >= 1).
type GetEntityAsOfRequest struct {
	EntityID    string
	Seq         int64
	IncludeTags bool
}

// NeighborsAsOfRequest is a NeighborsRequest evaluated on the graph as
// it was right after change event Seq. Since must be zero (last_seen is
// not reconstructed).
type NeighborsAsOfRequest struct {
	NeighborsRequest
	Seq int64
}

// CommonnessRequest asks how widely an entity is shared.
type CommonnessRequest struct {
	EntityID string
}

// CommonThreshold is the holder count above which a node is common (V2
// §3.4): pivots and leads ignore it, and sharing it never implies
// ownership.
const CommonThreshold = 50

// Commonness reports how many distinct sources (entities) and holders
// (hosts of those entities) point at an entity over active edges, and
// whether it is common (Holders > CommonThreshold).
type Commonness struct {
	EntityID string `json:"entity_id"`
	Sources  int    `json:"sources"`
	Holders  int    `json:"holders"`
	Common   bool   `json:"common"`
}

// IngestRequest carries one observation (proto IngestRequest). TenantID
// comes from the verified caller identity, never from the observation
// body. ProgramID, RunID and ToolName default to the observation's own
// fields; when both are set they must agree.
type IngestRequest struct {
	TenantID    string
	ProgramID   string
	RunID       string
	EffectPath  string
	ToolName    string
	ToolVersion string
	Observation oam.Observation
}

// IngestResult reports the effect of one observation (proto
// IngestResponse). A Duplicate observation id had no effect.
type IngestResult struct {
	Duplicate       bool
	EntitiesCreated int
	EntitiesUpdated int
	EdgesCreated    int
	EdgesUpdated    int
	Changes         []ChangeEvent
	RefToEntityID   map[string]string
}

// GetEntityRequest is proto GetEntityRequest.
type GetEntityRequest struct {
	EntityID    string
	IncludeTags bool
}

// PageRequest is proto PageRequest: PageSize 0 means DefaultPageSize; the
// token is opaque.
type PageRequest struct {
	PageSize  int
	PageToken string
}

// FindEntitiesRequest is proto FindEntitiesRequest. Zero fields do not
// filter. Results are ordered by entity id.
type FindEntitiesRequest struct {
	ProgramID   string
	AssetTypes  []string
	ScopeStatus oam.ScopeStatus
	State       oam.State
	// SeenAfter keeps entities whose last_seen is at or after it.
	SeenAfter time.Time
	KeyPrefix string
	Page      PageRequest
}

// FindEntitiesResult is proto FindEntitiesResponse (entities without tags).
type FindEntitiesResult struct {
	Entities      []Entity
	NextPageToken string
}

// NeighborsRequest is proto NeighborsRequest. Depth 0 means 1; MaxNodes 0
// means DefaultMaxNodes. Since keeps edges and neighbours whose last_seen
// is at or after it.
type NeighborsRequest struct {
	EntityID  string
	Labels    []string
	Direction Direction
	Depth     int
	Since     time.Time
	MaxNodes  int
}

// Subgraph is proto Subgraph: entities ordered by (distance, id), edges by
// id, each at most once. Truncated reports that MaxNodes cut the result.
type Subgraph struct {
	Entities  []Entity
	Edges     []Edge
	Truncated bool
}

// FindPathRequest is proto FindPathRequest. MaxCost 0 means unbounded.
type FindPathRequest struct {
	FromEntityID string
	ToEntityID   string
	MaxCost      float64
	Labels       []string
}

// Path is proto PathResponse. An empty EntityIDs means no path within
// MaxCost.
type Path struct {
	EntityIDs          []string
	Edges              []Edge
	TotalCost          float64
	CombinedConfidence float64
}

// DiffRequest is proto DiffRequest: either Since or RunIDA is set. With
// RunIDA the diff covers the change events after run A's last change up
// to and including run B's last change (or the newest event when RunIDB is
// empty).
type DiffRequest struct {
	ProgramID string
	Since     time.Time
	RunIDA    string
	RunIDB    string
	Page      PageRequest
}

// DiffResult is proto DiffResponse (events in seq order).
type DiffResult struct {
	Changes       []ChangeEvent
	NextPageToken string
}

// SearchRequest is proto SearchRequest. Limit 0 means DefaultSearchLimit.
type SearchRequest struct {
	ProgramID  string
	Query      string
	AssetTypes []string
	Limit      int
	Fuzzy      bool
}

// SearchHit is proto SearchHit.
type SearchHit struct {
	Entity          Entity
	Score           float64
	MatchedTrigrams []string
}

// SearchResult is proto SearchResponse.
type SearchResult struct {
	Hits []SearchHit
}

// ChangesRequest reads one page of change events after AfterSeq (the
// pull form of WatchChanges). Limit 0 means DefaultPageSize.
type ChangesRequest struct {
	ProgramID string
	AfterSeq  int64
	Types     []oam.ChangeType
	Limit     int
}

// WatchRequest is proto WatchChangesRequest.
type WatchRequest struct {
	ProgramID string
	AfterSeq  int64
	Types     []oam.ChangeType
}

// AddTagRequest is proto AddTagRequest. Content holds the property fields
// other than "type"; the property's name field may be given in Name
// instead. SourceProperty tags are rejected: provenance comes from ingest.
type AddTagRequest struct {
	TargetKind   TargetKind
	TargetID     string
	PropertyType string
	Name         string
	Content      map[string]any
}

// SetStaleRequest is proto SetStaleRequest, sent after a covering run.
// StaleAfterRuns 0 means the store default.
type SetStaleRequest struct {
	ProgramID      string
	RunID          string
	ToolName       string
	AssetTypes     []string
	StaleAfterRuns int
}

// SetStaleResult is proto SetStaleResponse plus the change events.
type SetStaleResult struct {
	Duplicate      bool
	EntitiesMarked int
	EdgesMarked    int
	Changes        []ChangeEvent
}

// BlastRadiusRequest asks for the entities reachable from EntityID over
// the ownership and hosting labels (spec 5.9). Depth 0 means
// DefaultBlastDepth.
type BlastRadiusRequest struct {
	EntityID string
	Depth    int
}

// Reach is an entity found by BlastRadius with its hop distance.
type Reach struct {
	Entity   Entity
	Distance int
}

// Components is the weakly connected components of a program's active
// graph, computed on a snapshot at WatermarkSeq. Each group is sorted by
// entity id; groups are ordered by their smallest member.
type Components struct {
	WatermarkSeq uint64
	Groups       [][]string
}

// Export is a program's whole graph, every state included, sorted by id,
// with tags. Its canonical JSON is what convergence tests compare.
type Export struct {
	Entities []Entity `json:"entities"`
	Edges    []Edge   `json:"edges"`
}
