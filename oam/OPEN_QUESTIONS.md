# oam: open questions

Spec ambiguities met while implementing `oam`, the choice made, and the
test that pins it.

## Fields tagged "verify" in spec 5.4-5.6

The spec marks these field lists as unverified against the OAM docs. The
spec's names are used as written:

* FQDN `name`; IPAddress `ip_version` (OAM's Go field is `Type`); Netblock
  `cidr`, `ip_version`; AutonomousSystem `number`.
* TLSCertificate, File, DomainRecord, IPNetRecord, AutnumRecord: every field
  as listed.
* BasicDNSRelation `header {rr_type, class, ttl}`; PrefDNSRelation
  `preference`; SRVDNSRelation `priority`, `weight`, `port`.
* DNSRecordProperty `property_name`, `header`, `data`; VulnProperty `id`,
  `desc`, `source`, `category`, `enum`, `ref`.
* Labels `common_name` and `san_dns_name` (endpoints "verify"): accepted
  between any types; not validated.
* Person, Location, Phone, ContactRecord, Identifier, Account,
  FundsTransfer, Product, ProductRelease: stored as `Opaque`.

## Decisions

1. **`type` clashes inside File and IPNetRecord.** Both OAM structs have a
   content field called `type`, which collides with the asset
   discriminator. Following the spec's own `ip_version` precedent they are
   renamed `file_type` and `ipnet_type`, in the envelope and in stored
   content. (`TestAssetCodecRoundTrip`, keys conformance.)
2. **TLSCertificate key.** `tls:<sha256 of DER>` needs the DER, which is not
   in the field list. A Flowforge field `fingerprint_sha256` (64 hex digits,
   colons allowed on input) carries it; `TLSFingerprint(der)` computes it. A
   certificate without it has no key and is rejected rather than keyed by
   issuer/serial. (keys `artifacts.json`.)
3. **Product key.** Kept as the spec says: `product:<sha256 of canonical
   JSON>`. A name-based key was rejected for v0.1 because Product's fields
   are unverified (vendor or version may be needed to disambiguate) and
   because merging distinct products is worse than duplicating one.
   (keys `opaque.json`.)
4. **Opaque keys** hash `core.CanonicalJSON` of the content, which keeps
   number literals textually (`1` and `1.0` differ). Opaque type names must
   be identifiers and must not equal a modeled type in any letter case.
5. **File key** hashes the netnorm-normalized URL when it is http(s), and
   the trimmed string otherwise (`s3://`, `gs://`).
6. **IPNetRecord key** uses the handle (uppercased, registry handles are
   case-insensitive) when present, else the normalized CIDR; a record seen
   once with and once without a handle therefore yields two entities.
7. **Service `unique_id`** is used as given (trimmed; lowercased when hex).
   `ServiceUniqueID` lowercases protocol and service type before hashing.
8. **URL userinfo.** `netnorm.ParseURL` rejects userinfo, so a URL asset
   whose `url` carries credentials is invalid; tools may report them in
   `username`/`password`.
9. **AS 0** is rejected (reserved, and indistinguishable from a missing
   `number`); numbers must fit 32 bits.
10. **Edge identity.** 23.5 says edges are keyed by (from, label, to); 5.7
    and the SQL unique constraint add relation type and discriminator. The
    5.7 key is used (`EdgeKey`).
11. **Label validation** checks endpoint types only, as 5.5 says ("MUST
    validate only the endpoint types"); a docs label on an unusual relation
    type (e.g. `dns_record` as SimpleRelation) is accepted. `ptr_record`
    ("docs (named)") and `owns` ("docs (named only)") are enforced; `owns`
    requires an Organization source and accepts any target type. Labels are
    lowercased; unknown labels are accepted. (`TestOAM004_LabelEndpoints`.)
12. **Content merge granularity.** "Non-empty wins, newest wins on conflict"
    is applied per top-level field with a per-field stamp; a single
    entity-level stamp is not convergent. Nested objects/arrays are whole
    values. Empty means null, "", [] or {}; `false` and `0` are values, but
    zero-valued Go fields are omitted on encoding, so pointer types (`*bool`)
    are used where false is meaningful. (`TestOAMProp_ContentJoinLaws`.)
13. **Confidence units.** Per-source confidence is kept as the
    SourceProperty integer percent, so the in-memory graph and a Postgres
    implementation reading `entity_tags` agree. The combined value is
    rounded to 4 decimals before the 0.99 cap. Observation confidence must
    be in [0, 1]; a missing value decodes as 0 and adds no confidence.
14. **Tagging is not a sighting.** A tag on an existing entity or edge does
    not add its tool as a source or move `last_seen`; a SourceProperty tag
    does add that named source.
15. **Staleness** (5.7 "the sources that previously reported it completed a
    covering run"): a record becomes stale when every tool that has run a
    covering run over it has missed it `stale_after_runs` consecutive times;
    sources without covering runs (passive feeds) do not keep it alive.
    Edges follow the same rule (event `edge_removed`); a re-seen stale edge
    emits `edge_added` because the spec has no `edge_reactivated`. Covering
    runs are idempotent per (tenant, program, tool, run).
    (`TestOAM005_Staleness`, merge `staleness.json`.)
16. **oam_005 wording** ("older than stale_after") reads as time-based; 5.7
    defines staleness by covering runs, which is what is implemented. A
    time-based ageing filter (spec 22, CSR build) is left to its consumer.
17. **entity_changed patch** paths are JSON pointers into the asset content
    (`/title`) with ops `add` and `replace` only (merges never remove).
    Changes to confidence or last_seen alone emit no event.
    `scope_status_changed` carries `[{"op":"replace","path":"/scope_status"}]`.
18. **Scope status** is recomputed from the merged content whenever content
    changes, through the `ScopeFunc` port; any answer other than in_scope or
    out_of_scope is unknown, and no status is inferred from neighbours.
19. **Canonical form.** `CanonicalBytes` covers entities and edges with
    their tags; it excludes the change-event log (order-dependent history),
    applied observation ids and per-field stamps. Times are RFC 3339 with
    nanoseconds in UTC.
20. **Ids.** Entity/edge ids are UUIDv8 derived from SHA-256 of the identity
    tuple and rendered as `ent_`/`edg_` public ids, instead of random
    UUIDv7s. Endpoints in an envelope are local refs unless they are a
    well-formed `ent_` id (bare UUIDs are not accepted as ids); refs that
    look like ids are rejected. Observation ids are free-form (<= 256 bytes)
    and deduplicated per tenant.
21. **Strict decoding** matches keys exactly (Go's encoding/json would match
    `Name` to `name`), rejects duplicate keys and trailing data at every
    level, and accepts unknown OAM asset types as Opaque. Unknown relation
    or property *types* are rejected in both modes.

## v2 graph expansion (V2 design §3)

22. **Authority units.** A unit is (source entity, profile, rule
    family). Edges leaving `authority.source` and non-source tags on it
    are *authoritative*: they are reported to the unit of their family
    (`FamilyOfLabel`, `FamilyOfProperty`; unknown labels and property
    types are their own family) and do not register the tool for covering
    runs. Edges from other entities, tags on other targets, and every part
    of an observation without authority stay *classic* (spec 5.7). The
    source entity itself, and listed entities that are not the target of
    an authoritative edge, are sighted classically.
23. **Coverage gate** (amended after the Sentinel review). A reading is
    complete for a family only when `coverage = complete` and the family
    is in `covered_rules`; it then replaces the unit's member set. Every
    other reading (partial, failed, unsupported coverage, or a complete
    capture's families outside `covered_rules`) only adds members. A
    complete capture that reports nothing for a covered family empties
    that unit (withdraws every member). `covered_rules` of a non-complete
    observation is accepted and ignored. Withdrawal is not retraction: the
    record goes stale with history kept and comes back when re-reported
    (a `retract` stance is left for v3).
24. **Convergence.** Readings are ordered by `Stamp` (observed_at, tool,
    observation id), a total order over distinct observations. A unit's
    effective members are `members(latest complete reading) ∪ {m : a
    partial reading newer than it reported m}`, a function of the
    multiset of observations, so the state is independent of order and
    duplication (23.5). `AuthUnit.Apply` maintains it incrementally: an
    older complete reading and a partial one older than the latest
    complete change nothing. (`TestOAMInv05_*`, `TestOAMInv06_*`.)
25. **Liveness.** A record is active iff it has classic support that
    covering runs have not made stale (a classic sighting clears classic
    staleness), or authoritative support: an edge held by at least one
    unit (any profile), an entity that is the target of such an edge.
    Covering runs never stale a record with authoritative support; a
    withdrawal stales at once only records without other support. A tag
    is visible iff some classic observation (or the API) reported it or a
    unit holds it; withdrawn tags are hidden, never deleted. Records an
    out-of-order (older) complete capture alone reports are created and
    stale in the same operation (`entity_added` then `entity_stale`).
26. **Event order** within one observation: entity events (observation
    order), edge events, then liveness transitions caused by authority
    (edges by id, then entities by id), then `tag_changed` (entity tags
    before edge tags, by target id, type, name). Event ids are derived
    from the emission index, so every implementation must keep this order.
27. **tag_changed** covers every property type except SourceProperty
    (provenance; confidence changes emit no event, decision 17). One event
    per tag per operation: the diff between its visible content before and
    after (`TagPatch`): `[{"op":"add","path":"","value":{...}}]` when it
    appears, `[{"op":"remove","path":""}]` when it disappears (the root
    "remove" is "RFC 6902-like": 6902 cannot remove the root), field-level
    add/replace/remove ops otherwise. `PatchOp.value` is omitted when nil.
28. **Change-log base.** `entity_added` carries `base = {content,
    scope_status}` and the `edge_added` that creates an edge `base =
    {content}`; reactivations have none. Edge content changes still emit
    no event (decision 17), so replays reconstruct edge existence, state
    and tags but not later edge content.
29. **Cause and baseline.** `Observation.cause` (`target` default,
    `parser`, `normalizer`) and `baseline` are copied onto every event the
    observation produces (target is stored as empty); covering-run events
    carry neither. They do not affect state.
30. **content_changed.** `RecordContentChange` validates and normalizes
    the patch (`ContentChangePatch.Normalize`: levels ordered hash, text,
    semantic; cause defaulted to target; class from a closed list; score
    in [0, 1]; feature weights in [-1, 1]; summary counts >= 0) and is
    idempotent per (entity, snapshot_from, snapshot_to, diff_id) via
    `ContentChangeUUID`. It changes no state.
31. **url_template choices.** Query strings are dropped (parameters are
    `ParameterProperty` tags; routing by query is not modelled); named
    placeholders other than id/uuid/hex/expr (OpenAPI `{userId}`) become
    `{expr}`, so an OpenAPI path and an observed numeric path of the same
    route stay distinct keys (`{expr}` vs `{id}`); the content service may
    unify them. Trailing slashes and empty segments are kept (RFC 3986).
    Only http(s): `ws(s)` endpoints are rejected. Percent-encoded braces
    are literal. A segment mixing text and a placeholder is `{expr}`.
32. **Endpoint method** is upper-cased although RFC 9110 methods are
    case-sensitive (V2 §3.1 says upper case). `source` is not part of the
    key and merges last-writer-wins.
33. **redirects_to status** is an optional `status` field on
    SimpleRelation (300-399), rejected on any other label; it is part of
    the edge content, not of its identity.
34. **Secrets.** exposed_secret `VulnProperty`: id
    `secret:<rule>:<hmac hex>`, enum = rule, desc = preview (`xxxx...yy`,
    or `...` for secrets under 12 bytes or with non-printable bytes),
    optional source and ref. `LooksLikeSecret` (rules in its doc comment)
    applies to every field; it is not applied to other properties
    (CSP nonces and digests would trip it) but `CheckNoSecrets` is exported
    for producers. `HTTPHeaderProperty` rejects `authorization`,
    `proxy-authorization` and `cookie`, and `set-cookie` values with any
    `k=v` whose key is not a cookie attribute.
35. **ContentProperty** has one tag per profile (the tag name);
    `last_changed_at` must be RFC 3339 UTC with milliseconds; `size` and
    `change_count` are always encoded (0 is a value).

## netnorm

No defects found while using `core/netnorm`. One note for its owner: it
percent-decodes `%2e` before removing dot segments, so `/%2e%2e/x`
normalizes to `/x` (consistent with RFC 3986 section 6.2.2 and WHATWG).

## Registration endpoints (ff extension)

Spec 5.5 gives `registration` only Netblock → IPNetRecord. OAM also links
FQDN → DomainRecord and AutonomousSystem → AutnumRecord with it, and the
rdap and asn tools emit those edges, so the table admits the three pairs
(`EndpointRule.Pairs`: each source only with its own record type).
[`conformance/oam/labels/registration.json`]
