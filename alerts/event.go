package alerts

import (
	"slices"
	"sort"
	"strings"
	"time"
)

// Event is the typed input to rule evaluation: the Pub/Sub envelope of
// spec 11.7 plus the optional sections a rule can address. Services build
// it from an EventEnvelope: graph.changes fill Change and Entity (the
// denormalized snapshot carried by ChangeEvent, spec 11.6) or Edge,
// run.events fill Run, approval.events fill Approval. scope.* and auth.*
// events carry only the envelope fields.
type Event struct {
	ID            string    `json:"event_id"`
	Type          string    `json:"type"`
	SchemaVersion int64     `json:"schema_version,omitempty"`
	TenantID      string    `json:"tenant_id"`
	ProgramID     string    `json:"program_id,omitempty"`
	OccurredAt    time.Time `json:"occurred_at"`

	Change   *Change   `json:"change,omitempty"`
	Entity   *Entity   `json:"entity,omitempty"`
	Edge     *Edge     `json:"edge,omitempty"`
	Run      *Run      `json:"run,omitempty"`
	Approval *Approval `json:"approval,omitempty"`
	// Diff is set for content changes (content.changes events, V2 §8).
	Diff *Diff `json:"diff,omitempty"`
}

// Change mirrors ChangeEvent (spec 11.6) without the embedded entity.
type Change struct {
	ID       string         `json:"id,omitempty"`
	Seq      int64          `json:"seq,omitempty"`
	Type     string         `json:"type"`
	EntityID string         `json:"entity_id,omitempty"`
	EdgeID   string         `json:"edge_id,omitempty"`
	RunID    string         `json:"run_id,omitempty"`
	Patch    map[string]any `json:"patch,omitempty"`
	At       time.Time      `json:"at"`
}

// Entity is the entity snapshot. Content holds the OAM asset content;
// rules address its fields flattened (entity.name, entity.address, ...).
type Entity struct {
	ID           string         `json:"id"`
	AssetType    string         `json:"asset_type"`
	Key          string         `json:"key,omitempty"`
	Content      map[string]any `json:"content,omitempty"`
	ScopeStatus  string         `json:"scope_status,omitempty"`
	Confidence   float64        `json:"confidence,omitempty"`
	State        string         `json:"state,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	LastSeen     time.Time      `json:"last_seen"`
	FirstSeenRun string         `json:"first_seen_run,omitempty"`
	LastSeenRun  string         `json:"last_seen_run,omitempty"`
}

// Edge is the edge snapshot of an edge_added / edge_removed change.
type Edge struct {
	ID            string    `json:"id"`
	RelationType  string    `json:"relation_type,omitempty"`
	Label         string    `json:"label,omitempty"`
	Discriminator string    `json:"discriminator,omitempty"`
	FromEntityID  string    `json:"from_entity_id,omitempty"`
	ToEntityID    string    `json:"to_entity_id,omitempty"`
	Confidence    float64   `json:"confidence,omitempty"`
	State         string    `json:"state,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	LastSeen      time.Time `json:"last_seen"`
}

// Run carries a run event (RunEventPayload, spec 11.7) plus the run
// attributes alerting needs. Code and Message are lifted by the producer
// from the event payload (run_failed: code, message; effect_failed:
// error.code, error.message); Path and Resource likewise (effect path,
// budget_exceeded resource).
type Run struct {
	ID              string         `json:"id"`
	Seq             int64          `json:"seq,omitempty"`
	Type            string         `json:"type,omitempty"`
	WorkflowName    string         `json:"workflow_name,omitempty"`
	WorkflowVersion int64          `json:"workflow_version,omitempty"`
	Status          string         `json:"status,omitempty"`
	Code            string         `json:"code,omitempty"`
	Message         string         `json:"message,omitempty"`
	Path            string         `json:"path,omitempty"`
	Resource        string         `json:"resource,omitempty"`
	Payload         map[string]any `json:"payload,omitempty"`
}

// Approval mirrors ApprovalEventPayload (spec 11.7).
type Approval struct {
	ID         string    `json:"id"`
	RunID      string    `json:"run_id,omitempty"`
	Role       string    `json:"role,omitempty"`
	Status     string    `json:"status,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	EffectPath string    `json:"effect_path,omitempty"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// Diff is a content change (V2_DESIGN §8, §12): the classification the
// content service stored with a captured version, and the excerpt an
// alert shows. Producers fill it from a content.changes event; every text
// is already redacted (V2 §12.11), so an alert never carries a secret.
type Diff struct {
	SnapshotID     string `json:"snapshot_id,omitempty"`
	FromSnapshotID string `json:"from_snapshot_id,omitempty"`
	DiffID         string `json:"diff_id,omitempty"`
	URL            string `json:"url,omitempty"`
	Profile        string `json:"profile,omitempty"`
	// Outcome is no_change, masked_only, reordered, structural, changed or
	// degraded; Levels are hash, text and semantic (level 3).
	Outcome string   `json:"outcome,omitempty"`
	Levels  []string `json:"levels,omitempty"`
	// Class, Newness and Cause classify the change (V2 §12.2, §12.13).
	Class    string `json:"class,omitempty"`
	Newness  string `json:"newness,omitempty"`
	Cause    string `json:"cause,omitempty"`
	Baseline bool   `json:"baseline,omitempty"`
	// Alertable is the content service's default-feed verdict: a change
	// that is not a baseline, of cause target and not pre-existing.
	Alertable bool `json:"alertable,omitempty"`
	// Changes are the level-3 changes, "<op> <kind>|<key>".
	Changes []string `json:"changes,omitempty"`
	// Excerpt is a unified-diff excerpt of the changed text (redacted,
	// bounded by the producer).
	Excerpt string `json:"excerpt,omitempty"`
	// Score and ScoreFeatures rank the change (V2 §12.4). They are carried
	// to the payload so receivers can order alerts, and are deliberately
	// not addressable by rules: scores rank, they never decide sending.
	Score         float64        `json:"score,omitempty"`
	ScoreFeatures []ScoreFeature `json:"score_features,omitempty"`
}

// ScoreFeature is one named reason of a score.
type ScoreFeature struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
}

// Kinds returns the "<kind>_<op>" names of d's changes (endpoint_added,
// header_removed, ...), sorted and unique: the change kinds of the
// content search index (V2 §7).
func (d *Diff) Kinds() []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range d.Changes {
		op, rest, ok := strings.Cut(c, " ")
		kind, _, ok2 := strings.Cut(rest, "|")
		if !ok || !ok2 {
			continue
		}
		k := kind + "_" + op
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Semantic reports a level-3 change.
func (d *Diff) Semantic() bool { return slices.Contains(d.Levels, "semantic") }
