// Package alerts implements the Flowforge notifications and alert rules
// of spec section 15 (ff-alerts, layer 2):
//
//   - the alert rule language (15.2): a lexer following the DSL lexical
//     rules (7.2), a parser for
//     `alert NAME when EXPR -> action(args), ... { clause }` reusing the
//     DSL expression grammar and precedence (7.3), and a static type
//     checker against the event schema (envelope 11.7, ChangeEvent 11.6,
//     OAM content fields 5.4) that reports diagnostics in the standard
//     format (7.7) with A-prefixed codes (see diag.go);
//   - evaluation (RuleSet.Evaluate) of compiled rules against a typed
//     Event, producing matches with a dedupe key and a rendered payload;
//   - the notifier semantics of 15.3 (Processor): consumer idempotency on
//     event_id (inv_08), dedupe keys, throttling, digests and quiet hours,
//     with time taken only from an injected core.Clock;
//   - delivery helpers of 15.1 and 15.4: the retry schedule with full
//     jitter (NextAttempt), webhook HMAC signing and verification (Sign,
//     Verify) and SSRF destination checks (ValidateWebhookURL,
//     CheckResolved);
//   - the default operational rules shipped per tenant (DefaultRules).
//
// The package is pure: no clock, network, DNS or environment access.
// Services resolve webhook hosts, send requests and persist alert events.
package alerts
