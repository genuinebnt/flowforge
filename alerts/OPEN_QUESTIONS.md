# Open questions and decisions (alerts)

Each item records a spec ambiguity, the choice made, and the test that
pins it.

1. **Word operators.** 15.2 lists `contains, starts_with, ends_with,
   matches, len, in` as functions, but its examples use `entity.name
   matches "..."` and the 12.7 REST example `entity.name contains
   "jenkins"`. Both forms are accepted; the infix forms sit at the
   relational level (5) and are non-associative. They are contextual
   identifiers, so `contains(a, b)` still parses as a call.
   *operators.json: "contains infix" / "contains call" etc.*

2. **`matches` pattern.** "size-limited" is unspecified. The pattern must
   be a string literal (compiled once, at rule compile time), at most
   1,024 bytes and at most 4,096 RE2 instructions (A0401/A0402). Matching
   is unanchored RE2 search. *diagnostics.json: "regex too large",
   "pattern not literal".*

3. **String interpolation.** 7.2 makes `{expr}` interpolate. The alert
   language has no use for it and it collides with regex quantifiers, so a
   lone brace is A0007 and `{{`/`}}` are literal braces (write
   `"^[a-z]{{7}}"`). *operators.json: "matches braces"; diagnostics.json:
   "interpolation".*

4. **Severity in an action** (`slack("sec-recon", severity = High)` in 12.7).
   An alert event has one severity (10.6 stores one payload), so an action
   severity sets the rule severity; two different severities (between
   actions or with the clause) are A0307. Default severity when none is
   given: **Medium**. Rule source spells severities `Info Low Medium High
   Critical` exactly; payloads use lower case. *spec_examples.json "REST
   rule"; diagnostics.json "severity conflict", "unknown severity".*

5. **Quiet hours and High.** 15.3: "delay delivery of severities below High
   until the window ends. Critical ignores them." Read literally, High is
   not below High, so both High and Critical are delivered immediately;
   Info/Low/Medium are held until the window end (end is exclusive). The
   zone is optional (default UTC); `Local` is refused; equal start and end
   is an error. *TestAlrQuietHours.*

6. **Throttle clock and window.** Windows use the processor's injected
   clock, not `occurred_at`, so out-of-order transport delivery cannot
   reopen a window. A window opens when an alert fires and lasts `d`; a
   match at exactly `fired + d` fires again. The count of suppressed
   matches is carried on the next fired alert's payload
   (`suppressed_count`). Without `throttle`, every match alerts (same key).
   *throttle.json.*

7. **Dedupe key encoding.** "sha256(rule_id || group_by values)" leaves the
   encoding open. Chosen: `hex(sha256(rule_id || 0x00 ||
   canonical_json([values...])))`, times as RFC 3339 strings, durations as
   Go duration strings, absent values as `null`. `rule_id` is the storage id
   given to `RuleSet.Add`, so renaming a rule does not change keys.
   *TestAlrPayloadShape, TestAlrProp_DedupeFastPathCanonical.*

8. **Digest grouping.** A digest collects all matches of a rule (not per
   dedupe key), from the first match for `d`; the message has the count,
   the first 20 payloads and the highest severity. Throttle applies before
   the digest: suppressed matches are not digested. Each digested match is
   still an alert event (`pending`, `Digested`). *TestAlrDigest_CountAndFirstTwenty.*

9. **Absent fields.** Sections not present in the event (e.g. `entity` on a
   run event) and content fields not carried by the asset are absent;
   operators propagate absence (Kleene logic for `&&`, `||`, `!`) and an
   absent condition does not match. String and number fields of a present
   section are always present; zero times are absent. Run-time arithmetic
   overflow or division by zero fails the rule closed (no match, reported
   in `Evaluation.Errors`). *operators.json "absent ..." and "overflow ...".*

10. **Entity content fields.** OAM content varies by asset type, so the
    checker accepts the union of the content field names of the types
    modelled in spec 5.4, each with one type (strings by default; `number`,
    `port`, `output_length`, `headcount` Int; `is_ca`, `dnssec`, `active`,
    `non_profit` Bool; certificate/record dates Time parsed from RFC 3339;
    `status`, `key_usage`, `crl_distribution_points`, `target_markets`
    List<String>; `attributes` Object). Fixed columns shadow content fields
    (`entity.id` is the Flowforge id, not DomainRecord.id). A value whose
    JSON type differs is absent. Opaque types expose no content fields.
    Unknown names are A0202.

11. **Run and auth fields.** 15.2 names `run` without fields. `run` exposes
    the RunEventPayload (id, seq, type, payload) plus workflow_name,
    workflow_version, status and `code`, `message`, `path`, `resource`,
    which producers lift from the event payload (run_failed code/message,
    effect_failed error.code, budget_exceeded resource). auth.* events
    carry only the envelope. The auth event type names used by the default
    signup-abuse rule (`auth.signup_throttled`, `auth.login_throttled`) are
    not defined by the spec and must be aligned with the gateway.

12. **Patch / payload fields.** `change.patch.name` is A0202: patches are
    untyped. Use `"name" in change.patch` or `len(change.patch)`.

13. **Retry schedule.** 15.1 lists 10 s, 30 s, 2 min, 10 min, 1 h and "give
    up after 24 h". After the fifth retry the 1 h step repeats until the
    next attempt would start more than 24 h after the first attempt. Full
    jitter is uniform in [1 s, base] (a 1 s floor avoids hot retries).
    *TestAlrRetrySchedule.*

14. **Webhook verification.** Receivers reject `|now - ts| > 300 s`
    (300 s exactly is accepted). The signature header may hold several
    comma-separated `v1=` values (secret rotation); any match passes.
    *TestAlrWebhookSignature.*

15. **SSRF.** Beyond "internal ranges" the deny list includes CGNAT,
    documentation, benchmarking, reserved/broadcast, IETF protocol
    assignments, discard-only, deprecated site-local, IPv4-compatible
    IPv6, local-use NAT64, zoned addresses, and NAT64/6to4/mapped forms of
    denied IPv4 addresses. Names: single-label names and `localhost`,
    `*.localhost`, `*.local`, `*.internal`, `*.home.arpa` are refused before
    resolution. An empty resolution fails closed. Callers must pin the
    connection to the checked addresses (DNS rebinding). *TestAlrSSRF.*

16. **Idempotency transaction.** `ProcessedStore.MarkProcessed` runs before
    evaluation; evaluation cannot fail, so this is equivalent to the
    spec's single transaction for the in-memory fake. A Postgres adapter
    must write the mark and the alert events in one transaction.

17. **Throttle and digest state** live in the processor's memory, and
    every change is reported in `Outcome.State` (the windows and open
    digests an event touched) so the caller persists it with the alert
    events; `Processor.Restore` rebuilds a processor from that state
    (G-LIM-06). Restore skips windows that have ended by the processor's
    clock, drops digests whose rule is gone, and takes a digest's actions
    and quiet hours from the current rule (they are not persisted, so a
    rule edit applies to its open digest). `SuppressedBefore` of the first
    alert after a restart counts only suppressions that were persisted.
    *TestRegress_GLIM06_StateSurvivesRestart.*

18. **Columns** in diagnostics count Unicode code points; offsets are bytes.
