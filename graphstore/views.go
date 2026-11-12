package graphstore

import (
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
)

// RoundConfidence rounds to the four decimals oam keeps. It recovers the
// exact float64 from a value stored as Postgres real.
func RoundConfidence(c float64) float64 { return math.Round(c*1e4) / 1e4 }

// TagFromOAM converts an oam tag view on a target.
func TagFromOAM(tenantID string, kind TargetKind, targetID string, t oam.Tag) Tag {
	return Tag{
		ID:         core.PublicID(core.PrefixTag, TagUUID(tenantID, kind, targetID, t.PropertyType, t.Name)),
		TargetKind: kind, TargetID: targetID, PropertyType: t.PropertyType, Name: t.Name,
		Content: t.Content, CreatedAt: t.CreatedAt, LastSeen: t.LastSeen,
	}
}

// EntityFromOAM converts an oam entity view; MissedRuns and Tags are kept
// only when withTags is set.
func EntityFromOAM(e oam.Entity, withTags bool) Entity {
	out := Entity{
		ID: e.ID, TenantID: e.TenantID, ProgramID: e.ProgramID, AssetType: e.AssetType, Key: e.Key,
		Content: e.Content, ScopeStatus: e.ScopeStatus, Confidence: e.Confidence, State: e.State,
		CreatedAt: e.CreatedAt, LastSeen: e.LastSeen, FirstSeenRun: e.FirstSeenRun, LastSeenRun: e.LastSeenRun,
	}
	if withTags {
		out.MissedRuns = e.MissedRuns
		out.Tags = make([]Tag, 0, len(e.Tags))
		for _, t := range e.Tags {
			out.Tags = append(out.Tags, TagFromOAM(e.TenantID, TargetEntity, e.ID, t))
		}
	}
	return out
}

// EdgeFromOAM converts an oam edge view; MissedRuns and Tags are kept only
// when withTags is set.
func EdgeFromOAM(e oam.Edge, withTags bool) Edge {
	out := Edge{
		ID: e.ID, TenantID: e.TenantID, ProgramID: e.ProgramID, RelationType: e.RelationType, Label: e.Label,
		Discriminator: e.Discriminator, Content: e.Content, FromEntityID: e.FromEntity, ToEntityID: e.ToEntity,
		Confidence: e.Confidence, State: e.State, CreatedAt: e.CreatedAt, LastSeen: e.LastSeen,
	}
	if withTags {
		out.MissedRuns = e.MissedRuns
		out.Tags = make([]Tag, 0, len(e.Tags))
		for _, t := range e.Tags {
			out.Tags = append(out.Tags, TagFromOAM(e.TenantID, TargetEdge, e.ID, t))
		}
	}
	return out
}

// ChangeFromOAM converts an oam change event (without the entity snapshot).
func ChangeFromOAM(ev oam.ChangeEvent) ChangeEvent {
	return ChangeEvent{
		ID: ev.ID, Seq: ev.Seq, TenantID: ev.TenantID, ProgramID: ev.ProgramID, Type: ev.Type,
		EntityID: ev.EntityID, EdgeID: ev.EdgeID, RunID: ev.RunID, Patch: ev.Patch, At: ev.At,
	}
}

// StripTags returns e without tags and missed-run counters.
func StripTags(e Entity) Entity {
	e.Tags, e.MissedRuns = nil, nil
	return e
}

// nameFields is the content field that carries each property type's name.
var nameFields = map[string]string{
	oam.PropertySimple: "name", oam.PropertyFlowforge: "name",
	oam.PropertyDNSRecord: "property_name", oam.PropertyVuln: "id",
}

// TagProperty builds and validates the property of an AddTagRequest.
// Content may omit the name field when Name is set; when both are set
// they must agree. SourceProperty is rejected (provenance only comes from
// ingest) and so is an unknown property type.
func TagProperty(req AddTagRequest) (oam.Property, error) {
	if req.PropertyType == oam.PropertySource {
		return nil, InvalidArgument("property_type", "SourceProperty tags are recorded by ingest only")
	}
	field, ok := nameFields[req.PropertyType]
	if !ok {
		return nil, InvalidArgument("property_type", "unknown property type %q", req.PropertyType)
	}
	content := maps.Clone(req.Content)
	if content == nil {
		content = map[string]any{}
	}
	if _, has := content[field]; !has && req.Name != "" {
		content[field] = req.Name
	}
	p, err := oam.JoinProperty(req.PropertyType, content, oam.Strict)
	if err != nil {
		return nil, Classify(err)
	}
	ptype, name, _, err := oam.SplitProperty(p)
	if err != nil {
		return nil, Classify(err)
	}
	if req.Name != "" && strings.TrimSpace(req.Name) != name {
		return nil, InvalidArgument("name", "name %q does not match the property's %q", req.Name, name)
	}
	_ = ptype
	return p, nil
}

// ValidateChangeTypes checks a change-type filter.
func ValidateChangeTypes(types []oam.ChangeType) error {
	known := []oam.ChangeType{
		oam.ChangeEntityAdded, oam.ChangeEntityChanged, oam.ChangeEntityStale, oam.ChangeEntityReactivated,
		oam.ChangeEdgeAdded, oam.ChangeEdgeRemoved, oam.ChangeScopeStatusChanged,
	}
	for _, t := range types {
		if !slices.Contains(known, t) {
			return InvalidArgument("types", "unknown change type %q", t)
		}
	}
	return nil
}
