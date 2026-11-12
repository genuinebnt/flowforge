# scope: open questions

Spec ambiguities met while implementing section 4, with the choice made.
Where the spec is silent the most conservative (fail-closed) reading was
taken, and each choice is pinned by a test or conformance case (named in
brackets).

1. **Failed resolution.** 4.3 resolves active name targets but says nothing
   about failure. Invariant 2.4 ("no silent success") requires a deny. A
   resolver error, an empty answer, a zoned or invalid address, a cancelled
   context or a missing resolver all yield `deny("resolution_failed")`, a
   reason not in the proto's list. [resolved_ip.json `resolution_*`,
   `TestResolutionFailsClosed`]

2. **Resolution happens before the allow check**, as in 4.3. A name that no
   allow rule covers but whose resolution fails is therefore reported as
   `resolution_failed`, not `no_matching_allow`. Both are denies.
   [`resolution_before_allow`]

3. **The `resolve` flag.** The proto `CheckRequest.resolve` makes
   resolution optional, and the spec's `wildcards.json` expects active
   allows without resolver answers, but 4.3 always resolves. The Go API
   resolves by default and offers `Request.SkipResolution` for advisory
   checks. Such a decision has no pinned addresses, so the egress proxy
   (which connects only to pinned addresses) cannot act on it. In the
   conformance runner, files without a `resolver` section skip resolution.
   [`skip_resolution`, `TestCheckResolvesOnce`]

4. **"t is hostname"** is read as "the connection goes to a DNS name", so
   URL targets with a name host also resolve and pin. IP literals (bare, as
   host, or as URL host) are not resolved and get no pinned addresses; the
   connection goes to the literal, which the cidr rules already checked.
   [`scope_010c`, `ip_literal_*`]

5. **Unknown activity classes.** `core.ClassNone` (the zero value) and
   out-of-range classes are refused with `deny("invalid_class")` rather
   than treated as passive, which would skip windows and resolution for a
   caller that forgot to set the class. [`class_none`]

6. **url_prefix deny rules at host level.** A host-level action (port
   scan, crawler) on a host may reach a path that a deny url_prefix
   excludes, and the egress proxy cannot see paths. So a deny url_prefix
   also denies host and IP targets naming its host, and CIDR targets that
   contain its IP host. Allow url_prefix rules only ever match URL targets.
   This can make a whole host out of scope for host-level tools because one
   path is excluded; that is intentional. [url_prefix.json
   `host_level_deny*`, `ip_host_level_deny`, `cidr_contains_url_deny`]

7. **url_prefix path boundary.** A rule path matches the same path, any
   path extending it when the rule path ends in `/`, and any path that
   continues it with `/`. So `https://h/v2/` does not match `https://h/v2`,
   and `https://h/docs` matches `/docs` and `/docs/x` but not `/docsx`.
   Rule values with a query are rejected; target queries are ignored.

8. **CIDR targets.** An allow cidr rule must contain the whole target
   prefix; a deny cidr rule matches on any overlap. A /16 that is partly
   denied is therefore denied as a whole. [cidr.json `cidr_*`]

9. **asn rules** are validated (`AS<uint32>`) and exposed via
   `Policy.ASNs()` for discovery seeding, but never match any target, for
   active or passive checks, allow or deny: the scope service has no ASN
   membership data, and an ASN must never be "sole permission". Passive
   graph status therefore ignores them too. [`TestStatus`]

10. **Windows.** Start is inclusive and end exclusive; `24:00` is allowed
    as an end; start equal to end is rejected (use `00:00`-`24:00`). A
    window whose end is before its start runs past midnight and belongs to
    the day it starts on. Times are wall-clock in the window's zone, so DST
    moves the window with local time. `tz` must be a non-empty IANA name;
    `Local` is rejected because it depends on the host. The zone database
    is embedded (`time/tzdata`). Days must be lowercase `mon`..`sun`.
    [windows.json]

11. **Window check before deny rules.** Following 4.3 literally, an active
    check outside the window reports `outside_window` even for an
    explicitly denied target. [`window_before_rules`]

12. **Kill switch first.** `kill_switch` is checked before normalization;
    the decision still carries the normalized target when there is one.
    [`kill_switch_first`]

13. **Rule order.** When several rules of the deciding effect match, the
    first in document order is reported (the spec's `for rule in rules`).
    Resolved addresses are tried in answer order. [`first_rule_wins`,
    rest_example.json `url_wildcard_first`]

14. **effective_from** is carried but not enforced by `Check`: choosing the
    version (current or pinned) is the scope service's job.

15. **Status ignores max_class**, windows and resolution: it is the
    rule-matching part of a passive check. A target that does not
    normalize is `unknown`.

16. **Lenient rule values.** CIDR rule values with host bits set are
    masked (`10.0.0.1/8` is `10.0.0.0/8`); names and URLs are normalized
    as targets are. A domain value that looks numeric is rejected; use a
    cidr rule.

17. **Unspecified target kind** is inferred: `://` means URL, `/` means
    CIDR, anything else host (which accepts IP literals). Text such as
    `acme.com/v2` thus fails as a CIDR, which is a deny.
