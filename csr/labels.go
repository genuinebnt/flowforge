package csr

import (
	"fmt"
	"unicode/utf8"
)

// LabelSet selects edges by label index. The zero value matches every
// label; a set built with OnlyLabels or Graph.LabelSetOf matches exactly
// its members (possibly none).
type LabelSet struct {
	bits       [MaxLabels / 64]uint64
	restricted bool
}

// AnyLabel returns the unrestricted set (the zero value).
func AnyLabel() LabelSet { return LabelSet{} }

// OnlyLabels returns a set matching exactly the given label indices.
func OnlyLabels(idx ...uint8) LabelSet {
	s := LabelSet{restricted: true}
	for _, i := range idx {
		s.add(i)
	}
	return s
}

func (s *LabelSet) add(i uint8) { s.bits[i>>6] |= 1 << (i & 63) }

// Has reports whether label l is selected.
func (s LabelSet) Has(l uint8) bool {
	return !s.restricted || s.bits[l>>6]&(1<<(l&63)) != 0
}

// Restricted reports whether the set filters at all.
func (s LabelSet) Restricted() bool { return s.restricted }

// Direction selects which edges a traversal follows.
type Direction uint8

// Traversal directions, numbered like the graph.v1 Direction enum.
const (
	DirUnspecified Direction = iota
	DirOut                   // follow edges from source to target
	DirIn                    // follow edges backwards, target to source
	DirBoth                  // ignore edge direction
)

func (d Direction) String() string {
	switch d {
	case DirOut:
		return "out"
	case DirIn:
		return "in"
	case DirBoth:
		return "both"
	}
	return "unspecified"
}

// ParseDirection parses "out", "in" or "both".
func ParseDirection(s string) (Direction, error) {
	switch s {
	case "out":
		return DirOut, nil
	case "in":
		return DirIn, nil
	case "both":
		return DirBoth, nil
	}
	return DirUnspecified, fmt.Errorf("%w: direction %q", ErrInvalidInput, s)
}

func (d Direction) valid() bool { return d >= DirOut && d <= DirBoth }

// validateLabels checks a label dictionary: at most MaxLabels entries,
// valid UTF-8 of at most 65535 bytes, no duplicates.
func validateLabels(labels []string) error {
	if len(labels) > MaxLabels {
		return fmt.Errorf("%w: %d labels, at most %d", ErrInvalidInput, len(labels), MaxLabels)
	}
	seen := make(map[string]struct{}, len(labels))
	for _, l := range labels {
		if len(l) > 0xffff || !utf8.ValidString(l) {
			return fmt.Errorf("%w: label %q is not short valid UTF-8", ErrInvalidInput, l)
		}
		if _, dup := seen[l]; dup {
			return fmt.Errorf("%w: duplicate label %q", ErrInvalidInput, l)
		}
		seen[l] = struct{}{}
	}
	return nil
}
