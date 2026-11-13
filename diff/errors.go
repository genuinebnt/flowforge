package diff

import "errors"

// Sentinel errors. Every error returned by this package wraps one of these
// (with %w), so callers test with errors.Is. The texts are stable codes the
// Rust port reproduces.
var (
	// ErrInvalidScript reports an EditScript that is not in canonical form:
	// ranges that do not tile both sides, empty or mis-sized ops, or token
	// arrays whose lengths disagree with the ops.
	ErrInvalidScript = errors.New("diff: invalid edit script")
	// ErrMismatch reports a script or patch applied to an input it was not
	// computed from (wrong length, or a deleted/context token differs).
	ErrMismatch = errors.New("diff: input does not match script")
	// ErrInvalidOptions reports an unknown algorithm or a negative bound.
	ErrInvalidOptions = errors.New("diff: invalid options")
	// ErrMalformedPatch reports unified-diff text that does not parse.
	ErrMalformedPatch = errors.New("diff: malformed unified diff")
	// ErrInvalidJSON reports JSON input that does not parse, has duplicate
	// object keys or trailing data, or nests deeper than MaxJSONDepth.
	ErrInvalidJSON = errors.New("diff: invalid JSON")
	// ErrInvalidPointer reports a malformed RFC 6901 JSON Pointer.
	ErrInvalidPointer = errors.New("diff: invalid JSON pointer")
	// ErrInvalidPatch reports an RFC 6902 document that is not an array of
	// well-formed operation objects.
	ErrInvalidPatch = errors.New("diff: invalid JSON patch")
	// ErrPatchTarget reports an RFC 6902 operation whose target or source
	// location does not exist or cannot be written.
	ErrPatchTarget = errors.New("diff: JSON patch target not found")
	// ErrPatchTest reports a failed RFC 6902 "test" operation.
	ErrPatchTest = errors.New("diff: JSON patch test failed")
	// ErrTreeTooLarge reports a tree with more nodes than TreeOptions.MaxNodes
	// (or a Tree implementation whose links do not form a finite tree).
	ErrTreeTooLarge = errors.New("diff: tree too large")
	// ErrMalformedTree reports bracket-notation text that does not parse.
	ErrMalformedTree = errors.New("diff: malformed tree")
)
