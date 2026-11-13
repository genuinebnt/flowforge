package content

import "errors"

// Sentinel errors. Every error returned by this package wraps one of
// these, so callers test with errors.Is; the texts are stable.
var (
	// ErrInvalidProfile: a capture profile failed validation (bad name,
	// method or header).
	ErrInvalidProfile = errors.New("content: invalid capture profile")
	// ErrInvalidCapture: a capture failed validation (no URL, bad status,
	// unknown outcome).
	ErrInvalidCapture = errors.New("content: invalid capture")
	// ErrInvalidRule: a learned volatility rule is malformed.
	ErrInvalidRule = errors.New("content: invalid volatility rule")
	// ErrMaskOnFeaturePath: a learned volatility rule would mask a level-3
	// feature path (V2 §12.5: learned volatility never masks one).
	ErrMaskOnFeaturePath = errors.New("content: volatility rule targets a level-3 feature path")
	// ErrIncomparable: two artifact versions do not share (document,
	// profile), so they must not be diffed (V2 §5 capture profiles).
	ErrIncomparable = errors.New("content: artifact versions are not comparable")
	// ErrUnknownAlgorithm: a configuration names an algorithm candidate
	// that does not exist.
	ErrUnknownAlgorithm = errors.New("content: unknown algorithm")
)
