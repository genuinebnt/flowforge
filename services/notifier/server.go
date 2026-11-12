package notifier

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// Server implements flowforge.v1.NotifierService.
type Server struct {
	pb.UnimplementedNotifierServiceServer
	S *Service
}

func repoErr(what string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, ErrNotFound):
		return core.Errorf(core.KindNotFound, core.ReasonNotFound, "%s not found", what)
	case errors.Is(err, ErrExists):
		return core.Errorf(core.KindAlreadyExists, core.ReasonAlreadyExists, "%s with that name already exists", what)
	}
	return &core.Error{Kind: core.KindUnavailable, Reason: core.ReasonUnavailable, Message: what + ": storage unavailable", Err: err}
}

func diagsProto(ds []alerts.Diagnostic) []*pb.Diagnostic { return convert.DiagnosticsFromJSON(ds) }

func ruleProto(r Rule, ds []alerts.Diagnostic) *pb.AlertRule {
	return &pb.AlertRule{Id: convert.ID(core.PrefixAlertRule, r.ID), ProgramId: convert.ID(core.PrefixProgram, r.Program), Name: r.Name,
		Source: r.Source, Enabled: r.Enabled, Version: r.Version, Diagnostics: diagsProto(ds)}
}

// PutRule implements NotifierService: the source is compiled (spec 15.2);
// a rule with error diagnostics may be stored disabled but never enabled
// (spec 15.3): enabling one fails with INVALID_ARGUMENT DSL_DIAGNOSTICS.
func (s *Server) PutRule(ctx context.Context, req *pb.PutRuleRequest) (*pb.AlertRule, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	rule, diags := alerts.Compile(req.GetSource())
	if alerts.HasErrors(diags) && req.GetEnabled() {
		e := core.Errorf(core.KindInvalidArgument, core.ReasonDSLDiagnostics, "the rule does not compile (%d diagnostics); it cannot be enabled", len(diags))
		for _, d := range diagsProto(diags) {
			e.Details = append(e.Details, d)
		}
		return nil, e
	}
	prog, err := convert.OptionalID(core.PrefixProgram, "program_id", req.GetProgramId())
	if err != nil {
		return nil, err
	}
	name := req.GetName()
	if name == "" && rule != nil {
		name = rule.Name
	}
	if name == "" {
		return nil, convert.Invalid("name", "name is required")
	}
	r := Rule{Tenant: id.Tenant, Program: prog, Name: name, Source: req.GetSource(), Enabled: req.GetEnabled(),
		CreatedBy: id.Actor, CreatedAt: s.S.o.Clock.Now().UTC()}
	if rule != nil {
		r.Compiled, _ = json.Marshal(map[string]any{"name": rule.Name, "actions": rule.Actions, "severity": rule.Severity,
			"throttle_ns": rule.Throttle, "digest_ns": rule.Digest, "group_by": rule.GroupBy})
	}
	create := req.GetId() == ""
	if create {
		r.ID = s.S.newID()
	} else if r.ID, err = convert.ParseID(core.PrefixAlertRule, "id", req.GetId()); err != nil {
		return nil, err
	}
	r, err = s.S.o.Repo.PutRule(ctx, r, create)
	if err != nil {
		return nil, repoErr("rule", err)
	}
	s.S.Invalidate(id.Tenant)
	return ruleProto(r, diags), nil
}

// GetRule implements NotifierService.
func (s *Server) GetRule(ctx context.Context, req *pb.GetRuleRequest) (*pb.AlertRule, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	rid, err := convert.ParseID(core.PrefixAlertRule, "id", req.GetId())
	if err != nil {
		return nil, err
	}
	r, err := s.S.o.Repo.GetRule(ctx, id.Tenant, rid)
	if err != nil {
		return nil, repoErr("rule", err)
	}
	_, diags := alerts.Compile(r.Source)
	return ruleProto(r, diags), nil
}

// ListRules implements NotifierService.
func (s *Server) ListRules(ctx context.Context, req *pb.ListRulesRequest) (*pb.ListRulesResponse, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	n, tok, err := convert.Page(req.GetPage())
	if err != nil {
		return nil, err
	}
	after, err := convert.OptionalID(core.PrefixAlertRule, "page.page_token", tok)
	if err != nil {
		return nil, err
	}
	rs, err := s.S.o.Repo.ListRules(ctx, id.Tenant, after, n+1)
	if err != nil {
		return nil, repoErr("rules", err)
	}
	out := &pb.ListRulesResponse{}
	for i, r := range rs {
		if i == n {
			out.Page = convert.PageResp(convert.ID(core.PrefixAlertRule, rs[n-1].ID))
			break
		}
		out.Rules = append(out.Rules, ruleProto(r, nil))
	}
	return out, nil
}

// DeleteRule implements NotifierService.
func (s *Server) DeleteRule(ctx context.Context, req *pb.DeleteRuleRequest) (*pb.DeleteRuleResponse, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	rid, err := convert.ParseID(core.PrefixAlertRule, "id", req.GetId())
	if err != nil {
		return nil, err
	}
	if err := s.S.o.Repo.DeleteRule(ctx, id.Tenant, rid); err != nil {
		return nil, repoErr("rule", err)
	}
	s.S.Invalidate(id.Tenant)
	return &pb.DeleteRuleResponse{}, nil
}

// TestRule implements NotifierService: diagnostics, and for a sample change
// whether the rule matches and the payload it would send. Historical events
// are not retained by this implementation (FAILED_PRECONDITION).
func (s *Server) TestRule(ctx context.Context, req *pb.TestRuleRequest) (*pb.TestRuleResponse, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	rule, diags := alerts.Compile(req.GetSource())
	out := &pb.TestRuleResponse{Diagnostics: diagsProto(diags)}
	if rule == nil {
		return out, nil
	}
	if req.GetHistoricalEventId() != "" {
		return nil, core.Errorf(core.KindFailedPrecondition, core.ReasonValidationFailed, "historical events are not retained; send a sample change")
	}
	c := req.GetChange()
	if c == nil {
		return out, nil
	}
	ev := EventFromEnvelope(&pb.EventEnvelope{EventId: c.GetId(), Type: "graph." + c.GetType(), TenantId: id.TenantPublicID(),
		ProgramId: c.GetProgramId(), OccurredAt: c.GetAt(), Payload: &pb.EventEnvelope_GraphChange{GraphChange: c}})
	if ev.ID == "" {
		ev.ID = "test"
	}
	m, ok, err := alerts.EvaluateRule("alr_test", rule, ev, s.S.o.Render)
	if err != nil {
		return nil, convert.Invalid("change", "evaluation failed: %v", err)
	}
	out.Matched = ok
	if ok {
		b, _ := json.Marshal(m.Payload)
		out.RenderedPayload = convert.MustStruct(b)
	}
	return out, nil
}

// PutChannel implements NotifierService. Config holds secret references
// only; webhook URLs are validated (https, not internal) when stored.
func (s *Server) PutChannel(ctx context.Context, req *pb.PutChannelRequest) (*pb.Channel, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	cfg := convert.StructToMap(req.GetConfig())
	switch req.GetKind() {
	case KindWebhook:
		u, _ := cfg["url"].(string)
		if _, err := alerts.ValidateWebhookURL(u); err != nil && !(s.S.o.Senders != nil && s.S.o.Senders.AllowInsecureLoopback) {
			return nil, convert.Invalid("config.url", "%v", err)
		}
		if ref, _ := cfg["secret_ref"].(string); ref == "" {
			return nil, convert.Invalid("config.secret_ref", "a webhook needs secret_ref")
		}
	case KindSlack:
		if ref, _ := cfg["webhook_secret_ref"].(string); ref == "" {
			return nil, convert.Invalid("config.webhook_secret_ref", "a slack channel needs webhook_secret_ref")
		}
	case KindEmail:
	default:
		return nil, convert.Invalid("kind", "kind must be slack, email or webhook")
	}
	if req.GetName() == "" {
		return nil, convert.Invalid("name", "name is required")
	}
	c := Channel{Tenant: id.Tenant, Kind: req.GetKind(), Name: req.GetName(), Config: cfg, Enabled: req.GetEnabled()}
	if req.GetId() == "" {
		c.ID = s.S.newID()
	} else if c.ID, err = convert.ParseID(core.PrefixChannel, "id", req.GetId()); err != nil {
		return nil, err
	}
	c, err = s.S.o.Repo.PutChannel(ctx, c)
	if err != nil {
		return nil, repoErr("channel", err)
	}
	return channelProto(c), nil
}

func channelProto(c Channel) *pb.Channel {
	return &pb.Channel{Id: convert.ID(core.PrefixChannel, c.ID), Kind: c.Kind, Name: c.Name, Config: convert.StructFromMap(c.Config), Enabled: c.Enabled}
}

// ListChannels implements NotifierService.
func (s *Server) ListChannels(ctx context.Context, _ *pb.ListChannelsRequest) (*pb.ListChannelsResponse, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	cs, err := s.S.o.Repo.ListChannels(ctx, id.Tenant)
	if err != nil {
		return nil, repoErr("channels", err)
	}
	out := &pb.ListChannelsResponse{}
	for _, c := range cs {
		out.Channels = append(out.Channels, channelProto(c))
	}
	return out, nil
}

// ListAlertEvents implements NotifierService.
func (s *Server) ListAlertEvents(ctx context.Context, req *pb.ListAlertEventsRequest) (*pb.ListAlertEventsResponse, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return nil, err
	}
	n, tok, err := convert.Page(req.GetPage())
	if err != nil {
		return nil, err
	}
	after, err := convert.OptionalID(core.PrefixAlertEvent, "page.page_token", tok)
	if err != nil {
		return nil, err
	}
	rule, err := convert.OptionalID(core.PrefixAlertRule, "rule_id", req.GetRuleId())
	if err != nil {
		return nil, err
	}
	as, err := s.S.o.Repo.ListAlertEvents(ctx, id.Tenant, rule, req.GetStatus(), after, n+1)
	if err != nil {
		return nil, repoErr("alert events", err)
	}
	out := &pb.ListAlertEventsResponse{}
	for i, a := range as {
		if i == n {
			out.Page = convert.PageResp(convert.ID(core.PrefixAlertEvent, as[n-1].ID))
			break
		}
		out.Events = append(out.Events, &pb.AlertEvent{Id: convert.ID(core.PrefixAlertEvent, a.ID), RuleId: convert.ID(core.PrefixAlertRule, a.RuleID),
			Status: a.Status, Payload: convert.MustStruct(a.Payload), CreatedAt: convert.TS(a.CreatedAt)})
	}
	return out, nil
}
