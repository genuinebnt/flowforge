package alerts

import (
	"errors"
	"fmt"
	"strings"
)

// Severity orders alerts. Quiet hours delay everything below High.
type Severity int8

// Severities, low to high.
const (
	SeverityInfo Severity = iota
	SeverityLow
	SeverityMedium
	SeverityHigh
	SeverityCritical
)

// DefaultSeverity applies when a rule names none.
const DefaultSeverity = SeverityMedium

// ErrUnknownSeverity is returned by ParseSeverity.
var ErrUnknownSeverity = errors.New("alerts: unknown severity")

// severityIdent is the rule-source spelling of a severity.
func (s Severity) ident() string {
	switch s {
	case SeverityInfo:
		return "Info"
	case SeverityLow:
		return "Low"
	case SeverityMedium:
		return "Medium"
	case SeverityHigh:
		return "High"
	case SeverityCritical:
		return "Critical"
	}
	return fmt.Sprintf("Severity(%d)", int(s))
}

// String returns the lower-case wire form used in payloads ("high").
func (s Severity) String() string { return strings.ToLower(s.ident()) }

// severityFromIdent parses the exact rule-source spelling (High, not high).
func severityFromIdent(id string) (Severity, bool) {
	for s := SeverityInfo; s <= SeverityCritical; s++ {
		if s.ident() == id {
			return s, true
		}
	}
	return 0, false
}

// ParseSeverity accepts the wire form or the rule-source form.
func ParseSeverity(s string) (Severity, error) {
	for v := SeverityInfo; v <= SeverityCritical; v++ {
		if strings.EqualFold(v.ident(), s) {
			return v, nil
		}
	}
	return 0, fmt.Errorf("%w %q", ErrUnknownSeverity, s)
}

// MarshalText implements encoding.TextMarshaler.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (s *Severity) UnmarshalText(b []byte) error {
	v, err := ParseSeverity(string(b))
	if err != nil {
		return err
	}
	*s = v
	return nil
}
