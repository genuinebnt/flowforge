// Package kernel is the merge kernel of the Postgres graph store: the
// spec 5.7 / 23.5 ingest rules of oam.Graph re-expressed over records
// loaded from storage, so a transaction can lock the touched rows, merge
// in Go with the oam primitives (Content, Stamp, Sources) and write the
// results back. It also defines the storage encoding of the bookkeeping
// oam keeps in memory (per-field stamps, sightings, covering-run counters).
//
// The kernel is pure and is tested against oam.Graph directly, including
// a storage round trip after every operation.
package kernel

import (
	"fmt"
	"slices"
	"strings"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
)

// Endpoint is a local entity (index into Prepared.Entities) or an
// existing id.
type Endpoint struct {
	Local int // -1 when ID is set
	ID    string
}

// PEntity is a prepared entity.
type PEntity struct {
	Ref, AssetType, Key, ID string
	Content                 map[string]any
}

// PEdge is a prepared edge.
type PEdge struct {
	From, To             Endpoint
	Rel                  oam.Relation
	RelType, Label, Disc string
	Content              map[string]any
}

// PTag is a prepared tag.
type PTag struct {
	Target      Endpoint
	Edge        bool
	PType, Name string
	Content     map[string]any
	Source      *oam.SourceProperty
}

// PAuth is a prepared authority.
type PAuth struct {
	Source   Endpoint
	Profile  string
	Complete bool
	Covered  []string
}

// Prepared is a validated, normalized observation.
type Prepared struct {
	Obs      oam.Observation
	Stamp    oam.Stamp
	Percent  int
	Entities []PEntity
	Edges    []PEdge
	Tags     []PTag
	Auth     *PAuth
	Cause    string
	Baseline bool
}

// SourceID returns the authority source's entity id ("" without
// authority).
func (p *Prepared) SourceID() string {
	if p.Auth == nil {
		return ""
	}
	return p.IDOf(p.Auth.Source)
}

// Prepare validates o with oam's own rules (so errors are identical to
// oam.Graph.Apply's) and normalizes it. o.TenantID and o.ProgramID must
// already be canonical.
func Prepare(o oam.Observation) (*Prepared, error) {
	if err := o.Validate(); err != nil {
		return nil, err
	}
	p := &Prepared{
		Obs:     o,
		Stamp:   oam.Stamp{At: o.ObservedAt.UTC(), Source: o.Tool.Name, Observation: o.ObservationID},
		Percent: oam.ConfidencePercent(o.Confidence),
		Cause:   o.Cause, Baseline: o.Baseline,
	}
	if p.Cause == oam.CauseTarget {
		p.Cause = ""
	}
	refs := make(map[string]int, len(o.Entities))
	for i, e := range o.Entities {
		n, err := e.Asset.Normalize()
		if err != nil {
			return nil, err
		}
		key, err := n.Key()
		if err != nil {
			return nil, err
		}
		typ, content, err := oam.SplitAsset(n)
		if err != nil {
			return nil, err
		}
		refs[e.Ref] = i
		p.Entities = append(p.Entities, PEntity{
			Ref: e.Ref, AssetType: typ, Key: key, Content: content,
			ID: oam.EntityID(o.TenantID, o.ProgramID, typ, key),
		})
	}
	resolve := func(s string) (Endpoint, error) {
		if i, ok := refs[s]; ok {
			return Endpoint{Local: i}, nil
		}
		if IsEntityID(s) {
			return Endpoint{Local: -1, ID: s}, nil
		}
		return Endpoint{}, fmt.Errorf("%w: %q", oam.ErrUnresolvedRef, s)
	}
	for _, e := range o.Edges {
		from, err := resolve(e.From)
		if err != nil {
			return nil, err
		}
		to, err := resolve(e.To)
		if err != nil {
			return nil, err
		}
		relType, label, disc, content, err := oam.SplitRelation(e.Relation)
		if err != nil {
			return nil, err
		}
		rel, err := e.Relation.Normalize()
		if err != nil {
			return nil, err
		}
		p.Edges = append(p.Edges, PEdge{From: from, To: to, Rel: rel, RelType: relType, Label: label, Disc: disc, Content: content})
	}
	for _, t := range o.Tags {
		var tgt Endpoint
		edge := IsEdgeID(t.Target)
		if edge {
			tgt = Endpoint{Local: -1, ID: t.Target}
		} else {
			var err error
			if tgt, err = resolve(t.Target); err != nil {
				return nil, err
			}
		}
		ptype, name, content, err := oam.SplitProperty(t.Property)
		if err != nil {
			return nil, err
		}
		pt := PTag{Target: tgt, Edge: edge, PType: ptype, Name: name, Content: content}
		if sp, ok := t.Property.(oam.SourceProperty); ok {
			pt.Source = &sp
		}
		p.Tags = append(p.Tags, pt)
	}
	if o.Authority != nil {
		pa, err := oam.PrepareAuthority(*o.Authority)
		if err != nil {
			return nil, err
		}
		src, err := resolve(o.Authority.Source)
		if err != nil {
			return nil, err
		}
		p.Auth = &PAuth{Source: src, Profile: pa.Profile, Complete: pa.Complete, Covered: pa.Covered}
	}
	return p, nil
}

// IDOf returns an endpoint's entity id.
func (p *Prepared) IDOf(ep Endpoint) string {
	if ep.Local >= 0 {
		return p.Entities[ep.Local].ID
	}
	return ep.ID
}

// EdgeKey returns the upsert key of a prepared edge.
func (p *Prepared) EdgeKey(e PEdge) oam.EdgeKey {
	return oam.EdgeKey{TenantID: p.Obs.TenantID, ProgramID: p.Obs.ProgramID, From: p.IDOf(e.From), To: p.IDOf(e.To),
		RelationType: e.RelType, Label: e.Label, Discriminator: e.Disc}
}

// EntityIDs returns every entity id the observation touches or names,
// sorted and unique: the rows a transaction must load and lock.
func (p *Prepared) EntityIDs() []string {
	var ids []string
	for _, e := range p.Entities {
		ids = append(ids, e.ID)
	}
	for _, e := range p.Edges {
		ids = append(ids, p.IDOf(e.From), p.IDOf(e.To))
	}
	for _, t := range p.Tags {
		if !t.Edge {
			ids = append(ids, p.IDOf(t.Target))
		}
	}
	if p.Auth != nil {
		ids = append(ids, p.SourceID())
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// EdgeIDs returns every edge id the observation touches or tags, sorted
// and unique.
func (p *Prepared) EdgeIDs() []string {
	var ids []string
	for _, e := range p.Edges {
		ids = append(ids, p.EdgeKey(e).ID())
	}
	for _, t := range p.Tags {
		if t.Edge {
			ids = append(ids, t.Target.ID)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

// IsEntityID reports whether s is a well-formed ent_ id (oam's rule).
func IsEntityID(s string) bool { return isID(core.PrefixEntity, s) }

// IsEdgeID reports whether s is a well-formed edg_ id (oam's rule).
func IsEdgeID(s string) bool { return isID(core.PrefixEdge, s) }

func isID(p core.Prefix, s string) bool {
	if !strings.HasPrefix(s, string(p)+"_") {
		return false
	}
	_, err := core.ParsePublicID(p, s)
	return err == nil
}
