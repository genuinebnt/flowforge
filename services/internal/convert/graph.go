package convert

import (
	"encoding/json"
	"fmt"
	"strings"

	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/oam"
)

// ObservationProto converts an adapter's observation (spec 6.1) to the
// streamed proto form, splitting assets, relations and properties into
// type + content with oam's normalizing Split functions.
func ObservationProto(o oam.Observation) (*pb.Observation, error) {
	out := &pb.Observation{ObservationId: o.ObservationID, ObservedAt: TS(o.ObservedAt), Confidence: o.Confidence, RawRef: o.RawRef}
	for _, e := range o.Entities {
		t, content, err := oam.SplitAsset(e.Asset)
		if err != nil {
			return nil, fmt.Errorf("entity %s: %w", e.Ref, err)
		}
		out.Entities = append(out.Entities, &pb.OamEntity{Ref: e.Ref, AssetType: t, Content: StructFromMap(content)})
	}
	for _, e := range o.Edges {
		rt, label, _, content, err := oam.SplitRelation(e.Relation)
		if err != nil {
			return nil, fmt.Errorf("edge %s->%s: %w", e.From, e.To, err)
		}
		out.Edges = append(out.Edges, &pb.OamEdge{From: e.From, To: e.To, Relation: &pb.OamRelation{RelationType: rt, Label: label, Content: StructFromMap(content)}})
	}
	for _, t := range o.Tags {
		pt, name, content, err := oam.SplitProperty(t.Property)
		if err != nil {
			return nil, fmt.Errorf("tag on %s: %w", t.Target, err)
		}
		out.Tags = append(out.Tags, &pb.OamTag{Target: t.Target, Property: &pb.OamProperty{PropertyType: pt, Name: name, Content: StructFromMap(content)}})
	}
	return out, nil
}

func withType(key, typ string, content map[string]any) map[string]any {
	m := map[string]any{}
	for k, v := range content {
		m[k] = v
	}
	m[key] = typ
	return m
}

// observationMap renders a proto observation in the spec 6.1 JSON shape.
func observationMap(p *pb.Observation) map[string]any {
	m := map[string]any{"observation_id": p.GetObservationId(), "confidence": p.GetConfidence()}
	if p.GetObservedAt() != nil {
		m["observed_at"] = Time(p.GetObservedAt()).Format("2006-01-02T15:04:05.999999999Z07:00")
	}
	if p.GetRawRef() != "" {
		m["raw_ref"] = p.GetRawRef()
	}
	var ents, edges, tags []any
	for _, e := range p.GetEntities() {
		ents = append(ents, map[string]any{"ref": e.GetRef(), "asset": withType("type", e.GetAssetType(), StructToMap(e.GetContent()))})
	}
	for _, e := range p.GetEdges() {
		rel := withType("type", e.GetRelation().GetRelationType(), StructToMap(e.GetRelation().GetContent()))
		rel["label"] = e.GetRelation().GetLabel()
		edges = append(edges, map[string]any{"from": e.GetFrom(), "to": e.GetTo(), "relation": rel})
	}
	for _, t := range p.GetTags() {
		prop := withType("type", t.GetProperty().GetPropertyType(), StructToMap(t.GetProperty().GetContent()))
		if _, ok := prop["name"]; !ok && t.GetProperty().GetName() != "" {
			prop["name"] = t.GetProperty().GetName()
		}
		tags = append(tags, map[string]any{"target": t.GetTarget(), "property": prop})
	}
	if ents != nil {
		m["entities"] = ents
	}
	if edges != nil {
		m["edges"] = edges
	}
	if tags != nil {
		m["tags"] = tags
	}
	return m
}

// ObservationToJSON renders a proto observation as a spec 6.1 envelope
// (what the engine records and forwards).
func ObservationToJSON(p *pb.Observation) (json.RawMessage, error) {
	return json.Marshal(observationMap(p))
}

// ObservationFromJSON parses a spec 6.1 envelope into the proto form. The
// envelope fields the engine adds (tenant_id, program_id, run_id,
// effect_path, tool) are ignored: they travel in IngestRequest.
func ObservationFromJSON(raw []byte) (*pb.Observation, error) {
	var o struct {
		ObservationID string  `json:"observation_id"`
		ObservedAt    string  `json:"observed_at"`
		Confidence    float64 `json:"confidence"`
		RawRef        string  `json:"raw_ref"`
		Entities      []struct {
			Ref   string         `json:"ref"`
			Asset map[string]any `json:"asset"`
		} `json:"entities"`
		Edges []struct {
			From     string         `json:"from"`
			To       string         `json:"to"`
			Relation map[string]any `json:"relation"`
		} `json:"edges"`
		Tags []struct {
			Target   string         `json:"target"`
			Property map[string]any `json:"property"`
		} `json:"tags"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, Invalid("observation", "observation is not valid JSON: %v", err)
	}
	out := &pb.Observation{ObservationId: o.ObservationID, Confidence: o.Confidence, RawRef: o.RawRef}
	if o.ObservedAt != "" {
		t, err := parseAnyTime(o.ObservedAt)
		if err != nil {
			return nil, Invalid("observation.observed_at", "observed_at: %v", err)
		}
		out.ObservedAt = TS(t)
	}
	pop := func(m map[string]any, k string) string {
		s, _ := m[k].(string)
		delete(m, k)
		return s
	}
	for _, e := range o.Entities {
		t := pop(e.Asset, "type")
		out.Entities = append(out.Entities, &pb.OamEntity{Ref: e.Ref, AssetType: t, Content: StructFromMap(e.Asset)})
	}
	for _, e := range o.Edges {
		t, l := pop(e.Relation, "type"), pop(e.Relation, "label")
		out.Edges = append(out.Edges, &pb.OamEdge{From: e.From, To: e.To, Relation: &pb.OamRelation{RelationType: t, Label: l, Content: StructFromMap(e.Relation)}})
	}
	for _, t := range o.Tags {
		pt := pop(t.Property, "type")
		name, _ := t.Property["name"].(string)
		out.Tags = append(out.Tags, &pb.OamTag{Target: t.Target, Property: &pb.OamProperty{PropertyType: pt, Name: name, Content: StructFromMap(t.Property)}})
	}
	return out, nil
}

// ObservationDomain converts a proto observation to oam's (strict decoding,
// spec 6.1), stamping the program, run and tool of the ingest request.
func ObservationDomain(p *pb.Observation, program, run, tool, version string) (oam.Observation, error) {
	if p == nil {
		return oam.Observation{}, Invalid("observation", "observation is required")
	}
	m := observationMap(p)
	if program != "" {
		m["program_id"] = program
	}
	if run != "" {
		m["run_id"] = run
	}
	if tool != "" {
		t := map[string]any{"name": tool}
		if version != "" {
			t["version"] = version
		}
		m["tool"] = t
	}
	b, err := json.Marshal(m)
	if err != nil {
		return oam.Observation{}, Invalid("observation", "%v", err)
	}
	o, err := oam.DecodeObservation(b, oam.Strict)
	if err != nil {
		return oam.Observation{}, graphstore.Classify(err)
	}
	return o, nil
}

// EntityProto converts a graph entity.
func EntityProto(e graphstore.Entity) *pb.Entity {
	out := &pb.Entity{Id: e.ID, ProgramId: e.ProgramID, AssetType: e.AssetType, Key: e.Key, Content: StructFromMap(e.Content),
		ScopeStatus: string(e.ScopeStatus), Confidence: e.Confidence, State: string(e.State), CreatedAt: TS(e.CreatedAt),
		LastSeen: TS(e.LastSeen), FirstSeenRun: e.FirstSeenRun, LastSeenRun: e.LastSeenRun}
	for _, t := range e.Tags {
		out.Tags = append(out.Tags, TagProto(t))
	}
	return out
}

// EntityFromProto converts a proto entity (as carried by change events).
func EntityFromProto(p *pb.Entity) graphstore.Entity {
	return graphstore.Entity{ID: p.GetId(), ProgramID: p.GetProgramId(), AssetType: p.GetAssetType(), Key: p.GetKey(),
		Content: StructToMap(p.GetContent()), ScopeStatus: oam.ScopeStatus(p.GetScopeStatus()), Confidence: p.GetConfidence(),
		State: oam.State(p.GetState()), CreatedAt: Time(p.GetCreatedAt()), LastSeen: Time(p.GetLastSeen()),
		FirstSeenRun: p.GetFirstSeenRun(), LastSeenRun: p.GetLastSeenRun()}
}

// EdgeProto converts a graph edge.
func EdgeProto(e graphstore.Edge) *pb.Edge {
	out := &pb.Edge{Id: e.ID, RelationType: e.RelationType, Label: e.Label, Discriminator: e.Discriminator,
		Content: StructFromMap(e.Content), FromEntityId: e.FromEntityID, ToEntityId: e.ToEntityID, Confidence: e.Confidence,
		State: string(e.State), CreatedAt: TS(e.CreatedAt), LastSeen: TS(e.LastSeen)}
	for _, t := range e.Tags {
		out.Tags = append(out.Tags, TagProto(t))
	}
	return out
}

// TagProto converts a tag.
func TagProto(t graphstore.Tag) *pb.Tag {
	return &pb.Tag{Id: t.ID, TargetKind: string(t.TargetKind), TargetId: t.TargetID, PropertyType: t.PropertyType,
		Name: t.Name, Content: StructFromMap(t.Content), LastSeen: TS(t.LastSeen)}
}

// ChangeProto converts a change event; the patch travels as {"ops": [...]}.
func ChangeProto(c graphstore.ChangeEvent) *pb.ChangeEvent {
	out := &pb.ChangeEvent{Id: c.ID, Seq: c.Seq, ProgramId: c.ProgramID, Type: string(c.Type), EntityId: c.EntityID,
		EdgeId: c.EdgeID, RunId: c.RunID, At: TS(c.At)}
	if len(c.Patch) > 0 {
		ops := make([]any, 0, len(c.Patch))
		for _, op := range c.Patch {
			ops = append(ops, map[string]any{"op": op.Op, "path": op.Path, "value": op.Value})
		}
		out.Patch = StructFromMap(map[string]any{"ops": ops})
	}
	if c.Entity != nil {
		out.Entity = EntityProto(*c.Entity)
	}
	return out
}

// ChangeTypes converts a change-type filter.
func ChangeTypes(ts []string) []oam.ChangeType {
	out := make([]oam.ChangeType, 0, len(ts))
	for _, t := range ts {
		out = append(out, oam.ChangeType(strings.TrimPrefix(t, "graph.")))
	}
	return out
}
