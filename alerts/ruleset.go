package alerts

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/genuinebnt/flowforge/core"
)

// RuleSet errors.
var (
	ErrDuplicateRule = errors.New("alerts: duplicate rule id")
	ErrEmptyRuleID   = errors.New("alerts: empty rule id")
)

// RenderOptions shape rendered payloads.
type RenderOptions struct {
	// ConsoleBaseURL, if set, adds console links to payloads, e.g.
	// "https://console.example.com".
	ConsoleBaseURL string
}

// RuleSet holds the enabled rules of one tenant/program and evaluates
// events against them. It is safe for concurrent use.
type RuleSet struct {
	mu      sync.RWMutex
	opts    RenderOptions
	rules   []ruleEntry
	byType  map[string][]int // event type -> indexes into rules
	anyType []int            // rules not restricted to an event type
}

type ruleEntry struct {
	id   string
	rule *Rule
}

// NewRuleSet returns an empty rule set.
func NewRuleSet(opts RenderOptions) *RuleSet {
	return &RuleSet{opts: opts, byType: map[string][]int{}}
}

// Add enables a compiled rule under its storage id (alr_...). Rule ids
// feed the dedupe key, so they must be stable across versions.
func (s *RuleSet) Add(id string, r *Rule) error {
	if id == "" {
		return ErrEmptyRuleID
	}
	if r == nil || r.cond == nil {
		return ErrRuleInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.rules {
		if e.id == id {
			return fmt.Errorf("%w: %s", ErrDuplicateRule, id)
		}
	}
	s.rules = append(s.rules, ruleEntry{id: id, rule: r})
	s.index(len(s.rules) - 1)
	return nil
}

// Remove disables a rule. It reports whether the id was present.
func (s *RuleSet) Remove(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.rules, func(e ruleEntry) bool { return e.id == id })
	if i < 0 {
		return false
	}
	s.rules = slices.Delete(s.rules, i, i+1)
	s.byType, s.anyType = map[string][]int{}, nil
	for j := range s.rules {
		s.index(j)
	}
	return true
}

func (s *RuleSet) index(i int) {
	types := s.rules[i].rule.eventTypes
	if types == nil {
		s.anyType = append(s.anyType, i)
		return
	}
	for _, t := range types {
		if l := s.byType[t]; len(l) == 0 || l[len(l)-1] != i {
			s.byType[t] = append(l, i)
		}
	}
}

// Len returns the number of rules.
func (s *RuleSet) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.rules)
}

// Rule returns the rule stored under id.
func (s *RuleSet) Rule(id string) (*Rule, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.rules {
		if e.id == id {
			return e.rule, true
		}
	}
	return nil, false
}

// Match is one rule that matched an event, with its rendered payload. It
// carries no alert id: ids are assigned when the alert event is stored.
type Match struct {
	RuleID      string
	RuleName    string
	Severity    Severity
	DedupeKey   string
	GroupValues []any
	Actions     []Action
	Payload     Payload

	rule *Rule
}

// RuleError reports a rule that failed at run time (overflow, division by
// zero) and therefore did not match.
type RuleError struct {
	RuleID string
	Err    error
}

// Error implements error.
func (e RuleError) Error() string { return fmt.Sprintf("alerts: rule %s: %v", e.RuleID, e.Err) }

// Unwrap returns the cause.
func (e RuleError) Unwrap() error { return e.Err }

// Evaluation is the result of evaluating one event.
type Evaluation struct {
	Matches []Match
	Errors  []RuleError
}

// Evaluate runs every applicable rule against ev, in the order the rules
// were added. It is pure: no clock, no I/O.
func (s *RuleSet) Evaluate(ev *Event) Evaluation {
	var out Evaluation
	if ev == nil {
		return out
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	typed := s.byType[ev.Type]
	i, j := 0, 0
	ctx := &evalCtx{ev: ev}
	for i < len(typed) || j < len(s.anyType) {
		var idx int
		if j >= len(s.anyType) || (i < len(typed) && typed[i] < s.anyType[j]) {
			idx, i = typed[i], i+1
		} else {
			idx, j = s.anyType[j], j+1
		}
		e := s.rules[idx]
		ctx.err = nil
		m, ok := evalRule(ctx, e.id, e.rule, s.opts)
		if ctx.err != nil {
			out.Errors = append(out.Errors, RuleError{RuleID: e.id, Err: ctx.err})
			continue
		}
		if ok {
			out.Matches = append(out.Matches, m)
		}
	}
	return out
}

// EvaluateRule evaluates a single rule (TestRule, spec 11.7).
func EvaluateRule(id string, r *Rule, ev *Event, opts RenderOptions) (Match, bool, error) {
	if r == nil || r.cond == nil {
		return Match{}, false, ErrRuleInvalid
	}
	if ev == nil {
		return Match{}, false, nil
	}
	ctx := &evalCtx{ev: ev}
	m, ok := evalRule(ctx, id, r, opts)
	if ctx.err != nil {
		return Match{}, false, RuleError{RuleID: id, Err: ctx.err}
	}
	return m, ok, nil
}

func evalRule(ctx *evalCtx, id string, r *Rule, opts RenderOptions) (Match, bool) {
	v := r.cond(ctx)
	if v.k != kBool || !v.b || ctx.err != nil {
		return Match{}, false
	}
	groups := make([]any, len(r.groups))
	for i, g := range r.groups {
		groups[i] = g(ctx).export()
	}
	if ctx.err != nil {
		return Match{}, false
	}
	return Match{
		RuleID:      id,
		RuleName:    r.Name,
		Severity:    r.Severity,
		DedupeKey:   DedupeKey(id, groups),
		GroupValues: groups,
		Actions:     r.Actions,
		Payload:     renderPayload(r, ctx.ev, opts),
		rule:        r,
	}, true
}

// Payload is the rendered alert body (spec 15.4) without alert_id.
type Payload struct {
	Rule            string            `json:"rule"`
	Severity        Severity          `json:"severity"`
	TenantID        string            `json:"tenant_id"`
	ProgramID       string            `json:"program_id,omitempty"`
	EventID         string            `json:"event_id,omitempty"`
	EventType       string            `json:"event_type,omitempty"`
	Summary         string            `json:"summary"`
	OccurredAt      string            `json:"occurred_at"`
	Entity          *PayloadEntity    `json:"entity,omitempty"`
	Diff            *PayloadDiff      `json:"diff,omitempty"`
	Links           map[string]string `json:"links,omitempty"`
	SuppressedCount int               `json:"suppressed_count,omitempty"`
}

// PayloadEntity identifies the entity an alert is about. Name is the
// asset's display field (name, address, cidr, url, number, ...).
type PayloadEntity struct {
	ID        string `json:"id"`
	AssetType string `json:"asset_type"`
	Name      string `json:"name,omitempty"`
}

// PayloadDiff shows what a content change changed (V2_DESIGN §8): the
// snapshots, the classification, the first MaxPayloadChanges level-3
// changes, a one-line summary and the redacted unified excerpt. Score and
// ScoreFeatures let receivers rank alerts; they did not decide this one.
type PayloadDiff struct {
	URL            string         `json:"url"`
	SnapshotID     string         `json:"snapshot_id,omitempty"`
	FromSnapshotID string         `json:"from_snapshot_id,omitempty"`
	DiffID         string         `json:"diff_id,omitempty"`
	Outcome        string         `json:"outcome,omitempty"`
	Levels         []string       `json:"levels,omitempty"`
	Class          string         `json:"class,omitempty"`
	Newness        string         `json:"newness,omitempty"`
	Cause          string         `json:"cause,omitempty"`
	Changes        []string       `json:"changes,omitempty"`
	MoreChanges    int            `json:"more_changes,omitempty"`
	Summary        string         `json:"summary"`
	Excerpt        string         `json:"excerpt,omitempty"`
	Score          float64        `json:"score"`
	ScoreFeatures  []ScoreFeature `json:"score_features,omitempty"`
}

// MaxPayloadChanges bounds the changes listed in a payload; the rest are
// counted in more_changes.
const MaxPayloadChanges = 20

// DiffSummary is a one-line account of a content change: its class (or
// outcome), newness, a non-target cause, and the first three changes,
// e.g. "semantic_endpoint, likely_new: added endpoint ep:POST
// https://h/api (+1 more)".
func DiffSummary(d *Diff) string {
	if d == nil {
		return ""
	}
	head := d.Class
	if head == "" {
		head = d.Outcome
	}
	if d.Newness != "" {
		head += ", " + d.Newness
	}
	if d.Cause != "" && d.Cause != "target" {
		head += ", cause " + d.Cause
	}
	if d.Baseline {
		head += ", baseline"
	}
	if len(d.Changes) == 0 {
		return head
	}
	shown := d.Changes[:min(3, len(d.Changes))]
	parts := make([]string, len(shown))
	for i, c := range shown {
		parts[i] = strings.Replace(c, "|", " ", 1)
	}
	out := head + ": " + strings.Join(parts, "; ")
	if n := len(d.Changes) - len(shown); n > 0 {
		out += fmt.Sprintf(" (+%d more)", n)
	}
	return out
}

func payloadDiff(d *Diff) *PayloadDiff {
	pd := &PayloadDiff{URL: d.URL, SnapshotID: d.SnapshotID, FromSnapshotID: d.FromSnapshotID, DiffID: d.DiffID, Outcome: d.Outcome,
		Levels: d.Levels, Class: d.Class, Newness: d.Newness, Cause: d.Cause, Summary: DiffSummary(d), Excerpt: d.Excerpt,
		Score: d.Score, ScoreFeatures: d.ScoreFeatures}
	pd.Changes = d.Changes[:min(MaxPayloadChanges, len(d.Changes))]
	pd.MoreChanges = len(d.Changes) - len(pd.Changes)
	return pd
}

// AlertBody is the webhook JSON: the payload with its stored alert id.
type AlertBody struct {
	AlertID string `json:"alert_id"`
	Payload
}

func renderPayload(r *Rule, ev *Event, opts RenderOptions) Payload {
	p := Payload{
		Rule:      r.Name,
		Severity:  r.Severity,
		TenantID:  ev.TenantID,
		ProgramID: ev.ProgramID,
		EventID:   ev.ID,
		EventType: ev.Type,
	}
	if !ev.OccurredAt.IsZero() {
		p.OccurredAt = core.FormatTime(ev.OccurredAt)
	}
	subject := ""
	link := func(key, path string) {
		if opts.ConsoleBaseURL == "" {
			return
		}
		if p.Links == nil {
			p.Links = map[string]string{}
		}
		p.Links[key] = opts.ConsoleBaseURL + path
	}
	switch {
	case ev.Entity != nil:
		p.Entity = &PayloadEntity{ID: ev.Entity.ID, AssetType: ev.Entity.AssetType, Name: displayName(ev.Entity)}
		subject = ev.Entity.AssetType + " " + p.Entity.Name
		link("console", "/graph/entities/"+ev.Entity.ID)
	case ev.Diff != nil:
		p.Diff = payloadDiff(ev.Diff)
		subject = "content " + ev.Diff.URL
		if s := p.Diff.Summary; s != "" {
			subject += " (" + s + ")"
		}
		if ev.Diff.SnapshotID != "" {
			link("console", "/snapshots/"+ev.Diff.SnapshotID)
		}
	case ev.Approval != nil:
		subject = "approval " + ev.Approval.ID
		if ev.Approval.Role != "" {
			subject += " (role " + ev.Approval.Role + ")"
		}
		link("console", "/approvals/"+ev.Approval.ID)
	case ev.Run != nil:
		subject = "run " + ev.Run.ID
		if ev.Run.Code != "" {
			subject += ": " + ev.Run.Code
		}
		link("console", "/runs/"+ev.Run.ID)
	case ev.Edge != nil:
		subject = "edge " + ev.Edge.Label + " " + ev.Edge.FromEntityID + " -> " + ev.Edge.ToEntityID
	}
	p.Summary = r.Name + ": " + ev.Type
	if subject != "" {
		p.Summary += " for " + subject
	}
	return p
}

// displayName picks the identifying content field of an entity.
func displayName(e *Entity) string {
	for _, k := range []string{"name", "address", "cidr", "url", "number", "domain", "subject_common_name", "unique_id", "handle"} {
		switch v := e.Content[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return fmt.Sprint(int64(v))
		}
	}
	return e.Key
}
