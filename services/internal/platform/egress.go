package platform

// Headers between the tool runner and the egress proxy (spec 4.4, 4.5).
// The tool runner sets them on every proxied request (CONNECT or forward);
// the proxy validates them and strips them before anything leaves.
const (
	// HeaderProxyAuthorization carries "Bearer <internal token>".
	HeaderProxyAuthorization = "Proxy-Authorization"
	// HeaderDecisionID names the pinned allow decision (dec_...).
	HeaderDecisionID = "X-Flowforge-Decision-Id"
	// HeaderRunID and HeaderEffectPath identify the effect for the logs.
	HeaderRunID      = "X-Flowforge-Run-Id"
	HeaderEffectPath = "X-Flowforge-Effect-Path"
	// HeaderRefused is set by the proxy on every refusal with the reason.
	HeaderRefused = "X-Flowforge-Refused"
	// HeaderRedirectDecision is set by the proxy on a redirect it re-checked
	// and allowed: the decision id of the next hop.
	HeaderRedirectDecision = "X-Flowforge-Redirect-Decision"
	// HeaderEgressPurpose selects a proxy path other than the scope-decision
	// one. PurposeNotify is the notifier's webhook and Slack deliveries
	// (G-SVC-08): no decision, an operator allowlist and the SSRF checks of
	// spec 15.4 instead.
	HeaderEgressPurpose = "X-Flowforge-Egress-Purpose"
	PurposeNotify       = "notify"
)
