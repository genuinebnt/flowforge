package oam

import (
	"maps"
	"math"
	"slices"
)

// MaxConfidence caps combined confidence (spec 5.7): no amount of
// agreement makes an observation certain.
const MaxConfidence = 0.99

// SourceReport is one source's confidence (0..1) in an entity or edge.
type SourceReport struct {
	Source     string
	Confidence float64
}

// Sources maps each distinct source to the highest confidence (0..1) it
// reported. Its union keeps the per-source maximum, so merging is
// commutative, associative and idempotent (spec 23.5).
type Sources map[string]float64

// Observe records a report and reports whether it raised the source's
// confidence. Values outside [0, 1] are clamped; NaN counts as 0.
func (s Sources) Observe(source string, c float64) bool {
	c = clamp01(c)
	if old, ok := s[source]; ok && old >= c {
		return false
	}
	s[source] = c
	return true
}

// Union returns a new Sources holding the per-source maximum of s and o.
func (s Sources) Union(o Sources) Sources {
	out := make(Sources, len(s)+len(o))
	for k, v := range s {
		out.Observe(k, v)
	}
	for k, v := range o {
		out.Observe(k, v)
	}
	return out
}

// Confidence combines the sources by noisy-or, 1 - Π(1 - cᵢ), evaluated in
// source-name order, rounded to four decimals and capped at
// MaxConfidence. No sources gives 0.
func (s Sources) Confidence() float64 {
	if len(s) == 0 {
		return 0
	}
	prod := 1.0
	for _, k := range slices.Sorted(maps.Keys(s)) {
		prod *= 1 - s[k]
	}
	return min(math.Round((1-prod)*1e4)/1e4, MaxConfidence)
}

// CombineConfidence combines reports that may repeat a source: each
// source counts once, with its highest confidence (spec 5.7, 23.5).
func CombineConfidence(reports ...SourceReport) float64 {
	s := make(Sources, len(reports))
	for _, r := range reports {
		s.Observe(r.Source, r.Confidence)
	}
	return s.Confidence()
}

// ConfidencePercent converts a 0..1 confidence to the integer percent a
// SourceProperty carries.
func ConfidencePercent(c float64) int {
	return int(math.Round(clamp01(c) * 100))
}

func clamp01(c float64) float64 {
	switch {
	case math.IsNaN(c) || c < 0:
		return 0
	case c > 1:
		return 1
	}
	return c
}
