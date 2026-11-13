// Package oam holds the Open Asset Model domain types of Flowforge and
// their convergent merge semantics.
//
// It implements spec sections 5 (graph model: assets, canonical keys,
// relations and their label/endpoint rules, properties, ingest and merge
// semantics), 6.1-6.3 (observation envelope and adapter batch limit) and
// 23.5 (convergent graph merge), plus the required tests oam_001..007 of
// 25.3, the property of 25.4 and benchmark b_oam_01 of 25.8.
//
// The package is pure (layer 1): it depends only on core and core/netnorm
// and does no I/O. Its pieces are
//
//   - Asset and its concrete types (FQDN, IPAddress, ... Organization, and
//     Opaque for every other OAM type), each with the canonical
//     de-duplication key of spec 5.4 computed through core/netnorm;
//   - Relation (five OAM relation types) with the spec 5.7 discriminator,
//     and ValidateEdge for the spec 5.5 label table;
//   - Property (SourceProperty, SimpleProperty, DNSRecordProperty,
//     VulnProperty, FlowforgeProperty);
//   - Observation, the spec 6.1 envelope, with Strict (requests) and
//     Lenient (stored data) decoding;
//   - the merge primitives the Postgres graph service reuses: Sources and
//     CombineConfidence (noisy-or over distinct sources, per-source
//     maximum, capped at MaxConfidence), Content (per-field
//     last-writer-wins by Stamp with non-empty-wins), MinTime/MaxTime,
//     EdgeKey and the derived EntityID/EdgeID;
//   - the v2 graph expansion (docs/V2_DESIGN.md §3): the Endpoint asset
//     and NormalizeURLTemplate, the content-capture relation labels and
//     rule families, ContentProperty, HTTPHeaderProperty,
//     ParameterProperty and the exposed_secret conventions, coverage-gated
//     authoritative staleness (Authority, AuthUnit), tag_changed and
//     content_changed events;
//   - Graph, an in-memory reference graph that applies observations with
//     exactly those semantics, tracks covering-run staleness, emits change
//     events and encodes its state canonically (CanonicalBytes).
//
// Determinism: entity and edge ids are derived from their identity
// (EntityUUID, EdgeUUID), all timestamps come from observations or run
// reports, and every iteration over maps is sorted, so applying any
// permutation of an observation multiset, with any duplication, yields
// byte-identical CanonicalBytes.
package oam
