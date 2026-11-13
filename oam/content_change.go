package oam

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Content-change levels (V2 §5), in canonical order.
const (
	LevelHash     = "hash"
	LevelText     = "text"
	LevelSemantic = "semantic"
)

// Content-change classes (V2 §12).
const (
	ClassStructural       = "structural"
	ClassMetadata         = "metadata"
	ClassSemanticEndpoint = "semantic_endpoint"
	ClassObservation      = "observation"
	ClassCatalog          = "catalog"
)

// MaxScoreFeatures bounds ContentChangePatch.ScoreFeatures and
// MaxSummaryKeys bounds ContentChangePatch.Summary.
const (
	MaxScoreFeatures = 64
	MaxSummaryKeys   = 64
)

// ScoreFeature is one reason behind a change's score.
type ScoreFeature struct {
	Name   string  `json:"name"`
	Weight float64 `json:"weight"`
}

// ContentChangePatch is the patch of a content_changed event (V2 §3.4,
// §12): which snapshots were compared, the diff, the levels that differ,
// summary counts, and the content service's classification.
type ContentChangePatch struct {
	SnapshotFrom  string         `json:"snapshot_from,omitempty"`
	SnapshotTo    string         `json:"snapshot_to"`
	DiffID        string         `json:"diff_id,omitempty"`
	Levels        []string       `json:"levels"`
	Summary       map[string]int `json:"summary,omitempty"`
	Cause         string         `json:"cause"`
	Baseline      bool           `json:"baseline,omitempty"`
	Class         string         `json:"class,omitempty"`
	Score         float64        `json:"score"`
	ScoreFeatures []ScoreFeature `json:"score_features,omitempty"`
}

// Normalize validates p and returns it with levels in canonical order
// (hash, text, semantic; duplicates removed), cause defaulted to target
// and score features sorted by name.
//
// Rules: snapshot_to required; snapshot ids and diff_id at most 256
// printable bytes and snapshot_from != snapshot_to; at least one level;
// summary keys lower-case identifiers (at most MaxSummaryKeys) with
// non-negative counts; cause target, parser or normalizer; class one of
// the Class constants or empty; score finite in [0, 1]; at most
// MaxScoreFeatures features with unique lower-case identifier names and
// finite weights in [-1, 1].
func (p ContentChangePatch) Normalize() (ContentChangePatch, error) {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("%w: content change: "+format, append([]any{ErrInvalidObservation}, args...)...)
	}
	ref := func(name, v string, required bool) error {
		if v == "" && !required {
			return nil
		}
		if strings.TrimSpace(v) == "" || len(v) > 256 || hasControl(v) {
			return bad("%s must be 1-256 printable bytes", name)
		}
		return nil
	}
	if err := ref("snapshot_to", p.SnapshotTo, true); err != nil {
		return p, err
	}
	if err := ref("snapshot_from", p.SnapshotFrom, false); err != nil {
		return p, err
	}
	if err := ref("diff_id", p.DiffID, false); err != nil {
		return p, err
	}
	if p.SnapshotFrom == p.SnapshotTo {
		return p, bad("snapshot_from equals snapshot_to")
	}
	order := []string{LevelHash, LevelText, LevelSemantic}
	var levels []string
	for _, l := range order {
		if slices.Contains(p.Levels, l) {
			levels = append(levels, l)
		}
	}
	for _, l := range p.Levels {
		if !slices.Contains(order, l) {
			return p, bad("unknown level %q", l)
		}
	}
	if len(levels) == 0 {
		return p, bad("at least one level is required")
	}
	p.Levels = levels
	if len(p.Summary) > MaxSummaryKeys {
		return p, bad("more than %d summary keys", MaxSummaryKeys)
	}
	// PORT: map order is random, so with several bad keys the one the error
	// names varies between runs; whether the patch is valid does not.
	for k, v := range p.Summary {
		if !isLowerIdent(k, true) || len(k) > 64 || v < 0 {
			return p, bad("summary %q=%d: keys are lower-case identifiers, counts non-negative", k, v)
		}
	}
	if p.Cause == "" {
		p.Cause = CauseTarget
	}
	if !ValidCause(p.Cause) {
		return p, bad("unknown cause %q", p.Cause)
	}
	switch p.Class {
	case "", ClassStructural, ClassMetadata, ClassSemanticEndpoint, ClassObservation, ClassCatalog:
	default:
		return p, bad("unknown class %q", p.Class)
	}
	if math.IsNaN(p.Score) || p.Score < 0 || p.Score > 1 {
		return p, bad("score %v outside [0, 1]", p.Score)
	}
	if len(p.ScoreFeatures) > MaxScoreFeatures {
		return p, bad("more than %d score features", MaxScoreFeatures)
	}
	feats := slices.Clone(p.ScoreFeatures)
	slices.SortFunc(feats, func(a, b ScoreFeature) int { return cmp.Compare(a.Name, b.Name) })
	for i, f := range feats {
		if !isLowerIdent(f.Name, true) || len(f.Name) > 64 {
			return p, bad("score feature name %q is not a lower-case identifier", f.Name)
		}
		if i > 0 && feats[i-1].Name == f.Name {
			return p, bad("duplicate score feature %q", f.Name)
		}
		if math.IsNaN(f.Weight) || math.IsInf(f.Weight, 0) || f.Weight < -1 || f.Weight > 1 {
			return p, bad("score feature %q weight %v outside [-1, 1]", f.Name, f.Weight)
		}
	}
	if len(feats) == 0 {
		feats = nil
	}
	p.ScoreFeatures = feats
	return p, nil
}

// ContentChange asks the graph to record a content_changed event for an
// entity (graphstore RecordContentChange).
type ContentChange struct {
	TenantID, ProgramID string
	EntityID            string
	RunID               string
	At                  time.Time
	Patch               ContentChangePatch
}

// ContentChangeUUID is the id of the content_changed event of (entity,
// snapshot_from, snapshot_to, diff_id): recording the same change twice
// is idempotent.
func ContentChangeUUID(tenantID, programID, entityID string, p ContentChangePatch) core.UUID {
	return derivedUUID("ff-oam-content-change", tenantID, programID, entityID, p.SnapshotFrom, p.SnapshotTo, p.DiffID)
}

// RecordContentChange appends a content_changed event for an existing
// entity. It changes no graph state. A second call for the same change
// (ContentChangeUUID) returns the first event and duplicate = true.
func (g *Graph) RecordContentChange(cc ContentChange) (ChangeEvent, bool, error) {
	p, err := cc.Patch.Normalize()
	if err != nil {
		return ChangeEvent{}, false, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	e, ok := g.entities[cc.EntityID]
	if !ok || e.tenant != cc.TenantID || e.program != cc.ProgramID {
		return ChangeEvent{}, false, fmt.Errorf("%w: %s", ErrUnknownEntity, cc.EntityID)
	}
	id := core.PublicID(core.PrefixChange, ContentChangeUUID(cc.TenantID, cc.ProgramID, cc.EntityID, p))
	if i, ok := g.contentIdx[id]; ok {
		return g.changes[i], true, nil
	}
	ev := ChangeEvent{
		ID: id, Seq: int64(len(g.changes) + 1), TenantID: cc.TenantID, ProgramID: cc.ProgramID,
		Type: ChangeContentChanged, EntityID: cc.EntityID, RunID: cc.RunID, At: cc.At.UTC(),
		ContentChange: &p, Cause: p.Cause, Baseline: p.Baseline,
	}
	if ev.Cause == CauseTarget {
		ev.Cause = ""
	}
	g.contentIdx[id] = len(g.changes)
	g.changes = append(g.changes, ev)
	return ev, false, nil
}
