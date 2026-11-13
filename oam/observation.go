package oam

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
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
	p := &prepared{
		stamp:    Stamp{At: o.ObservedAt.UTC(), Source: o.Tool.Name, Observation: o.ObservationID},
		percent:  ConfidencePercent(o.Confidence),
		entities: make([]preparedEntity, 0, len(o.Entities)),
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
