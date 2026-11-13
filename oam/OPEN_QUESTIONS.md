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
    and deduplicated per tenant. A referenced id that is not in the graph
    (for the observation's tenant and program: another tenant's or
    program's id counts as missing, as Postgres row-level security would
    see it) makes the observation wait instead of failing (G-CORE-03, spec
    23.5 any-order convergence); see the README. Released observations are
    applied in sorted (tenant, observation id) order so change events are
    deterministic; change events are not part of the canonical state.
21. **Strict decoding** matches keys exactly (Go's encoding/json would match
    `Name` to `name`), rejects duplicate keys and trailing data at every
    level, and accepts unknown OAM asset types as Opaque. Unknown relation
    or property *types* are rejected in both modes.

## netnorm

No defects found while using `core/netnorm`. One note for its owner: it
percent-decodes `%2e` before removing dot segments, so `/%2e%2e/x`
normalizes to `/x` (consistent with RFC 3986 section 6.2.2 and WHATWG).
