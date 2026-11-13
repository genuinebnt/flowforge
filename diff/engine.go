package diff

// engine runs one diff over a and b, emitting into a builder. Patience and
// histogram recurse over sub-ranges through an explicit work stack (so the
// Rust port needs no deep recursion) and fall back to linear-space Myers,
// bounded by limit, where git's implementations fall back to classic diff.
type engine struct {
	a, b   []uint32
	bld    builder
	limit  int // Myers cap; -1 unlimited
	capped bool
	lin    *linear
	stack  []work
	hist   *histIndex // histogram scratch, nil for other algorithms
}

// work is one pending item: an equal run to emit, or a range to diff.
type work struct {
	equal              int // > 0: emit an equal run of this length
	lo1, hi1, lo2, hi2 int
}

func newEngine(a, b []uint32, limit int) *engine {
	return &engine{a: a, b: b, limit: limit}
}

// myers diffs a range with linear-space Myers under the engine's limit,
// emitting a delete+insert of the trimmed middle when it is exceeded.
func (e *engine) myers(lo1, hi1, lo2, hi2 int) {
	if e.lin == nil {
		e.lin = newLinear(&e.bld, e.a, e.b)
	}
	if !e.lin.run(lo1, hi1, lo2, hi2, e.limit) {
		e.capped = true
		fallback(&e.bld, e.a, e.b, lo1, hi1, lo2, hi2)
	}
}

// pushOrdered pushes items so that they are processed in slice order.
func (e *engine) pushOrdered(items []work) {
	for i := len(items) - 1; i >= 0; i-- {
		e.stack = append(e.stack, items[i])
	}
}

// drain processes the stack with step handling each range.
func (e *engine) drain(step func(lo1, hi1, lo2, hi2 int)) {
	for len(e.stack) > 0 {
		w := e.stack[len(e.stack)-1]
		e.stack = e.stack[:len(e.stack)-1]
		if w.equal > 0 {
			e.bld.equal(w.equal)
			continue
		}
		step(w.lo1, w.hi1, w.lo2, w.hi2)
	}
}
