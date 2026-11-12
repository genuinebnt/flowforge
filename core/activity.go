package core

import "fmt"

// ActivityClass orders how intrusive an action is: passive < active <
// intrusive (spec 2.2, 4.1). ClassNone is used for workflows that perform
// no target-facing effect at all.
type ActivityClass int

// Activity classes, in increasing order.
const (
	ClassNone ActivityClass = iota
	ClassPassive
	ClassActive
	ClassIntrusive
)

var classNames = [...]string{"none", "passive", "active", "intrusive"}

func (c ActivityClass) String() string {
	if c >= 0 && int(c) < len(classNames) {
		return classNames[c]
	}
	return fmt.Sprintf("class(%d)", int(c))
}

// ParseActivityClass parses the lower snake case name of a class.
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
