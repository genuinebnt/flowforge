package alerts

import "time"

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
