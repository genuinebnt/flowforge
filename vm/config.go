package vm

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/bytecode"
)

// API errors. Run-time failures of the program are not Go errors: they are
// Failed outcomes with a failure code.
var (
	ErrState          = errors.New("vm: operation not allowed in the current state")
	ErrEntryArgs      = errors.New("vm: wrong number of entry arguments")
	ErrBadValue       = errors.New("vm: invalid value")
	ErrConfig         = errors.New("vm: invalid configuration")
	ErrSnapshot       = errors.New("vm: invalid snapshot")
	ErrModuleMismatch = errors.New("vm: snapshot belongs to a different module")
	ErrEncode         = errors.New("vm: value cannot be encoded")
	ErrDecode         = errors.New("vm: invalid encoded value")
	ErrProofInOutput  = errors.New("vm: ScopeProof values cannot appear in outputs")
	ErrType           = errors.New("vm: invalid type text")
	ErrJSON           = errors.New("vm: JSON does not match the expected type")
)

// Default limits (spec 8.7).
const (
	DefaultFuelLimit   = 50_000_000
	DefaultStack       = bytecode.MaxStackDepth
	DefaultCallDepth   = 256
	DefaultHeap        = 64 << 20
	DefaultString      = bytecode.MaxStringBytes
	DefaultList        = 1_000_000
	DefaultEffects     = 10_000
	MaxEffectsOverride = 1_000_000
)

// Limits are the per-run resource limits of spec 8.7. Zero fields take
// the defaults.
type Limits struct {
	Stack     int   // values on the value stack, locals included
	CallDepth int   // frames
	Heap      int64 // live heap bytes, counted by value size
	String    int   // bytes per string
	List      int   // elements per list or map
	// Effects is the maximum number of effects per run: 10,000 unless the
	// workflow declares #[effects(max = N)], in which case the engine passes
	// N here (at most 1,000,000).
	Effects int
}

func (l Limits) withDefaults() Limits {
	if l.Stack == 0 {
		l.Stack = DefaultStack
	}
	if l.CallDepth == 0 {
		l.CallDepth = DefaultCallDepth
	}
	if l.Heap == 0 {
		l.Heap = DefaultHeap
	}
	if l.String == 0 {
		l.String = DefaultString
	}
	if l.List == 0 {
		l.List = DefaultList
	}
	if l.Effects == 0 {
		l.Effects = DefaultEffects
	}
	return l
}

func (l Limits) validate() error {
	switch {
	case l.Stack < 1 || l.Stack > DefaultStack:
		return fmt.Errorf("%w: stack limit %d", ErrConfig, l.Stack)
	case l.CallDepth < 1 || l.CallDepth > DefaultCallDepth:
		return fmt.Errorf("%w: call depth limit %d", ErrConfig, l.CallDepth)
	case l.Heap < 1:
		return fmt.Errorf("%w: heap limit %d", ErrConfig, l.Heap)
	case l.String < 1 || l.String > DefaultString:
		return fmt.Errorf("%w: string limit %d", ErrConfig, l.String)
	case l.List < 1 || l.List > DefaultList:
		return fmt.Errorf("%w: list limit %d", ErrConfig, l.List)
	case l.Effects < 1 || l.Effects > MaxEffectsOverride:
		return fmt.Errorf("%w: effects limit %d", ErrConfig, l.Effects)
	}
	return nil
}

// ScopeSnapshot answers scope.allows(x) (spec 7.6): a local, passive
// pre-filter against the scope document pinned for the run. It is not an
// effect and not a proof. The engine must pass the snapshot of the run's
// pinned scope version, also on replay, so that the VM stays a pure
// function of its inputs.
type ScopeSnapshot interface {
	Allows(target Value) bool
}

// Config configures a VM.
type Config struct {
	// FuelLimit is the total fuel for the whole run (default 50,000,000;
	// agent plans use a smaller limit). Exhausting it fails the run with
	// out_of_fuel. The fuel passed to Run and Resume is a slice of it;
	// exhausting a slice yields a resumable OutOfFuel outcome.
	FuelLimit int64
	Limits    Limits
	// Scope backs the scope_allows builtin; nil makes it return false.
	Scope ScopeSnapshot
}

func (c Config) normalized() (Config, error) {
	if c.FuelLimit == 0 {
		c.FuelLimit = DefaultFuelLimit
	}
	if c.FuelLimit < 0 {
		return c, fmt.Errorf("%w: negative fuel limit", ErrConfig)
	}
	c.Limits = c.Limits.withDefaults()
	return c, c.Limits.validate()
}

// Path is a hierarchical effect id: "14" for the 15th effect of a run
// (sequence numbers start at 0), "14/3/2" for the third effect of the
// fourth par_map child of effect 14 (spec 8.6).
type Path []uint32

func (p Path) String() string {
	var sb strings.Builder
	for i, c := range p {
		if i > 0 {
			sb.WriteByte('/')
		}
		sb.WriteString(strconv.FormatUint(uint64(c), 10))
	}
	return sb.String()
}

// Child returns a copy of p extended with one component.
func (p Path) Child(i uint32) Path {
	out := make(Path, len(p)+1)
	copy(out, p)
	out[len(p)] = i
	return out
}

// Equal reports whether two paths are the same.
func (p Path) Equal(q Path) bool {
	if len(p) != len(q) {
		return false
	}
	for i := range p {
		if p[i] != q[i] {
			return false
		}
	}
	return true
}

// ParsePath parses the text form of a path.
func ParsePath(s string) (Path, error) {
	if s == "" {
		return nil, fmt.Errorf("vm: empty path")
	}
	parts := strings.Split(s, "/")
	p := make(Path, len(parts))
	for i, part := range parts {
		n, err := strconv.ParseUint(part, 10, 32)
		if err != nil || (len(part) > 1 && part[0] == '0') {
			return nil, fmt.Errorf("vm: bad path %q", s)
		}
		p[i] = uint32(n)
	}
	return p, nil
}
