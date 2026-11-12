package gateway

// All JSON <-> proto conversion of the gateway lives in this file: enum
// names, timestamps, Struct/Value payloads and the response shapes of spec
// 12.4-12.7 and 22.9.

import (
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/genuinebnt/flowforge/core"
	graphv1 "github.com/genuinebnt/flowforge/gen/flowforge/graph/v1"
	similarityv1 "github.com/genuinebnt/flowforge/gen/flowforge/similarity/v1"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
)

// ---- scalars -----------------------------------------------------------

func ts(t *timestamppb.Timestamp) any {
	if t == nil || (t.Seconds == 0 && t.Nanos == 0) {
		return nil
	}
	return core.FormatTime(t.AsTime())
}

func pbTime(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

func timeOrNull(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return core.FormatTime(t)
}

func structJSON(s *structpb.Struct) any {
	if s == nil {
		return nil
	}
	return s.AsMap()
}

func structOrEmpty(s *structpb.Struct) any {
	if s == nil {
		return map[string]any{}
	}
	return s.AsMap()
}

func valueJSON(v *structpb.Value) any {
	if v == nil {
		return nil
	}
	return v.AsInterface()
}

func toStruct(field string, m map[string]any) (*structpb.Struct, error) {
	if m == nil {
		return nil, nil
	}
	s, err := structpb.NewStruct(m)
	if err != nil {
		return nil, errValidation("%s: %v", field, err)
	}
	return s, nil
}

func strs(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func nonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// ---- enums -------------------------------------------------------------

// enumName lower-cases an enum value name without its prefix
// (ACTIVITY_CLASS_ACTIVE -> active); UNSPECIFIED becomes "".
func enumName(name, prefix string) string {
	n := strings.TrimPrefix(name, prefix)
	if n == "UNSPECIFIED" {
		return ""
	}
	return strings.ToLower(n)
}

func enumValue(field, v, prefix string, values map[string]int32) (int32, error) {
	if v == "" {
		return 0, nil
	}
	n, ok := values[prefix+strings.ToUpper(v)]
	if !ok || n == 0 {
		return 0, errValidation("%s: unknown value %q", field, v)
	}
	return n, nil
}

func activityName(c pb.ActivityClass) any {
	return nonEmpty(enumName(c.String(), "ACTIVITY_CLASS_"))
}

func parseActivity(field, v string) (pb.ActivityClass, error) {
	n, err := enumValue(field, v, "ACTIVITY_CLASS_", pb.ActivityClass_value)
	return pb.ActivityClass(n), err
}

func runStatusName(s pb.RunStatus) string { return enumName(s.String(), "RUN_STATUS_") }

func parseRunStatus(v string) (pb.RunStatus, error) {
	n, err := enumValue("status", v, "RUN_STATUS_", pb.RunStatus_value)
	return pb.RunStatus(n), err
}

// ---- scope -------------------------------------------------------------

type programOut struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Platform       string `json:"platform"`
	Status         string `json:"status"`
	CurrentVersion int32  `json:"current_version"`
	CreatedAt      any    `json:"created_at"`
}

func programJSON(p *pb.Program) programOut {
	return programOut{ID: p.GetId(), Name: p.GetName(), Platform: p.GetPlatform(), Status: p.GetStatus(),
		CurrentVersion: p.GetCurrentVersion(), CreatedAt: ts(p.GetCreatedAt())}
}

type ruleIO struct {
	ID     string `json:"id"`
	Effect string `json:"effect"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
	Note   string `json:"note,omitempty"`
}

type rateIO struct {
	RequestsPerSecond float64 `json:"requests_per_second"`
	Burst             int32   `json:"burst"`
	Concurrency       int32   `json:"concurrency"`
}

type windowIO struct {
	Days  []string `json:"days"`
	Start string   `json:"start"`
	End   string   `json:"end"`
	TZ    string   `json:"tz"`
}

type activityIO struct {
	MaxClass   string     `json:"max_class"`
	Prohibited []string   `json:"prohibited"`
	Rate       *rateIO    `json:"rate"`
	Windows    []windowIO `json:"windows"`
}

type scopeDocIO struct {
	Rules    []ruleIO    `json:"rules"`
	Activity *activityIO `json:"activity"`
}

func scopeDocJSON(d *pb.ScopeDocument) scopeDocIO {
	out := scopeDocIO{Rules: []ruleIO{}}
	for _, r := range d.GetRules() {
		out.Rules = append(out.Rules, ruleIO{ID: r.GetId(), Effect: enumName(r.GetEffect().String(), "RULE_EFFECT_"),
			Kind: enumName(r.GetKind().String(), "RULE_KIND_"), Value: r.GetValue(), Note: r.GetNote()})
	}
	if a := d.GetActivity(); a != nil {
		act := &activityIO{Prohibited: strs(a.GetProhibited()), Windows: []windowIO{}}
		if n, ok := activityName(a.GetMaxClass()).(string); ok {
			act.MaxClass = n
		}
		if r := a.GetRate(); r != nil {
			act.Rate = &rateIO{RequestsPerSecond: r.GetRequestsPerSecond(), Burst: r.GetBurst(), Concurrency: r.GetConcurrency()}
		}
		for _, w := range a.GetWindows() {
			act.Windows = append(act.Windows, windowIO{Days: strs(w.GetDays()), Start: w.GetStart(), End: w.GetEnd(), TZ: w.GetTz()})
		}
		out.Activity = act
	}
	return out
}

func scopeDocProto(field string, d *scopeDocIO) (*pb.ScopeDocument, error) {
	if d == nil {
		return nil, errValidation("%s: required", field)
	}
	out := &pb.ScopeDocument{}
	for i, r := range d.Rules {
		eff, err := enumValue(field+".rules.effect", r.Effect, "RULE_EFFECT_", pb.RuleEffect_value)
		if err != nil {
			return nil, err
		}
		kind, err := enumValue(field+".rules.kind", r.Kind, "RULE_KIND_", pb.RuleKind_value)
		if err != nil {
			return nil, err
		}
		if r.ID == "" || eff == 0 || kind == 0 || r.Value == "" {
			return nil, errValidation("%s.rules[%d]: id, effect, kind and value are required", field, i)
		}
		out.Rules = append(out.Rules, &pb.ScopeRule{Id: r.ID, Effect: pb.RuleEffect(eff), Kind: pb.RuleKind(kind), Value: r.Value, Note: r.Note})
	}
	if a := d.Activity; a != nil {
		mc, err := parseActivity(field+".activity.max_class", a.MaxClass)
		if err != nil {
			return nil, err
		}
		pa := &pb.ActivityPolicy{MaxClass: mc, Prohibited: a.Prohibited}
		if r := a.Rate; r != nil {
			if r.RequestsPerSecond < 0 || r.Burst < 0 || r.Concurrency < 0 {
				return nil, errValidation("%s.activity.rate: values must not be negative", field)
			}
			pa.Rate = &pb.RateLimit{RequestsPerSecond: r.RequestsPerSecond, Burst: r.Burst, Concurrency: r.Concurrency}
		}
		for _, w := range a.Windows {
			pa.Windows = append(pa.Windows, &pb.TimeWindow{Days: w.Days, Start: w.Start, End: w.End, Tz: w.TZ})
		}
		out.Activity = pa
	}
	return out, nil
}

type scopeVersionOut struct {
	ProgramID     string      `json:"program_id"`
	Version       int32       `json:"version"`
	EffectiveFrom any         `json:"effective_from"`
	CreatedBy     string      `json:"created_by"`
	CreatedAt     any         `json:"created_at"`
	Document      *scopeDocIO `json:"document,omitempty"`
}

func scopeVersionJSON(v *pb.ScopeVersion, withDoc bool) scopeVersionOut {
	out := scopeVersionOut{ProgramID: v.GetProgramId(), Version: v.GetVersion(), EffectiveFrom: ts(v.GetEffectiveFrom()),
		CreatedBy: v.GetCreatedBy(), CreatedAt: ts(v.GetCreatedAt())}
	if withDoc {
		d := scopeDocJSON(v.GetDocument())
		out.Document = &d
	}
	return out
}

func decisionJSON(d *pb.Decision) map[string]any {
	return map[string]any{
		"result": enumName(d.GetResult().String(), "DECISION_RESULT_"), "reason": d.GetReason(), "rule_id": nonEmpty(d.GetRuleId()),
		"scope_version": d.GetScopeVersion(), "normalized_target": d.GetNormalizedTarget(), "pinned_ips": strs(d.GetPinnedIps()),
		"decision_id": d.GetDecisionId(), "decided_at": ts(d.GetDecidedAt()),
	}
}

func killSwitchJSON(k *pb.KillSwitch) map[string]any {
	return map[string]any{"level": k.GetLevel(), "target_id": k.GetTargetId(), "enabled": k.GetEnabled(), "reason": k.GetReason(),
		"set_by": k.GetSetBy(), "set_at": ts(k.GetSetAt())}
}

// ---- workflows and runs ------------------------------------------------

type posOut struct {
	Line   int32 `json:"line"`
	Col    int32 `json:"col"`
	Offset int32 `json:"offset"`
}

type spanOut struct {
	File  string `json:"file"`
	Start posOut `json:"start"`
	End   posOut `json:"end"`
	Label string `json:"label"`
}

func spanJSON(s *pb.SourceSpan) *spanOut {
	if s == nil {
		return nil
	}
	p := func(x *pb.SourcePos) posOut { return posOut{Line: x.GetLine(), Col: x.GetCol(), Offset: x.GetOffset()} }
	return &spanOut{File: s.GetFile(), Start: p(s.GetStart()), End: p(s.GetEnd()), Label: s.GetLabel()}
}

func diagnosticJSON(d *pb.Diagnostic) map[string]any {
	sec := []*spanOut{}
	for _, s := range d.GetSecondary() {
		sec = append(sec, spanJSON(s))
	}
	return map[string]any{"code": d.GetCode(), "severity": enumName(d.GetSeverity().String(), "SEVERITY_"), "message": d.GetMessage(),
		"primary": spanJSON(d.GetPrimary()), "secondary": sec, "help": d.GetHelp()}
}

func diagnostics(ds []*pb.Diagnostic) []map[string]any {
	out := []map[string]any{}
	for _, d := range ds {
		out = append(out, diagnosticJSON(d))
	}
	return out
}

func workflowJSON(w *pb.WorkflowVersion, withSource bool) map[string]any {
	m := map[string]any{"id": w.GetId(), "name": w.GetName(), "version": w.GetVersion(), "status": w.GetStatus(),
		"module_sha256": w.GetModuleSha256(), "effects": strs(w.GetEffects()), "inferred_class": activityName(w.GetInferredClass()),
		"diagnostics": diagnostics(w.GetDiagnostics()), "created_at": ts(w.GetCreatedAt()), "created_by": nonEmpty(w.GetCreatedBy())}
	if withSource {
		m["source"] = w.GetSource()
	}
	return m
}

func runJSON(r *pb.Run) map[string]any {
	var failure any
	if r.GetFailureCode() != "" || r.GetFailureMessage() != "" {
		failure = map[string]any{"code": r.GetFailureCode(), "message": r.GetFailureMessage()}
	}
	return map[string]any{
		"id": r.GetId(), "status": runStatusName(r.GetStatus()), "workflow": r.GetWorkflowName(), "workflow_version": r.GetWorkflowVersion(),
		"program_id": r.GetProgramId(), "scope_version": r.GetScopeVersion(), "input": structOrEmpty(r.GetInput()),
		"output": valueJSON(r.GetOutput()), "failure": failure, "compensation_outcome": nonEmpty(r.GetCompensationOutcome()),
		"waiting_on": nil, "created_by": r.GetCreatedBy(), "created_at": ts(r.GetCreatedAt()), "started_at": ts(r.GetStartedAt()),
		"finished_at": ts(r.GetFinishedAt()), "last_seq": r.GetLastSeq(), "parent_run_id": nonEmpty(r.GetParentRunId()),
	}
}

func runEventJSON(e *pb.RunEvent) map[string]any {
	return map[string]any{"seq": e.GetSeq(), "type": e.GetType(), "at": ts(e.GetAt()), "actor": e.GetActor(), "payload": structOrEmpty(e.GetPayload())}
}

func approvalJSON(a *pb.Approval) map[string]any {
	return map[string]any{"id": a.GetId(), "run_id": a.GetRunId(), "effect_path": a.GetEffectPath(), "role": a.GetRole(),
		"reason": a.GetReason(), "proposed": structJSON(a.GetProposed()), "status": a.GetStatus(), "expires_at": ts(a.GetExpiresAt()),
		"decided_by": nonEmpty(a.GetDecidedBy()), "decided_at": ts(a.GetDecidedAt()), "comment": nonEmpty(a.GetComment()),
		"created_at": ts(a.GetCreatedAt())}
}

func toolJSON(t *pb.ToolSpec) map[string]any {
	var timeout any
	if t.GetTimeout() != nil {
		timeout = t.GetTimeout().AsDuration().Milliseconds()
	}
	return map[string]any{"name": t.GetName(), "version": t.GetVersion(), "description": t.GetDescription(),
		"input_type": t.GetInputType(), "output_type": t.GetOutputType(), "observes": strs(t.GetObserves()),
		"effect_class": activityName(t.GetEffectClass()), "rate_rps": t.GetRateRps(), "rate_burst": t.GetRateBurst(),
		"concurrency": t.GetConcurrency(), "timeout_ms": timeout, "idempotent": t.GetIdempotent(), "spec_sha256": t.GetSpecSha256()}
}

// ---- graph -------------------------------------------------------------

func tagJSON(t *pb.Tag) map[string]any {
	return map[string]any{"id": t.GetId(), "target_kind": t.GetTargetKind(), "target_id": t.GetTargetId(),
		"property_type": t.GetPropertyType(), "name": t.GetName(), "content": structJSON(t.GetContent()), "last_seen": ts(t.GetLastSeen())}
}

func tags(ts []*pb.Tag) []map[string]any {
	out := []map[string]any{}
	for _, t := range ts {
		out = append(out, tagJSON(t))
	}
	return out
}

func entityJSON(e *pb.Entity) map[string]any {
	if e == nil {
		return nil
	}
	return map[string]any{"id": e.GetId(), "program_id": e.GetProgramId(), "asset_type": e.GetAssetType(), "key": e.GetKey(),
		"content": structOrEmpty(e.GetContent()), "scope_status": e.GetScopeStatus(), "confidence": e.GetConfidence(),
		"state": e.GetState(), "created_at": ts(e.GetCreatedAt()), "last_seen": ts(e.GetLastSeen()),
		"first_seen_run": nonEmpty(e.GetFirstSeenRun()), "last_seen_run": nonEmpty(e.GetLastSeenRun()), "tags": tags(e.GetTags())}
}

func edgeJSON(e *pb.Edge) map[string]any {
	return map[string]any{"id": e.GetId(), "relation_type": e.GetRelationType(), "label": e.GetLabel(),
		"discriminator": nonEmpty(e.GetDiscriminator()), "content": structOrEmpty(e.GetContent()),
		"from_entity_id": e.GetFromEntityId(), "to_entity_id": e.GetToEntityId(), "confidence": e.GetConfidence(),
		"state": e.GetState(), "created_at": ts(e.GetCreatedAt()), "last_seen": ts(e.GetLastSeen()), "tags": tags(e.GetTags())}
}

func edges(es []*pb.Edge) []map[string]any {
	out := []map[string]any{}
	for _, e := range es {
		out = append(out, edgeJSON(e))
	}
	return out
}

func entities(es []*pb.Entity) []map[string]any {
	out := []map[string]any{}
	for _, e := range es {
		out = append(out, entityJSON(e))
	}
	return out
}

func subgraphJSON(g *pb.Subgraph) map[string]any {
	return map[string]any{"entities": entities(g.GetEntities()), "edges": edges(g.GetEdges()), "truncated": g.GetTruncated()}
}

func changeJSON(c *pb.ChangeEvent) map[string]any {
	return map[string]any{"id": c.GetId(), "seq": c.GetSeq(), "program_id": c.GetProgramId(), "type": c.GetType(),
		"entity_id": nonEmpty(c.GetEntityId()), "edge_id": nonEmpty(c.GetEdgeId()), "run_id": nonEmpty(c.GetRunId()),
		"patch": structJSON(c.GetPatch()), "entity": entityJSON(c.GetEntity()), "at": ts(c.GetAt())}
}

func pathJSON(p *pb.PathResponse) map[string]any {
	return map[string]any{"entity_ids": strs(p.GetEntityIds()), "edges": edges(p.GetEdges()),
		"total_cost": p.GetTotalCost(), "combined_confidence": p.GetCombinedConfidence()}
}

func parseDirection(v string) (pb.Direction, error) {
	n, err := enumValue("direction", v, "DIRECTION_", pb.Direction_value)
	return pb.Direction(n), err
}

// ---- analytics and similarity (21.6, 22.9) -----------------------------

type bfsIn struct {
	StartEntityID string   `json:"start_entity_id"`
	MaxDepth      uint32   `json:"max_depth"`
	Direction     string   `json:"direction"`
	Labels        []string `json:"labels"`
}

type shortestIn struct {
	FromEntityID string   `json:"from_entity_id"`
	ToEntityID   string   `json:"to_entity_id"`
	Labels       []string `json:"labels"`
}

type pagerankIn struct {
	Damping       float64 `json:"damping"`
	Tolerance     float64 `json:"tolerance"`
	MaxIterations uint32  `json:"max_iterations"`
	TopN          uint32  `json:"top_n"`
}

type componentsIn struct {
	Strong bool `json:"strong"`
}

type sccIn struct{}

type kcoreIn struct {
	K uint32 `json:"k"`
}

// analysisIn mirrors graphv1.RunAnalysisRequest in JSON (spec 22.9).
type analysisIn struct {
	ProgramID       string        `json:"program_id"`
	Bfs             *bfsIn        `json:"bfs"`
	Shortest        *shortestIn   `json:"shortest"`
	PageRank        *pagerankIn   `json:"pagerank"`
	Components      *componentsIn `json:"components"`
	Scc             *sccIn        `json:"scc"`
	KCore           *kcoreIn      `json:"kcore"`
	MinWatermarkSeq uint64        `json:"min_watermark_seq"`
}

func (a *analysisIn) proto(tenant, program string) (*graphv1.RunAnalysisRequest, error) {
	req := &graphv1.RunAnalysisRequest{TenantId: tenant, ProgramId: program, MinWatermarkSeq: a.MinWatermarkSeq}
	n := 0
	if b := a.Bfs; b != nil {
		n++
		dir, err := enumValue("bfs.direction", b.Direction, "DIRECTION_", graphv1.Direction_value)
		if err != nil {
			return nil, err
		}
		if _, err := bodyID("bfs.start_entity_id", b.StartEntityID, core.PrefixEntity, true); err != nil {
			return nil, err
		}
		req.Algorithm = &graphv1.RunAnalysisRequest_Bfs{Bfs: &graphv1.Bfs{StartEntityId: b.StartEntityID, MaxDepth: b.MaxDepth,
			Direction: graphv1.Direction(dir), Labels: b.Labels}}
	}
	if s := a.Shortest; s != nil {
		n++
		if _, err := bodyID("shortest.from_entity_id", s.FromEntityID, core.PrefixEntity, true); err != nil {
			return nil, err
		}
		if _, err := bodyID("shortest.to_entity_id", s.ToEntityID, core.PrefixEntity, true); err != nil {
			return nil, err
		}
		req.Algorithm = &graphv1.RunAnalysisRequest_Shortest{Shortest: &graphv1.ShortestPath{FromEntityId: s.FromEntityID, ToEntityId: s.ToEntityID, Labels: s.Labels}}
	}
	if p := a.PageRank; p != nil {
		n++
		if p.Damping < 0 || p.Damping >= 1 || p.Tolerance < 0 {
			return nil, errValidation("pagerank: damping must be in [0, 1) and tolerance non-negative")
		}
		req.Algorithm = &graphv1.RunAnalysisRequest_Pagerank{Pagerank: &graphv1.PageRank{Damping: p.Damping, Tolerance: p.Tolerance, MaxIterations: p.MaxIterations, TopN: p.TopN}}
	}
	if c := a.Components; c != nil {
		n++
		req.Algorithm = &graphv1.RunAnalysisRequest_Components{Components: &graphv1.Components{Strong: c.Strong}}
	}
	if a.Scc != nil {
		n++
		req.Algorithm = &graphv1.RunAnalysisRequest_Scc{Scc: &graphv1.Scc{}}
	}
	if k := a.KCore; k != nil {
		n++
		req.Algorithm = &graphv1.RunAnalysisRequest_Kcore{Kcore: &graphv1.KCore{K: k.K}}
	}
	if n != 1 {
		return nil, errValidation("exactly one of bfs, shortest, pagerank, components, scc, kcore is required")
	}
	return req, nil
}

func analysisJSON(r *graphv1.RunAnalysisResponse) map[string]any {
	m := map[string]any{"watermark_seq": r.GetWatermarkSeq(), "node_count": r.GetNodeCount(), "edge_count": r.GetEdgeCount(),
		"next_page_token": nextToken(r.GetNextPageToken())}
	switch res := r.GetResult().(type) {
	case *graphv1.RunAnalysisResponse_Levels:
		lv := []map[string]any{}
		for _, l := range res.Levels.GetLevels() {
			lv = append(lv, map[string]any{"depth": l.GetDepth(), "entity_ids": strs(l.GetEntityIds())})
		}
		m["levels"] = lv
	case *graphv1.RunAnalysisResponse_Path:
		m["path"] = map[string]any{"entity_ids": strs(res.Path.GetEntityIds()), "edge_ids": strs(res.Path.GetEdgeIds()), "confidence": res.Path.GetConfidence()}
	case *graphv1.RunAnalysisResponse_Scores:
		top := []map[string]any{}
		for _, s := range res.Scores.GetTopN() {
			top = append(top, map[string]any{"entity_id": s.GetEntityId(), "score": s.GetScore()})
		}
		m["scores"] = map[string]any{"top_n": top, "iterations": res.Scores.GetIterations(), "converged": res.Scores.GetConverged()}
	case *graphv1.RunAnalysisResponse_Groups:
		gs := []map[string]any{}
		for _, g := range res.Groups.GetGroups() {
			gs = append(gs, map[string]any{"id": g.GetId(), "entity_ids": strs(g.GetEntityIds())})
		}
		m["groups"] = gs
	}
	return m
}

func snapshotJSON(programID string, s *graphv1.SnapshotInfo) map[string]any {
	return map[string]any{"program_id": programID, "watermark_seq": s.GetWatermarkSeq(), "node_count": s.GetNodeCount(),
		"edge_count": s.GetEdgeCount(), "built_at": ts(s.GetBuiltAt()), "sha256": s.GetSha256(), "overlay_events": s.GetOverlayEvents()}
}

func indexInfoJSON(i *similarityv1.IndexInfo) map[string]any {
	return map[string]any{"model_id": i.GetModelId(), "model_version": i.GetModelVersion(), "dimension": i.GetDimension(),
		"metric": nonEmpty(enumName(i.GetMetric().String(), "METRIC_")), "vectors": i.GetVectors(), "tombstones": i.GetTombstones(),
		"segments": i.GetSegments(), "memtable_vectors": i.GetMemtableVectors(), "ram_bytes": i.GetRamBytes(),
		"disk_bytes": i.GetDiskBytes(), "watermark_seq": i.GetWatermarkSeq()}
}

// ---- alerts ------------------------------------------------------------

func alertRuleJSON(r *pb.AlertRule) map[string]any {
	return map[string]any{"id": r.GetId(), "program_id": nonEmpty(r.GetProgramId()), "name": r.GetName(), "source": r.GetSource(),
		"enabled": r.GetEnabled(), "version": r.GetVersion(), "diagnostics": diagnostics(r.GetDiagnostics())}
}

func channelJSON(c *pb.Channel) map[string]any {
	return map[string]any{"id": c.GetId(), "kind": c.GetKind(), "name": c.GetName(), "config": structOrEmpty(c.GetConfig()), "enabled": c.GetEnabled()}
}

func alertEventJSON(e *pb.AlertEvent) map[string]any {
	return map[string]any{"id": e.GetId(), "rule_id": e.GetRuleId(), "status": e.GetStatus(), "payload": structOrEmpty(e.GetPayload()),
		"created_at": ts(e.GetCreatedAt())}
}

// changeIn is a graph change event in a request (alert rule tests).
type changeIn struct {
	ID        string         `json:"id"`
	Seq       int64          `json:"seq"`
	ProgramID string         `json:"program_id"`
	Type      string         `json:"type"`
	EntityID  string         `json:"entity_id"`
	EdgeID    string         `json:"edge_id"`
	RunID     string         `json:"run_id"`
	Patch     map[string]any `json:"patch"`
	Entity    *entityIn      `json:"entity"`
	At        string         `json:"at"`
}

type entityIn struct {
	ID          string         `json:"id"`
	AssetType   string         `json:"asset_type"`
	Key         string         `json:"key"`
	Content     map[string]any `json:"content"`
	ScopeStatus string         `json:"scope_status"`
	Confidence  float64        `json:"confidence"`
	State       string         `json:"state"`
}

func (c *changeIn) proto() (*pb.ChangeEvent, error) {
	patch, err := toStruct("change.patch", c.Patch)
	if err != nil {
		return nil, err
	}
	ev := &pb.ChangeEvent{Id: c.ID, Seq: c.Seq, ProgramId: c.ProgramID, Type: c.Type, EntityId: c.EntityID, EdgeId: c.EdgeID, RunId: c.RunID, Patch: patch}
	if c.At != "" {
		t, err := core.ParseTime(c.At)
		if err != nil {
			return nil, errValidation("change.at: must be an RFC 3339 timestamp")
		}
		ev.At = timestamppb.New(t)
	}
	if e := c.Entity; e != nil {
		content, err := toStruct("change.entity.content", e.Content)
		if err != nil {
			return nil, err
		}
		ev.Entity = &pb.Entity{Id: e.ID, AssetType: e.AssetType, Key: e.Key, Content: content, ScopeStatus: e.ScopeStatus, Confidence: e.Confidence, State: e.State}
	}
	return ev, nil
}
