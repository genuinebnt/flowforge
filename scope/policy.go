package scope

import (
	"errors"
	"fmt"
	"math"
	"net/netip"
	"slices"

	"github.com/genuinebnt/flowforge/core"
)

// Policy is a compiled, immutable scope document. It is safe for
// concurrent use by any number of goroutines.
type Policy struct {
	doc      Document
	rules    []compiledRule
	windows  []window
	asns     []uint32
	indexed  *indexedMatcher // nil when linear is set
	linear   *linearMatcher  // reference matcher, tests and benchmarks only
	resolver Resolver
}

// Option configures Compile.
type Option func(*config)

type config struct {
	resolver Resolver
	linear   bool
}

// WithResolver sets the resolver used by active checks of hostnames.
// Without one, every such check is denied with reason resolution_failed.
func WithResolver(r Resolver) Option { return func(c *config) { c.resolver = r } }

// withLinearMatcher swaps the indexed matcher for the reference linear
// scan; tests and benchmarks use it as the baseline.
func withLinearMatcher() Option { return func(c *config) { c.linear = true } }

// Compile validates doc and builds its indexes. The returned error is the
// same as doc.Validate's.
func Compile(doc Document, opts ...Option) (*Policy, error) {
	var cfg config
	for _, o := range opts {
		o(&cfg)
	}
	p, err := compileDocument(doc)
	if err != nil {
		return nil, err
	}
	p.resolver = cfg.resolver
	if cfg.linear {
		p.linear = &linearMatcher{rules: p.rules}
	} else {
		p.indexed = newIndexedMatcher(p.rules)
	}
	return p, nil
}

// first returns the index of the first rule with effect e matching t.
func (p *Policy) first(t *Target, e RuleEffect) int32 {
	if p.linear != nil {
		return p.linear.first(t, e)
	}
	return p.indexed.first(t, e)
}

// firstDenyCIDR returns the index of the first deny cidr rule containing a.
func (p *Policy) firstDenyCIDR(a netip.Addr) int32 {
	if p.linear != nil {
		return p.linear.firstDenyCIDR(a)
	}
	return p.indexed.firstDenyCIDR(a)
}

// compileDocument validates doc and compiles rules and windows, without
// building a matcher.
func compileDocument(doc Document) (*Policy, error) {
	var errs []error
	fail := func(err error) { errs = append(errs, fmt.Errorf("%w: %w", ErrInvalidDocument, err)) }

	if doc.Version < 0 {
		fail(fmt.Errorf("version %d is negative", doc.Version))
	}
	if len(doc.Rules) >= int(noRule) {
		fail(fmt.Errorf("too many rules (%d)", len(doc.Rules)))
	}
	p := &Policy{doc: cloneDocument(doc), rules: make([]compiledRule, 0, len(doc.Rules))}
	seen := make(map[string]int, len(doc.Rules))
	for i, r := range doc.Rules {
		c, err := compileRule(r)
		if err != nil {
			fail(fmt.Errorf("rule %d (%q): %w: %w", i, r.ID, ErrInvalidRule, err))
		}
		if prev, dup := seen[r.ID]; dup && r.ID != "" {
			fail(fmt.Errorf("rule %d (%q): %w (first used by rule %d)", i, r.ID, ErrDuplicateRuleID, prev))
		}
		seen[r.ID] = i
		p.rules = append(p.rules, c)
		if c.kind == RuleASN && err == nil {
			p.asns = append(p.asns, c.asn)
		}
	}
	a := doc.Activity
	if a.MaxClass < core.ClassNone || a.MaxClass > core.ClassIntrusive {
		fail(fmt.Errorf("%w: max_class %s", ErrInvalidActivity, a.MaxClass))
	}
	if !(a.Rate.RequestsPerSecond >= 0) || math.IsInf(a.Rate.RequestsPerSecond, 0) || a.Rate.Burst < 0 || a.Rate.Concurrency < 0 {
		fail(fmt.Errorf("%w: rate values must be finite and non-negative", ErrInvalidActivity))
	}
	for i, w := range a.Windows {
		cw, err := compileWindow(w)
		if err != nil {
			fail(fmt.Errorf("window %d: %w", i, err))
			continue
		}
		p.windows = append(p.windows, cw)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return p, nil
}

func cloneDocument(d Document) Document {
	d.Rules = slices.Clone(d.Rules)
	d.Activity.Prohibited = slices.Clone(d.Activity.Prohibited)
	d.Activity.Windows = slices.Clone(d.Activity.Windows)
	for i := range d.Activity.Windows {
		d.Activity.Windows[i].Days = slices.Clone(d.Activity.Windows[i].Days)
	}
	return d
}

// Document returns a copy of the document the policy was compiled from.
func (p *Policy) Document() Document { return cloneDocument(p.doc) }

// Version returns the scope version every decision of p carries.
func (p *Policy) Version() int { return p.doc.Version }

// ProgramID returns the program the scope belongs to.
func (p *Policy) ProgramID() string { return p.doc.ProgramID }

// MaxClass returns the program's highest permitted activity class.
func (p *Policy) MaxClass() core.ActivityClass { return p.doc.Activity.MaxClass }

// ASNs returns the autonomous system numbers named by asn rules, in
// document order. They seed passive discovery only and never grant
// permission (spec 4.1).
func (p *Policy) ASNs() []uint32 { return slices.Clone(p.asns) }
