package oam

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors. Every error returned by this package wraps one of them,
// so callers (the graph service, which maps them to API codes) can use
// errors.Is.
var (
	// ErrMalformed means the input is not well-formed JSON of the expected shape.
	ErrMalformed = errors.New("oam: malformed JSON")
	// ErrUnknownField means a strict decode met a key the schema does not define.
	ErrUnknownField = errors.New("oam: unknown field")
	// ErrDuplicateField means a JSON object repeats a key.
	ErrDuplicateField = errors.New("oam: duplicate field")
	// ErrInvalidAsset means an asset fails validation or has no canonical key.
	ErrInvalidAsset = errors.New("oam: invalid asset")
	// ErrInvalidRelation means a relation fails validation.
	ErrInvalidRelation = errors.New("oam: invalid relation")
	// ErrUnknownRelationType means the relation type is none of the five OAM types.
	ErrUnknownRelationType = errors.New("oam: unknown relation type")
	// ErrInvalidProperty means a property fails validation.
	ErrInvalidProperty = errors.New("oam: invalid property")
	// ErrUnknownPropertyType means the property type is none of the known types.
	ErrUnknownPropertyType = errors.New("oam: unknown property type")
	// ErrLabelEndpoints means a docs-tagged relation label joins asset types
	// the label does not allow (spec 5.5). Returned as an *EdgeError.
	ErrLabelEndpoints = errors.New("oam: relation label not allowed between these asset types")
	// ErrInvalidObservation means an observation envelope fails validation.
	ErrInvalidObservation = errors.New("oam: invalid observation")
	// ErrBatchTooLarge means an observation carries more than MaxBatchEntities entities.
	ErrBatchTooLarge = errors.New("oam: observation batch too large")
	// ErrUnresolvedRef means an edge or tag names neither a local ref nor an entity id.
	ErrUnresolvedRef = errors.New("oam: unresolved reference")
	// ErrUnknownEntity means an entity or edge id does not exist in the graph
	// (or belongs to another tenant or program).
	ErrUnknownEntity = errors.New("oam: unknown entity or edge")
	// ErrPendingLimit means an observation that references entities or
	// edges not ingested yet cannot be buffered: the tenant already has
	// Config.MaxPending observations waiting (G-CORE-03).
	ErrPendingLimit = errors.New("oam: too many observations waiting for referenced entities")
	// ErrInvalidRun means a covering-run report is incomplete.
	ErrInvalidRun = errors.New("oam: invalid covering run")
)

// EdgeError reports a docs-tagged relation label used between asset types
// that spec 5.5 does not allow. It unwraps to ErrLabelEndpoints.
type EdgeError struct {
	Label       string
	FromType    string
	ToType      string
	AllowedFrom []string
	AllowedTo   []string // empty means any type
}

// Error implements error.
func (e *EdgeError) Error() string {
	to := "any"
	if len(e.AllowedTo) > 0 {
		to = strings.Join(e.AllowedTo, "|")
	}
	return fmt.Sprintf("oam: relation label %q not allowed from %s to %s (allowed %s -> %s)",
		e.Label, e.FromType, e.ToType, strings.Join(e.AllowedFrom, "|"), to)
}

// Unwrap returns ErrLabelEndpoints.
func (e *EdgeError) Unwrap() error { return ErrLabelEndpoints }

// ValidationError locates a validation failure inside an observation
// envelope. It unwraps to both ErrInvalidObservation and the cause.
type ValidationError struct {
	Path string // for example "entities[2].asset"
	Err  error
}

// Error implements error.
func (e *ValidationError) Error() string {
	return fmt.Sprintf("oam: invalid observation: %s: %v", e.Path, e.Err)
}

// Unwrap returns ErrInvalidObservation and the underlying cause.
func (e *ValidationError) Unwrap() []error { return []error{ErrInvalidObservation, e.Err} }

func invalidAt(path string, err error) error { return &ValidationError{Path: path, Err: err} }
