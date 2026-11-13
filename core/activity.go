package core

import "fmt"

// ActivityClass orders how intrusive an action is: passive < active <
// intrusive (spec 2.2, 4.1). ClassNone is used for workflows that perform
// no target-facing effect at all. The numeric order is the policy order:
// a program allows class c when c <= its max class, so the constants must
// keep their relative values.
type ActivityClass int

// Activity classes, in increasing order.
const (
	ClassNone ActivityClass = iota
	ClassPassive
	ClassActive
	ClassIntrusive
)

var classNames = [...]string{"none", "passive", "active", "intrusive"}

// String returns the lower snake case name used in JSON, SQL and the DSL
// ("none", "passive", ...), or "class(N)" for a value outside the enum.
func (c ActivityClass) String() string {
	if c >= 0 && int(c) < len(classNames) {
		return classNames[c]
	}
	return fmt.Sprintf("class(%d)", int(c))
}

// ParseActivityClass parses the lower snake case name of a class. Matching
// is exact and case-sensitive; any other text is an error.
func ParseActivityClass(s string) (ActivityClass, error) {
	for i, n := range classNames {
		if n == s {
			return ActivityClass(i), nil
		}
	}
	return ClassNone, fmt.Errorf("core: unknown activity class %q", s)
}

// MarshalText implements encoding.TextMarshaler.
func (c ActivityClass) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (c *ActivityClass) UnmarshalText(b []byte) error {
	v, err := ParseActivityClass(string(b))
	if err != nil {
		return err
	}
	*c = v
	return nil
}

// ProhibitedActivity reports the first entry of prohibited (a scope
// document's activity.prohibited list, spec 4.1) that names one of
// activities, and true; or "" and false when none does. The activities of
// a tool call are the tool's name and its declared effect names (its
// effect class): an entry such as "credential_stuffing" forbids the tool
// of that name, and "intrusive" or "financial_write" forbids every tool
// with that effect. Entries and activities are compared in
// NormalizeActivity form; an entry that is empty in that form never
// matches.
//
// Every enforcement point (the compiler's policy check, the engine at
// submission, at scope re-validation and before each tool effect) uses
// this one function, so they agree; scope/OPEN_QUESTIONS.md 19 records
// the reading.
func ProhibitedActivity(prohibited []string, activities ...string) (string, bool) {
	for _, p := range prohibited {
		pn := NormalizeActivity(p)
		if pn == "" {
			continue
		}
		for _, a := range activities {
			if NormalizeActivity(a) == pn {
				return p, true
			}
		}
	}
	return "", false
}

// NormalizeActivity is the form activity names are compared in: leading
// and trailing ASCII white space removed and ASCII letters lowercased.
//
// PORT: both steps are ASCII only (not strings.TrimSpace or
// strings.ToLower, which also act on non-ASCII runes such as U+00A0 or the
// Kelvin sign), so a port can use trim_ascii and to_ascii_lowercase.
func NormalizeActivity(s string) string {
	isSpace := func(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' }
	for len(s) > 0 && isSpace(s[0]) {
		s = s[1:]
	}
	for len(s) > 0 && isSpace(s[len(s)-1]) {
		s = s[:len(s)-1]
	}
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}
