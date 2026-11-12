package alerts

// Default operational rules shipped to every tenant (spec 15.4, last
// paragraph): run failures, approvals waiting, tool rate limiting, budget
// exhaustion and signup abuse. They are rules over run.events,
// approval.events and auth.events and use the tenant's default Slack
// channel. Tenants may disable or replace them like any other rule.
const (
	DefaultRuleRunFailed = `alert ops_run_failed
  when event.type == "run.run_failed"
  -> slack()
  severity = High  group_by run.workflow_name, run.code  throttle 15m`

	DefaultRuleApprovalWaiting = `alert ops_approval_waiting
  when event.type == "approval.requested" && approval.expires_at - event.occurred_at > 6h
  -> slack()
  severity = Medium  digest 15m`

	DefaultRuleToolRateLimited = `alert ops_tool_rate_limited
  when event.type == "run.effect_failed" && run.code in ["RATE_LIMITED", "rate_limited"]
  -> slack()
  severity = Low  group_by run.path  throttle 1h`

	DefaultRuleBudgetExhausted = `alert ops_budget_exhausted
  when event.type == "run.budget_exceeded"
  -> slack()
  severity = High  group_by run.id, run.resource  throttle 1h`

	DefaultRuleSignupAbuse = `alert ops_signup_abuse
  when event.type in ["auth.signup_throttled", "auth.login_throttled"]
  -> slack()
  severity = High  digest 15m`
)

// DefaultRules returns the default operational rule sources keyed by a
// stable id suffix (the service prefixes the tenant's rule id).
func DefaultRules() map[string]string {
	return map[string]string{
		"ops_run_failed":        DefaultRuleRunFailed,
		"ops_approval_waiting":  DefaultRuleApprovalWaiting,
		"ops_tool_rate_limited": DefaultRuleToolRateLimited,
		"ops_budget_exhausted":  DefaultRuleBudgetExhausted,
		"ops_signup_abuse":      DefaultRuleSignupAbuse,
	}
}
