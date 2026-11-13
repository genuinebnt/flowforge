package gateway

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
)

// ---- programs and scope (12.5) -----------------------------------------

func (s *Server) listPrograms(c *call) error {
	size, tok, err := c.page()
	if err != nil {
		return err
	}
	res, err := s.cfg.Backends.Scope.ListPrograms(c.ctx, &pb.ListProgramsRequest{Page: &pb.PageRequest{PageSize: size, PageToken: tok}})
	if err != nil {
		return err
	}
	items := []programOut{}
	for _, p := range res.GetPrograms() {
		items = append(items, programJSON(p))
	}
	return c.ok(listOf(items, res.GetPage().GetNextPageToken()))
}

func checkName(field, v string, max int) error {
	if n := utf8.RuneCountInString(strings.TrimSpace(v)); n < 1 || n > max {
		return errValidation("%s: must be 1-%d characters", field, max)
	}
	return nil
}

func (s *Server) createProgram(c *call) error {
	var in struct {
		Name     string      `json:"name"`
		Platform string      `json:"platform"`
		Scope    *scopeDocIO `json:"scope"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if err := checkName("name", in.Name, 200); err != nil {
		return err
	}
	doc, err := scopeDocProto("scope", in.Scope)
	if err != nil {
		return err
	}
	p, err := s.cfg.Backends.Scope.CreateProgram(c.ctx, &pb.CreateProgramRequest{Name: strings.TrimSpace(in.Name), Platform: in.Platform, InitialScope: doc})
	if err != nil {
		return err
	}
	c.w.Header().Set("Location", "/v1/programs/"+p.GetId())
	return c.json(http.StatusCreated, programJSON(p))
}

func (s *Server) program(c *call) (*pb.Program, error) {
	id, err := c.pathID("id", core.PrefixProgram)
	if err != nil {
		return nil, err
	}
	return s.cfg.Backends.Scope.GetProgram(c.ctx, &pb.GetProgramRequest{ProgramId: id})
}

func (s *Server) getProgram(c *call) error {
	p, err := s.program(c)
	if err != nil {
		return err
	}
	out := programJSON(p)
	c.w.Header().Set("ETag", etag(out))
	return c.ok(out)
}

// patchProgram changes a program's status. The scope contract has no
// program update RPC, so "paused" and "active" map to the program-level
// kill switch; other fields are rejected as unknown.
func (s *Server) patchProgram(c *call) error {
	var in struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if in.Status != "active" && in.Status != "paused" {
		return errValidation("status: must be active or paused")
	}
	p, err := s.program(c)
	if err != nil {
		return err
	}
	if err := c.checkIfMatch(programJSON(p)); err != nil {
		return err
	}
	if _, err := s.cfg.Backends.Scope.SetKillSwitch(c.ctx, &pb.SetKillSwitchRequest{
		Level: "program", TargetId: p.GetId(), Enabled: in.Status == "paused", Reason: in.Reason,
	}); err != nil {
		return err
	}
	if p, err = s.program(c); err != nil {
		return err
	}
	out := programJSON(p)
	c.w.Header().Set("ETag", etag(out))
	return c.ok(out)
}

func (s *Server) getScope(c *call) error {
	id, err := c.pathID("id", core.PrefixProgram)
	if err != nil {
		return err
	}
	v, err := c.queryInt("version", 0, 0, 1<<31-1)
	if err != nil {
		return err
	}
	sv, err := s.cfg.Backends.Scope.GetScope(c.ctx, &pb.GetScopeRequest{ProgramId: id, Version: int32(v)})
	if err != nil {
		return err
	}
	return c.ok(scopeVersionJSON(sv, true))
}

func (s *Server) putScope(c *call) error {
	id, err := c.pathID("id", core.PrefixProgram)
	if err != nil {
		return err
	}
	var in struct {
		BaseVersion   *int32      `json:"base_version"`
		EffectiveFrom string      `json:"effective_from"`
		Document      *scopeDocIO `json:"document"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if in.BaseVersion == nil || *in.BaseVersion < 0 {
		return errValidation("base_version: required (the version this edit is based on)")
	}
	doc, err := scopeDocProto("document", in.Document)
	if err != nil {
		return err
	}
	req := &pb.PutScopeRequest{ProgramId: id, BaseVersion: *in.BaseVersion, Document: doc}
	if in.EffectiveFrom != "" {
		t, err := core.ParseTime(in.EffectiveFrom)
		if err != nil {
			return errValidation("effective_from: must be an RFC 3339 timestamp")
		}
		req.EffectiveFrom = pbTime(t)
	}
	sv, err := s.cfg.Backends.Scope.PutScope(c.ctx, req)
	if err != nil {
		return err
	}
	return c.ok(map[string]any{"program_id": sv.GetProgramId(), "version": sv.GetVersion(), "effective_from": ts(sv.GetEffectiveFrom()),
		"created_by": publicActor(sv.GetCreatedBy()), "created_at": ts(sv.GetCreatedAt()), "paused_runs": nil})
}

func (s *Server) listScopeVersions(c *call) error {
	id, err := c.pathID("id", core.PrefixProgram)
	if err != nil {
		return err
	}
	size, tok, err := c.page()
	if err != nil {
		return err
	}
	res, err := s.cfg.Backends.Scope.ListScopeVersions(c.ctx, &pb.ListScopeVersionsRequest{ProgramId: id, Page: &pb.PageRequest{PageSize: size, PageToken: tok}})
	if err != nil {
		return err
	}
	items := []scopeVersionOut{}
	for _, v := range res.GetVersions() {
		items = append(items, scopeVersionJSON(v, false))
	}
	return c.ok(listOf(items, res.GetPage().GetNextPageToken()))
}

func (s *Server) checkScope(c *call) error {
	id, err := c.pathID("id", core.PrefixProgram)
	if err != nil {
		return err
	}
	var in struct {
		Target        string `json:"target"`
		Kind          string `json:"kind"`
		ActivityClass string `json:"activity_class"`
		ScopeVersion  int32  `json:"scope_version"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if in.Target == "" || len(in.Target) > 4096 {
		return errValidation("target: required, at most 4096 bytes")
	}
	kind, err := enumValue("kind", in.Kind, "TARGET_KIND_", pb.TargetKind_value)
	if err != nil {
		return err
	}
	if kind == 0 {
		return errValidation("kind: required (host, ip, url or cidr)")
	}
	class, err := parseActivity("activity_class", in.ActivityClass)
	if err != nil {
		return err
	}
	if class == pb.ActivityClass_ACTIVITY_CLASS_UNSPECIFIED {
		class = pb.ActivityClass_ACTIVITY_CLASS_PASSIVE
	}
	if in.ScopeVersion < 0 {
		return errValidation("scope_version: must not be negative")
	}
	d, err := s.cfg.Backends.Scope.Check(c.ctx, &pb.CheckRequest{ProgramId: id, ScopeVersion: in.ScopeVersion, Kind: pb.TargetKind(kind),
		Target: in.Target, ActivityClass: class})
	if err != nil {
		return err
	}
	return c.ok(decisionJSON(d))
}

func (s *Server) killSwitch(c *call) error {
	id, err := c.pathID("id", core.PrefixProgram)
	if err != nil {
		return err
	}
	var in struct {
		Enabled *bool  `json:"enabled"`
		Reason  string `json:"reason"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if in.Enabled == nil {
		return errValidation("enabled: required")
	}
	if *in.Enabled && strings.TrimSpace(in.Reason) == "" {
		return errValidation("reason: required when enabling the kill switch")
	}
	k, err := s.cfg.Backends.Scope.SetKillSwitch(c.ctx, &pb.SetKillSwitchRequest{Level: "program", TargetId: id, Enabled: *in.Enabled, Reason: in.Reason})
	if err != nil {
		return err
	}
	return c.ok(killSwitchJSON(k))
}

// ---- workflows and tools ----------------------------------------------

var workflowNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_\-]{0,127}$`)

func checkSource(src string) error {
	if strings.TrimSpace(src) == "" {
		return errValidation("source: required")
	}
	if len(src) > 512<<10 {
		return errValidation("source: larger than 512 KiB")
	}
	return nil
}

func (s *Server) checkWorkflow(c *call) error {
	var in struct {
		ProgramID string `json:"program_id"`
		Source    string `json:"source"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if _, err := bodyID("program_id", in.ProgramID, core.PrefixProgram, false); err != nil {
		return err
	}
	if err := checkSource(in.Source); err != nil {
		return err
	}
	res, err := s.cfg.Backends.Engine.CheckWorkflow(c.ctx, &pb.CheckWorkflowRequest{Source: in.Source, ProgramId: in.ProgramID})
	if err != nil {
		return err
	}
	return c.ok(map[string]any{"ok": res.GetOk(), "inferred_effects": strs(res.GetInferredEffects()),
		"inferred_class": activityName(res.GetInferredClass()), "diagnostics": diagnostics(res.GetDiagnostics())})
}

func (s *Server) registerWorkflow(c *call) error {
	var in struct {
		Name      string `json:"name"`
		ProgramID string `json:"program_id"`
		Source    string `json:"source"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if !workflowNameRE.MatchString(in.Name) {
		return errValidation("name: must be an identifier (letters, digits, _ and -)")
	}
	if _, err := bodyID("program_id", in.ProgramID, core.PrefixProgram, false); err != nil {
		return err
	}
	if err := checkSource(in.Source); err != nil {
		return err
	}
	w, err := s.cfg.Backends.Engine.RegisterWorkflow(c.ctx, &pb.RegisterWorkflowRequest{Name: in.Name, Source: in.Source, ProgramId: in.ProgramID})
	if err != nil {
		return err
	}
	c.w.Header().Set("Location", "/v1/workflows/"+w.GetName()+"/versions/"+strconv.Itoa(int(w.GetVersion())))
	return c.json(http.StatusCreated, workflowJSON(w, false))
}

func (s *Server) listWorkflows(c *call) error {
	size, tok, err := c.page()
	if err != nil {
		return err
	}
	res, err := s.cfg.Backends.Engine.ListWorkflows(c.ctx, &pb.ListWorkflowsRequest{Page: &pb.PageRequest{PageSize: size, PageToken: tok}})
	if err != nil {
		return err
	}
	items := []map[string]any{}
	for _, w := range res.GetLatest() {
		items = append(items, workflowJSON(w, false))
	}
	return c.ok(listOf(items, res.GetPage().GetNextPageToken()))
}

func (s *Server) getWorkflowVersion(c *call) error {
	name := c.r.PathValue("name")
	if !workflowNameRE.MatchString(name) {
		return errNotFound()
	}
	vs := c.r.PathValue("v")
	v := 0
	if vs != "latest" {
		n, err := strconv.Atoi(vs)
		if err != nil || n < 1 || n > 1<<31-1 {
			return errNotFound()
		}
		v = n
	}
	w, err := s.cfg.Backends.Engine.GetWorkflowVersion(c.ctx, &pb.GetWorkflowVersionRequest{Name: name, Version: int32(v)})
	if err != nil {
		return err
	}
	return c.ok(workflowJSON(w, true))
}

func (s *Server) listTools(c *call) error {
	if _, _, err := c.page(); err != nil {
		return err
	}
	res, err := s.cfg.Backends.ToolRunner.ListTools(c.ctx, &pb.ListToolsRequest{})
	if err != nil {
		return err
	}
	items := []map[string]any{}
	for _, t := range res.GetTools() {
		items = append(items, toolJSON(t))
	}
	return c.ok(listOf(items, ""))
}

// ---- runs --------------------------------------------------------------

func (s *Server) submitRun(c *call) error {
	var in struct {
		Workflow   string         `json:"workflow"`
		Version    int32          `json:"version"`
		ProgramID  string         `json:"program_id"`
		Input      map[string]any `json:"input"`
		Priority   int32          `json:"priority"`
		DeadlineMS int64          `json:"deadline_ms"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if !workflowNameRE.MatchString(in.Workflow) {
		return errValidation("workflow: required workflow name")
	}
	if in.Version < 0 {
		return errValidation("version: must not be negative (0 = latest published)")
	}
	if _, err := bodyID("program_id", in.ProgramID, core.PrefixProgram, true); err != nil {
		return err
	}
	if in.DeadlineMS < 0 || in.DeadlineMS > int64(30*24*time.Hour/time.Millisecond) {
		return errValidation("deadline_ms: must be from 0 to 30 days")
	}
	if in.Input == nil {
		in.Input = map[string]any{}
	}
	input, err := toStruct("input", in.Input)
	if err != nil {
		return err
	}
	req := &pb.SubmitRunRequest{WorkflowName: in.Workflow, WorkflowVersion: in.Version, ProgramId: in.ProgramID, Input: input, Priority: in.Priority}
	if in.DeadlineMS > 0 {
		req.Deadline = durationpb.New(time.Duration(in.DeadlineMS) * time.Millisecond)
	}
	run, err := s.cfg.Backends.Engine.SubmitRun(c.ctx, req)
	if err != nil {
		return err
	}
	c.w.Header().Set("Location", "/v1/runs/"+run.GetId())
	return c.json(http.StatusAccepted, runJSON(run))
}

func (s *Server) listRuns(c *call) error {
	size, tok, err := c.page()
	if err != nil {
		return err
	}
	q := c.r.URL.Query()
	st, err := parseRunStatus(q.Get("status"))
	if err != nil {
		return err
	}
	pid, err := bodyID("program_id", q.Get("program_id"), core.PrefixProgram, false)
	if err != nil {
		return err
	}
	after, err := c.queryTime("created_after")
	if err != nil {
		return err
	}
	res, err := s.cfg.Backends.Engine.ListRuns(c.ctx, &pb.ListRunsRequest{Page: &pb.PageRequest{PageSize: size, PageToken: tok},
		ProgramId: pid, Status: st, WorkflowName: q.Get("workflow"), CreatedAfter: pbTime(after)})
	if err != nil {
		return err
	}
	items := []map[string]any{}
	for _, r := range res.GetRuns() {
		items = append(items, runJSON(r))
	}
	return c.ok(listOf(items, res.GetPage().GetNextPageToken()))
}

func (s *Server) getRun(c *call) error {
	id, err := c.pathID("id", core.PrefixRun)
	if err != nil {
		return err
	}
	run, err := s.cfg.Backends.Engine.GetRun(c.ctx, &pb.GetRunRequest{RunId: id})
	if err != nil {
		return err
	}
	out := runJSON(run)
	if run.GetStatus() == pb.RunStatus_RUN_STATUS_WAITING {
		// The run contract has no waiting_on; derive it from the pending
		// approval, if any (best effort).
		if aps, err := s.cfg.Backends.Engine.ListApprovals(c.ctx, &pb.ListApprovalsRequest{RunId: id, Status: "pending", Page: &pb.PageRequest{PageSize: 1}}); err == nil && len(aps.GetApprovals()) > 0 {
			a := aps.GetApprovals()[0]
			out["waiting_on"] = map[string]any{"kind": "approval", "approval_id": a.GetId(), "expires_at": ts(a.GetExpiresAt())}
		}
	}
	return c.ok(out)
}

func (s *Server) cancelRun(c *call) error {
	id, err := c.pathID("id", core.PrefixRun)
	if err != nil {
		return err
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := c.decodeOptional(&in); err != nil {
		return err
	}
	run, err := s.cfg.Backends.Engine.CancelRun(c.ctx, &pb.CancelRunRequest{RunId: id, Reason: in.Reason})
	if err != nil {
		return err
	}
	return c.ok(runJSON(run))
}

func (s *Server) listRunEvents(c *call) error {
	id, err := c.pathID("id", core.PrefixRun)
	if err != nil {
		return err
	}
	size, tok, err := c.page()
	if err != nil {
		return err
	}
	after, err := c.queryInt("after_seq", 0, 0, 1<<62)
	if err != nil {
		return err
	}
	res, err := s.cfg.Backends.Engine.ListRunEvents(c.ctx, &pb.ListRunEventsRequest{RunId: id, AfterSeq: after,
		Page: &pb.PageRequest{PageSize: size, PageToken: tok}, Types: c.queryList("types")})
	if err != nil {
		return err
	}
	items := []map[string]any{}
	for _, e := range res.GetEvents() {
		items = append(items, runEventJSON(e))
	}
	return c.ok(listOf(items, res.GetPage().GetNextPageToken()))
}

// ---- approvals ---------------------------------------------------------

func (s *Server) listApprovals(c *call) error {
	size, tok, err := c.page()
	if err != nil {
		return err
	}
	q := c.r.URL.Query()
	rid, err := bodyID("run_id", q.Get("run_id"), core.PrefixRun, false)
	if err != nil {
		return err
	}
	res, err := s.cfg.Backends.Engine.ListApprovals(c.ctx, &pb.ListApprovalsRequest{Page: &pb.PageRequest{PageSize: size, PageToken: tok},
		Status: q.Get("status"), Role: q.Get("role"), RunId: rid})
	if err != nil {
		return err
	}
	items := []map[string]any{}
	for _, a := range res.GetApprovals() {
		items = append(items, approvalJSON(a))
	}
	return c.ok(listOf(items, res.GetPage().GetNextPageToken()))
}

func (s *Server) getApproval(c *call) error {
	id, err := c.pathID("id", core.PrefixApproval)
	if err != nil {
		return err
	}
	a, err := s.cfg.Backends.Engine.GetApproval(c.ctx, &pb.GetApprovalRequest{ApprovalId: id})
	if err != nil {
		return err
	}
	return c.ok(approvalJSON(a))
}

// decideApproval also requires the approval's role to match the caller's
// membership role or one of their role groups (spec 9.7, 13.6; G-LIM-05).
// The groups travel in the internal token, and the engine re-checks the
// match and the self-approval rule.
func (s *Server) decideApproval(c *call) error {
	id, err := c.pathID("id", core.PrefixApproval)
	if err != nil {
		return err
	}
	var in struct {
		Decision string `json:"decision"`
		Comment  string `json:"comment"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	var d pb.ApprovalDecision
	switch in.Decision {
	case "approve":
		d = pb.ApprovalDecision_APPROVAL_DECISION_APPROVE
	case "deny":
		d = pb.ApprovalDecision_APPROVAL_DECISION_DENY
	default:
		return errValidation("decision: must be approve or deny")
	}
	if len(in.Comment) > 4096 {
		return errValidation("comment: at most 4096 bytes")
	}
	groups, err := c.loadGroups()
	if err != nil {
		return err
	}
	a, err := s.cfg.Backends.Engine.GetApproval(c.ctx, &pb.GetApprovalRequest{ApprovalId: id})
	if err != nil {
		return err
	}
	if !auth.CanDecideApproval(c.p.Role, a.GetRole(), groups) {
		return newError(http.StatusForbidden, core.ReasonForbiddenRole, "approval requires role %q", a.GetRole())
	}
	a, err = s.cfg.Backends.Engine.DecideApproval(c.ctx, &pb.DecideApprovalRequest{ApprovalId: id, Decision: d, Comment: in.Comment})
	if err != nil {
		return err
	}
	return c.ok(map[string]any{"id": a.GetId(), "status": a.GetStatus(), "decided_by": nonEmpty(publicActor(a.GetDecidedBy())),
		"decided_at": ts(a.GetDecidedAt()), "run_id": a.GetRunId(), "comment": nonEmpty(a.GetComment())})
}
