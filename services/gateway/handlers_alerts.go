package gateway

import (
	"context"
	"net/http"
	"sort"
	"time"

	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
)

// ---- alert rules -------------------------------------------------------

type ruleIn struct {
	Name      string `json:"name"`
	ProgramID string `json:"program_id"`
	Enabled   *bool  `json:"enabled"`
	Source    string `json:"source"`
}

func (in *ruleIn) proto(id string) (*pb.PutRuleRequest, error) {
	if err := checkName("name", in.Name, 200); err != nil {
		return nil, err
	}
	if _, err := bodyID("program_id", in.ProgramID, core.PrefixProgram, false); err != nil {
		return nil, err
	}
	if err := checkSource(in.Source); err != nil {
		return nil, err
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	return &pb.PutRuleRequest{Id: id, ProgramId: in.ProgramID, Name: in.Name, Source: in.Source, Enabled: enabled}, nil
}

func (s *Server) listRules(c *call) error {
	size, tok, err := c.page()
	if err != nil {
		return err
	}
	res, err := s.cfg.Backends.Notifier.ListRules(c.ctx, &pb.ListRulesRequest{Page: &pb.PageRequest{PageSize: size, PageToken: tok}})
	if err != nil {
		return err
	}
	items := []map[string]any{}
	for _, r := range res.GetRules() {
		items = append(items, alertRuleJSON(r))
	}
	return c.ok(listOf(items, res.GetPage().GetNextPageToken()))
}

func (s *Server) createRule(c *call) error {
	var in ruleIn
	if err := c.decode(&in); err != nil {
		return err
	}
	req, err := in.proto("")
	if err != nil {
		return err
	}
	r, err := s.cfg.Backends.Notifier.PutRule(c.ctx, req)
	if err != nil {
		return err
	}
	c.w.Header().Set("Location", "/v1/alerts/rules/"+r.GetId())
	return c.json(http.StatusCreated, alertRuleJSON(r))
}

func (s *Server) rule(c *call) (*pb.AlertRule, error) {
	id, err := c.pathID("id", core.PrefixAlertRule)
	if err != nil {
		return nil, err
	}
	return s.cfg.Backends.Notifier.GetRule(c.ctx, &pb.GetRuleRequest{Id: id})
}

func (s *Server) getRule(c *call) error {
	r, err := s.rule(c)
	if err != nil {
		return err
	}
	out := alertRuleJSON(r)
	c.w.Header().Set("ETag", etag(out))
	return c.ok(out)
}

func (s *Server) putRule(c *call) error {
	var in ruleIn
	if err := c.decode(&in); err != nil {
		return err
	}
	cur, err := s.rule(c)
	if err != nil {
		return err
	}
	if err := c.checkIfMatch(alertRuleJSON(cur)); err != nil {
		return err
	}
	req, err := in.proto(cur.GetId())
	if err != nil {
		return err
	}
	r, err := s.cfg.Backends.Notifier.PutRule(c.ctx, req)
	if err != nil {
		return err
	}
	out := alertRuleJSON(r)
	c.w.Header().Set("ETag", etag(out))
	return c.ok(out)
}

func (s *Server) deleteRule(c *call) error {
	id, err := c.pathID("id", core.PrefixAlertRule)
	if err != nil {
		return err
	}
	if _, err := s.cfg.Backends.Notifier.DeleteRule(c.ctx, &pb.DeleteRuleRequest{Id: id}); err != nil {
		return err
	}
	return c.noContent()
}

func (s *Server) testRule(c *call) error {
	var in struct {
		Source            string    `json:"source"`
		Change            *changeIn `json:"change"`
		HistoricalEventID string    `json:"historical_event_id"`
	}
	if err := c.decode(&in); err != nil {
		return err
	}
	if err := checkSource(in.Source); err != nil {
		return err
	}
	req := &pb.TestRuleRequest{Source: in.Source}
	switch {
	case in.Change != nil && in.HistoricalEventID != "":
		return errValidation("change and historical_event_id are mutually exclusive")
	case in.Change != nil:
		ev, err := in.Change.proto()
		if err != nil {
			return err
		}
		req.Sample = &pb.TestRuleRequest_Change{Change: ev}
	case in.HistoricalEventID != "":
		req.Sample = &pb.TestRuleRequest_HistoricalEventId{HistoricalEventId: in.HistoricalEventID}
	}
	res, err := s.cfg.Backends.Notifier.TestRule(c.ctx, req)
	if err != nil {
		return err
	}
	return c.ok(map[string]any{"matched": res.GetMatched(), "diagnostics": diagnostics(res.GetDiagnostics()),
		"rendered_payload": structJSON(res.GetRenderedPayload())})
}

// ---- channels and alert events -----------------------------------------

type channelIn struct {
	Kind    string         `json:"kind"`
	Name    string         `json:"name"`
	Config  map[string]any `json:"config"`
	Enabled *bool          `json:"enabled"`
}

func (in *channelIn) proto(id string) (*pb.PutChannelRequest, error) {
	switch in.Kind {
	case "slack", "webhook", "email", "pagerduty":
	default:
		return nil, errValidation("kind: must be slack, webhook, email or pagerduty")
	}
	if err := checkName("name", in.Name, 200); err != nil {
		return nil, err
	}
	cfg, err := toStruct("config", in.Config)
	if err != nil {
		return nil, err
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	return &pb.PutChannelRequest{Id: id, Kind: in.Kind, Name: in.Name, Config: cfg, Enabled: enabled}, nil
}

func (s *Server) listChannels(c *call) error {
	if _, _, err := c.page(); err != nil {
		return err
	}
	res, err := s.cfg.Backends.Notifier.ListChannels(c.ctx, &pb.ListChannelsRequest{})
	if err != nil {
		return err
	}
	items := []map[string]any{}
	for _, ch := range res.GetChannels() {
		items = append(items, channelJSON(ch))
	}
	return c.ok(listOf(items, ""))
}

func (s *Server) createChannel(c *call) error {
	var in channelIn
	if err := c.decode(&in); err != nil {
		return err
	}
	req, err := in.proto("")
	if err != nil {
		return err
	}
	ch, err := s.cfg.Backends.Notifier.PutChannel(c.ctx, req)
	if err != nil {
		return err
	}
	return c.json(http.StatusCreated, channelJSON(ch))
}

func (s *Server) putChannel(c *call) error {
	id, err := c.pathID("id", core.PrefixChannel)
	if err != nil {
		return err
	}
	var in channelIn
	if err := c.decode(&in); err != nil {
		return err
	}
	res, err := s.cfg.Backends.Notifier.ListChannels(c.ctx, &pb.ListChannelsRequest{})
	if err != nil {
		return err
	}
	var cur *pb.Channel
	for _, ch := range res.GetChannels() {
		if ch.GetId() == id {
			cur = ch
		}
	}
	if cur == nil {
		return errNotFound()
	}
	if err := c.checkIfMatch(channelJSON(cur)); err != nil {
		return err
	}
	req, err := in.proto(id)
	if err != nil {
		return err
	}
	ch, err := s.cfg.Backends.Notifier.PutChannel(c.ctx, req)
	if err != nil {
		return err
	}
	out := channelJSON(ch)
	c.w.Header().Set("ETag", etag(out))
	return c.ok(out)
}

func (s *Server) listAlertEvents(c *call) error {
	size, tok, err := c.page()
	if err != nil {
		return err
	}
	q := c.r.URL.Query()
	rid, err := bodyID("rule_id", q.Get("rule_id"), core.PrefixAlertRule, false)
	if err != nil {
		return err
	}
	res, err := s.cfg.Backends.Notifier.ListAlertEvents(c.ctx, &pb.ListAlertEventsRequest{Page: &pb.PageRequest{PageSize: size, PageToken: tok},
		RuleId: rid, Status: q.Get("status")})
	if err != nil {
		return err
	}
	items := []map[string]any{}
	for _, e := range res.GetEvents() {
		items = append(items, alertEventJSON(e))
	}
	return c.ok(listOf(items, res.GetPage().GetNextPageToken()))
}

// ---- probes and keys ---------------------------------------------------

func (s *Server) healthz(c *call) error { return c.ok(map[string]any{"status": "ok"}) }

func (s *Server) readyz(c *call) error {
	ctx, cancel := context.WithTimeout(c.ctx, 2*time.Second)
	defer cancel()
	checks := map[string]any{}
	ready := true
	names := make([]string, 0, len(s.cfg.Readiness)+1)
	for n := range s.cfg.Readiness {
		names = append(names, n)
	}
	sort.Strings(names)
	run := func(name string, f func(context.Context) error) {
		if err := f(ctx); err != nil {
			ready = false
			checks[name] = "failed"
			s.cfg.Log.WarnContext(ctx, "gateway: readiness check failed", "check", name, "err", err)
			return
		}
		checks[name] = "ok"
	}
	run("identity_store", s.cfg.Identity.Store().Ping)
	for _, n := range names {
		run(n, s.cfg.Readiness[n])
	}
	status := http.StatusOK
	state := "ready"
	if !ready {
		status, state = http.StatusServiceUnavailable, "not_ready"
	}
	return c.json(status, map[string]any{"status": state, "checks": checks})
}

func (s *Server) jwks(c *call) error {
	b, err := s.cfg.Identity.Keys().JWKS()
	if err != nil {
		return err
	}
	h := c.w.Header()
	h.Set("Cache-Control", "public, max-age=300")
	h.Set("Content-Type", "application/jwk-set+json")
	c.wrote = true
	c.w.WriteHeader(http.StatusOK)
	_, _ = c.w.Write(b)
	return nil
}
