package oam

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// MaxBatchEntities is the largest number of entities one observation may
// carry (spec 6.3).
const MaxBatchEntities = 500

// Tool identifies the adapter that produced an observation.
type Tool struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// Observation is the envelope a tool adapter emits (spec 6.1). Tools never
// write to the graph; the engine forwards observations to graph ingest.
type Observation struct {
	ObservationID string           `json:"observation_id"`
	TenantID      string           `json:"tenant_id,omitempty"`
	ProgramID     string           `json:"program_id,omitempty"`
	RunID         string           `json:"run_id,omitempty"`
	Step          string           `json:"step,omitempty"`
	EffectSeq     int64            `json:"effect_seq,omitempty"`
	Tool          Tool             `json:"tool"`
	ObservedAt    time.Time        `json:"observed_at"`
	Confidence    float64          `json:"confidence"`
	Entities      []ObservedEntity `json:"entities,omitempty"`
	Edges         []ObservedEdge   `json:"edges,omitempty"`
	Tags          []ObservedTag    `json:"tags,omitempty"`
	RawRef        string           `json:"raw_ref,omitempty"`
	// Authority (ff, V2 §3.4) makes the observation the authoritative
	// reading of one source entity under one capture profile.
	Authority *Authority `json:"authority,omitempty"`
	// Cause (ff) says why the facts changed: "target" (default), or
	// "parser" / "normalizer" for a re-derivation after an upgrade. It is
	// copied onto every change event the observation produces.
	Cause string `json:"cause,omitempty"`
	// Baseline (ff) marks the first complete capture of a (URL, profile);
	// copied onto every change event so feeds can hide it.
	Baseline bool `json:"baseline,omitempty"`
}

// Coverage is how completely an authoritative observation read its
// source (V2 §3.4).
type Coverage string

// Coverage values. Only CoverageComplete withdraws anything.
const (
	CoverageComplete    Coverage = "complete"
	CoveragePartial     Coverage = "partial"
	CoverageFailed      Coverage = "failed"
	CoverageUnsupported Coverage = "unsupported"
)

// Causes of a change (V2 §12 item 2).
const (
	CauseTarget     = "target"
	CauseParser     = "parser"
	CauseNormalizer = "normalizer"
)

// Authority declares an observation the authoritative reading of Source
// (a local ref or an ent_ id) under capture Profile.
//
// Every edge leaving Source and every tag on Source in the observation is
// authoritative: it is reported to the (Source, Profile, family) unit of
// its rule family (FamilyOfLabel, FamilyOfProperty) instead of the
// covering-run bookkeeping. When Coverage is complete, the units of the
// families in CoveredRules are replaced by this observation's reading:
// their members that it does not report are withdrawn (edge_removed, or
// a tag_changed removal). Every other unit, and every unit of a partial,
// failed or unsupported observation, only gains members. See
// OPEN_QUESTIONS.md for the exact rules.
type Authority struct {
	Source       string   `json:"source"`
	Profile      string   `json:"profile"`
	Coverage     Coverage `json:"coverage"`
	CoveredRules []string `json:"covered_rules,omitempty"`
}

// ObservedEntity is one asset in an envelope, named by a ref that is
// local to the envelope.
type ObservedEntity struct {
	Ref   string `json:"ref"`
	Asset Asset  `json:"asset"`
}

// ObservedEdge links two entities. From and To are each a local ref or
// the id of an existing entity (ent_...).
type ObservedEdge struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Relation Relation `json:"relation"`
}

// ObservedTag attaches a property to a local ref, an existing entity id
// (ent_...) or an existing edge id (edg_...).
type ObservedTag struct {
	Target   string   `json:"target"`
	Property Property `json:"property"`
}

// wire forms: polymorphic members stay raw until the mode is known.
type (
	wireObservation struct {
		ObservationID string       `json:"observation_id"`
		TenantID      string       `json:"tenant_id"`
		ProgramID     string       `json:"program_id"`
		RunID         string       `json:"run_id"`
		Step          string       `json:"step"`
		EffectSeq     int64        `json:"effect_seq"`
		Tool          Tool         `json:"tool"`
		ObservedAt    time.Time    `json:"observed_at"`
		Confidence    float64      `json:"confidence"`
		Entities      []wireEntity `json:"entities"`
		Edges         []wireEdge   `json:"edges"`
		Tags          []wireTag    `json:"tags"`
		RawRef        string       `json:"raw_ref"`
		Authority     *Authority   `json:"authority"`
		Cause         string       `json:"cause"`
		Baseline      bool         `json:"baseline"`
	}
	wireEntity struct {
		Ref   string          `json:"ref"`
		Asset json.RawMessage `json:"asset"`
	}
	wireEdge struct {
		From     string          `json:"from"`
		To       string          `json:"to"`
		Relation json.RawMessage `json:"relation"`
	}
	wireTag struct {
		Target   string          `json:"target"`
		Property json.RawMessage `json:"property"`
	}
)

// DecodeObservation decodes an envelope. Strict (requests, tool output)
// rejects unknown or duplicate keys at every level; Lenient (stored data)
// ignores unknown keys. Decoding checks structure only; call Validate for
// the semantic rules.
func DecodeObservation(data []byte, mode DecodeMode) (Observation, error) {
	var w wireObservation
	if err := decodeJSON(data, &w, mode); err != nil {
		return Observation{}, err
	}
	o := Observation{
		ObservationID: w.ObservationID, TenantID: w.TenantID, ProgramID: w.ProgramID,
		RunID: w.RunID, Step: w.Step, EffectSeq: w.EffectSeq, Tool: w.Tool,
		ObservedAt: w.ObservedAt.UTC(), Confidence: w.Confidence, RawRef: w.RawRef,
		Authority: w.Authority, Cause: w.Cause, Baseline: w.Baseline,
	}
	if y := o.ObservedAt.Year(); y < 0 || y > 9999 {
		return Observation{}, fmt.Errorf("%w: observed_at year %d outside 0..9999", ErrMalformed, y)
	}
	for i, e := range w.Entities {
		oe, err := e.decode(mode)
		if err != nil {
			return Observation{}, fmt.Errorf("entities[%d]: %w", i, err)
		}
		o.Entities = append(o.Entities, oe)
	}
	for i, e := range w.Edges {
		oe, err := e.decode(mode)
		if err != nil {
			return Observation{}, fmt.Errorf("edges[%d]: %w", i, err)
		}
		o.Edges = append(o.Edges, oe)
	}
	for i, t := range w.Tags {
		ot, err := t.decode(mode)
		if err != nil {
			return Observation{}, fmt.Errorf("tags[%d]: %w", i, err)
		}
		o.Tags = append(o.Tags, ot)
	}
	return o, nil
}

func (w wireEntity) decode(mode DecodeMode) (ObservedEntity, error) {
	a, err := DecodeAsset(w.Asset, mode)
	if err != nil {
		return ObservedEntity{}, fmt.Errorf("asset: %w", err)
	}
	return ObservedEntity{Ref: w.Ref, Asset: a}, nil
}

func (w wireEdge) decode(mode DecodeMode) (ObservedEdge, error) {
	r, err := DecodeRelation(w.Relation, mode)
	if err != nil {
		return ObservedEdge{}, fmt.Errorf("relation: %w", err)
	}
	return ObservedEdge{From: w.From, To: w.To, Relation: r}, nil
}

func (w wireTag) decode(mode DecodeMode) (ObservedTag, error) {
	p, err := DecodeProperty(w.Property, mode)
	if err != nil {
		return ObservedTag{}, fmt.Errorf("property: %w", err)
	}
	return ObservedTag{Target: w.Target, Property: p}, nil
}

// EncodeObservation encodes an envelope. Decoding the result with either
// mode yields an equal envelope.
//
// PORT: observed_at goes through time.Time's MarshalJSON, which is RFC
// 3339 with up to nanosecond precision and trailing fractional zeros
// trimmed ("2026-11-12T13:25:11.04Z"), not the millisecond form of
// core.FormatTime. json.Marshal also escapes <, > and & as \u003c, \u003e
// and \u0026 here; the bytes are stored, not hashed, so only the decoded
// value must match.
func EncodeObservation(o Observation) ([]byte, error) {
	o.ObservedAt = o.ObservedAt.UTC()
	b, err := json.Marshal(o)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidObservation, err)
	}
	return b, nil
}

// UnmarshalJSON decodes stored data leniently. Requests must use
// DecodeObservation with Strict instead.
func (o *Observation) UnmarshalJSON(b []byte) error {
	v, err := DecodeObservation(b, Lenient)
	if err != nil {
		return err
	}
	*o = v
	return nil
}

// UnmarshalJSON decodes leniently (see Observation.UnmarshalJSON).
func (e *ObservedEntity) UnmarshalJSON(b []byte) error {
	var w wireEntity
	if err := decodeJSON(b, &w, Lenient); err != nil {
		return err
	}
	v, err := w.decode(Lenient)
	*e = v
	return err
}

// UnmarshalJSON decodes leniently (see Observation.UnmarshalJSON).
func (e *ObservedEdge) UnmarshalJSON(b []byte) error {
	var w wireEdge
	if err := decodeJSON(b, &w, Lenient); err != nil {
		return err
	}
	v, err := w.decode(Lenient)
	*e = v
	return err
}

// UnmarshalJSON decodes leniently (see Observation.UnmarshalJSON).
func (t *ObservedTag) UnmarshalJSON(b []byte) error {
	var w wireTag
	if err := decodeJSON(b, &w, Lenient); err != nil {
		return err
	}
	v, err := w.decode(Lenient)
	*t = v
	return err
}

// Validate checks the envelope rules of spec 6.1 and 6.3 without a graph:
// required fields, confidence in [0, 1], at most MaxBatchEntities
// entities, unique refs, valid assets (each has a canonical key),
// relations and properties, endpoints that are local refs or entity ids,
// and label endpoint rules wherever both endpoint types are known. Errors
// are *ValidationError values.
func (o Observation) Validate() error {
	_, err := o.prepare()
	return err
}

// prepared is a validated, normalized observation.
type prepared struct {
	stamp    Stamp
	percent  int
	entities []preparedEntity
	edges    []preparedEdge
	tags     []preparedTag
	auth     *preparedAuth
	cause    string
	baseline bool
}

type preparedAuth struct {
	source   endpoint
	profile  string
	complete bool
	covered  []string // sorted, unique
}

// PreparedAuthority is a validated Authority: canonical profile, coverage
// and sorted, de-duplicated covered rule families.
type PreparedAuthority struct {
	Profile  string
	Complete bool
	Covered  []string
}

// PrepareAuthority validates an Authority's fields other than Source.
func PrepareAuthority(a Authority) (PreparedAuthority, error) {
	if !ValidProfile(a.Profile) {
		return PreparedAuthority{}, invalidAt("authority.profile", fmt.Errorf("bad profile %q", a.Profile))
	}
	switch a.Coverage {
	case CoverageComplete, CoveragePartial, CoverageFailed, CoverageUnsupported:
	default:
		return PreparedAuthority{}, invalidAt("authority.coverage", fmt.Errorf("coverage must be complete, partial, failed or unsupported, got %q", a.Coverage))
	}
	var covered []string
	for i, r := range a.CoveredRules {
		n, err := normalizeLabel(r)
		if err != nil {
			return PreparedAuthority{}, invalidAt(fmt.Sprintf("authority.covered_rules[%d]", i), fmt.Errorf("bad rule family %q", r))
		}
		covered = append(covered, n)
	}
	slices.Sort(covered)
	return PreparedAuthority{Profile: a.Profile, Complete: a.Coverage == CoverageComplete, Covered: slices.Compact(covered)}, nil
}

// ValidCause reports whether c is "" (meaning target) or a known cause.
func ValidCause(c string) bool {
	return c == "" || c == CauseTarget || c == CauseParser || c == CauseNormalizer
}

type preparedEntity struct {
	ref       string
	assetType string
	key       string
	content   map[string]any
}

// endpoint is a local entity (index into prepared.entities) or an id.
type endpoint struct {
	local int // -1 when id is set
	id    string
}

type preparedEdge struct {
	from, to      endpoint
	rel           Relation
	relType       string
	label         string
	discriminator string
	content       map[string]any
}

type preparedTag struct {
	target  endpoint // id may be an edge id
	edge    bool
	ptype   string
	name    string
	content map[string]any
	source  *SourceProperty
}

const maxIDLen = 256

func (o Observation) prepare() (*prepared, error) {
	switch {
	case strings.TrimSpace(o.ObservationID) == "":
		return nil, invalidAt("observation_id", errors.New("required"))
	case len(o.ObservationID) > maxIDLen:
		return nil, invalidAt("observation_id", errors.New("too long"))
	case strings.TrimSpace(o.Tool.Name) == "":
		return nil, invalidAt("tool.name", errors.New("required"))
	case o.ObservedAt.IsZero():
		return nil, invalidAt("observed_at", errors.New("required"))
	case math.IsNaN(o.Confidence) || o.Confidence < 0 || o.Confidence > 1:
		return nil, invalidAt("confidence", fmt.Errorf("%v outside [0, 1]", o.Confidence))
	case len(o.Entities) > MaxBatchEntities:
		return nil, invalidAt("entities", fmt.Errorf("%w: %d entities, limit %d", ErrBatchTooLarge, len(o.Entities), MaxBatchEntities))
	}
	if !ValidCause(o.Cause) {
		return nil, invalidAt("cause", fmt.Errorf("cause must be target, parser or normalizer, got %q", o.Cause))
	}
	p := &prepared{
		stamp:    Stamp{At: o.ObservedAt.UTC(), Source: o.Tool.Name, Observation: o.ObservationID},
		percent:  ConfidencePercent(o.Confidence),
		entities: make([]preparedEntity, 0, len(o.Entities)),
		cause:    o.Cause, baseline: o.Baseline,
	}
	if p.cause == CauseTarget {
		p.cause = ""
	}
	refs := make(map[string]int, len(o.Entities))
	for i, e := range o.Entities {
		path := fmt.Sprintf("entities[%d]", i)
		if e.Ref == "" {
			return nil, invalidAt(path+".ref", errors.New("required"))
		}
		if isEntityID(e.Ref) || isEdgeID(e.Ref) {
			return nil, invalidAt(path+".ref", fmt.Errorf("ref %q looks like an id", e.Ref))
		}
		if _, dup := refs[e.Ref]; dup {
			return nil, invalidAt(path+".ref", fmt.Errorf("duplicate ref %q", e.Ref))
		}
		refs[e.Ref] = i
		_, typ, key, content, err := prepareAsset(e.Asset)
		if err != nil {
			return nil, invalidAt(path+".asset", err)
		}
		p.entities = append(p.entities, preparedEntity{ref: e.Ref, assetType: typ, key: key, content: content})
	}
	resolve := func(path, s string) (endpoint, error) {
		if i, ok := refs[s]; ok {
			return endpoint{local: i}, nil
		}
		if isEntityID(s) {
			return endpoint{local: -1, id: s}, nil
		}
		return endpoint{}, invalidAt(path, fmt.Errorf("%w: %q", ErrUnresolvedRef, s))
	}
	for i, e := range o.Edges {
		path := fmt.Sprintf("edges[%d]", i)
		from, err := resolve(path+".from", e.From)
		if err != nil {
			return nil, err
		}
		to, err := resolve(path+".to", e.To)
		if err != nil {
			return nil, err
		}
		relType, label, disc, content, err := SplitRelation(e.Relation)
		if err != nil {
			return nil, invalidAt(path+".relation", err)
		}
		rel, _ := e.Relation.Normalize()
		if from.local >= 0 && to.local >= 0 {
			if err := ValidateEdge(rel, p.entities[from.local].assetType, p.entities[to.local].assetType); err != nil {
				return nil, invalidAt(path+".relation", err)
			}
		}
		p.edges = append(p.edges, preparedEdge{from: from, to: to, rel: rel, relType: relType, label: label, discriminator: disc, content: content})
	}
	for i, t := range o.Tags {
		path := fmt.Sprintf("tags[%d]", i)
		var tgt endpoint
		edge := false
		if isEdgeID(t.Target) {
			tgt, edge = endpoint{local: -1, id: t.Target}, true
		} else {
			var err error
			if tgt, err = resolve(path+".target", t.Target); err != nil {
				return nil, err
			}
		}
		ptype, name, content, err := SplitProperty(t.Property)
		if err != nil {
			return nil, invalidAt(path+".property", err)
		}
		pt := preparedTag{target: tgt, edge: edge, ptype: ptype, name: name, content: content}
		if sp, ok := t.Property.(SourceProperty); ok {
			pt.source = &sp
		}
		p.tags = append(p.tags, pt)
	}
	if o.Authority != nil {
		pa, err := PrepareAuthority(*o.Authority)
		if err != nil {
			return nil, err
		}
		src, err := resolve("authority.source", o.Authority.Source)
		if err != nil {
			return nil, err
		}
		p.auth = &preparedAuth{source: src, profile: pa.Profile, complete: pa.Complete, covered: pa.Covered}
	}
	return p, nil
}

func isEntityID(s string) bool {
	if !strings.HasPrefix(s, string(core.PrefixEntity)+"_") {
		return false
	}
	_, err := core.ParsePublicID(core.PrefixEntity, s)
	return err == nil
}

func isEdgeID(s string) bool {
	if !strings.HasPrefix(s, string(core.PrefixEdge)+"_") {
		return false
	}
	_, err := core.ParsePublicID(core.PrefixEdge, s)
	return err == nil
}
