// Package sourcemap decodes Source Map v3 as specified by ECMA-426
// (Source Map Format Specification): strict JSON fields, the "mappings"
// Base64 VLQ string (own state machine with 32-bit overflow checks), index
// maps with ordered, non-overlapping sections (flattened into one Map),
// sources resolution against sourceRoot and the map URL, sourcesContent,
// ignoreList, and generated-to-original lookup by binary search. It also
// encodes mappings, so decode and encode round-trip.
//
// The JSON layer is encoding/json; everything inside "mappings" is
// decoded here. Limits (MaxInputBytes 8 MiB, MaxSources 20,000) give
// GradePartial; sections by URL (which would require fetching) give
// GradeUnsupported; any other error GradeFailed.
package sourcemap
