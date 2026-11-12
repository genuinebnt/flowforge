package bytecode

import (
	"cmp"
	"fmt"
	"slices"
)

// FlagAnnotations is the module flag that announces the ANNOTS section
// (see Annotations). It is the only flag format version 1 defines; bit 0
// stays reserved.
const FlagAnnotations uint16 = 1 << 1

// Annotations carries the source annotations the engine enforces at run
// time (spec 7.5, 8.7, 9.5, 9.8): the workflow's #[timeout], #[effects(max)],
// #[on_unknown(retry)] and #[idempotent], and per EFFECT instruction the
// #[timeout] and #[retry] of the let statement that lexically contains it
// (including effects inside lambdas written in that statement, such as the
// closure of a par_map) and whether the call is idempotent.
//
// The section is optional: it is present exactly when the module's flags
// contain FlagAnnotations, so modules without annotations keep the plain
// spec 8.3 layout. It sits between META and entry:
//
//	ANNOTS  timeout_ns i64, max_effects u32, flags u8 (bit 0 on_unknown_retry,
//	          bit 1 idempotent), nsites u32, then per site:
//	          func u16, code_offset u32, source_offset u32, source_len u32,
//	          timeout_ns i64, retry_max u32, backoff_initial_ns i64,
//	          backoff_max_ns i64, flags u8 (bit 0 idempotent)
//
// Sites are sorted by (Func, CodeOffset) without duplicates; the verifier
// checks that each names an EFFECT instruction.
type Annotations struct {
	// Timeout is #[timeout(d)] on the workflow in nanoseconds (0: none).
	Timeout int64
	// MaxEffects is #[effects(max = N)] (0: the engine default).
	MaxEffects uint32
	// OnUnknownRetry is #[on_unknown(retry)] on the workflow.
	OnUnknownRetry bool
	// Idempotent is #[idempotent] on the workflow.
	Idempotent bool
	// Sites lists per-effect policies.
	Sites []SiteAnnotation
}

// SiteAnnotation is the policy of one EFFECT instruction.
type SiteAnnotation struct {
	// Func and CodeOffset locate the EFFECT instruction (vm.Suspended.Site
	// reports the same pair for a suspension).
	Func       uint16
	CodeOffset uint32
	// SourceOffset and SourceLen locate the annotated let statement, so
	// that the sites of one statement can be grouped.
	SourceOffset uint32
	SourceLen    uint32
	// Timeout is #[timeout(d)] on the let statement in nanoseconds (0: none).
	Timeout int64
	// RetryMax, BackoffInitial and BackoffMax come from
	// #[retry(max = n, backoff = exp(initial, max))] (RetryMax 0: none).
	RetryMax       uint32
	BackoffInitial int64
	BackoffMax     int64
	// Idempotent reports that the effect may be repeated safely: the called
	// tool or the enclosing fn is #[idempotent].
	Idempotent bool
}

// Site returns the annotation of the EFFECT at (fn, offset).
func (a *Annotations) Site(fn uint16, offset uint32) (SiteAnnotation, bool) {
	if a == nil {
		return SiteAnnotation{}, false
	}
	i, ok := slices.BinarySearchFunc(a.Sites, SiteAnnotation{Func: fn, CodeOffset: offset}, compareSites)
	if !ok {
		return SiteAnnotation{}, false
	}
	return a.Sites[i], true
}

func compareSites(x, y SiteAnnotation) int {
	if c := cmp.Compare(x.Func, y.Func); c != 0 {
		return c
	}
	return cmp.Compare(x.CodeOffset, y.CodeOffset)
}

// SortSites orders the sites as the format requires.
func (a *Annotations) SortSites() { slices.SortFunc(a.Sites, compareSites) }

const (
	annotOnUnknownRetry = 1 << 0
	annotIdempotent     = 1 << 1
	siteIdempotent      = 1 << 0
	siteSize            = 2 + 4 + 4 + 4 + 8 + 4 + 8 + 8 + 1
)

func encodeAnnotations(w *writer, a *Annotations) error {
	w.u64(uint64(a.Timeout))
	w.u32(a.MaxEffects)
	var fl uint8
	if a.OnUnknownRetry {
		fl |= annotOnUnknownRetry
	}
	if a.Idempotent {
		fl |= annotIdempotent
	}
	w.u8(fl)
	if !fits32(len(a.Sites)) {
		return fmt.Errorf("%w: too many annotated sites", ErrEncode)
	}
	w.u32(uint32(len(a.Sites)))
	for _, s := range a.Sites {
		w.u16(s.Func)
		w.u32(s.CodeOffset)
		w.u32(s.SourceOffset)
		w.u32(s.SourceLen)
		w.u64(uint64(s.Timeout))
		w.u32(s.RetryMax)
		w.u64(uint64(s.BackoffInitial))
		w.u64(uint64(s.BackoffMax))
		var sf uint8
		if s.Idempotent {
			sf |= siteIdempotent
		}
		w.u8(sf)
	}
	return nil
}

func decodeAnnotations(r *reader) *Annotations {
	a := &Annotations{Timeout: int64(r.u64()), MaxEffects: r.u32()}
	fl := r.u8()
	if fl&^(annotOnUnknownRetry|annotIdempotent) != 0 {
		r.fail(fmt.Errorf("%w: annotation flags 0x%02x", ErrBadAnnotations, fl))
		return a
	}
	a.OnUnknownRetry = fl&annotOnUnknownRetry != 0
	a.Idempotent = fl&annotIdempotent != 0
	n := r.count(uint64(r.u32()), siteSize)
	if n > 0 {
		a.Sites = make([]SiteAnnotation, n)
	}
	for i := 0; i < n && r.err == nil; i++ {
		s := SiteAnnotation{
			Func: r.u16(), CodeOffset: r.u32(), SourceOffset: r.u32(), SourceLen: r.u32(),
			Timeout: int64(r.u64()), RetryMax: r.u32(),
			BackoffInitial: int64(r.u64()), BackoffMax: int64(r.u64()),
		}
		sf := r.u8()
		if sf&^siteIdempotent != 0 {
			r.fail(fmt.Errorf("%w: site flags 0x%02x", ErrBadAnnotations, sf))
			return a
		}
		s.Idempotent = sf&siteIdempotent != 0
		a.Sites[i] = s
	}
	return a
}

// verifyAnnotations checks the ANNOTS section against the code: values in
// range, sites sorted and unique, each on an EFFECT instruction boundary.
func verifyAnnotations(m *Module) error {
	hasFlag := m.Flags&FlagAnnotations != 0
	if hasFlag != (m.Annotations != nil) {
		return &VerifyError{Rule: ErrFormatVersionFlag, Func: -1, Offset: -1, Detail: "annotation flag and section disagree"}
	}
	a := m.Annotations
	if a == nil {
		return nil
	}
	bad := func(fn, off int, format string, args ...any) error {
		return &VerifyError{Rule: ErrAnnotations, Func: fn, Offset: off, Detail: fmt.Sprintf(format, args...)}
	}
	if a.Timeout < 0 {
		return bad(-1, -1, "negative workflow timeout")
	}
	if a.MaxEffects > MaxEffectsAnnotation {
		return bad(-1, -1, "effects max %d over %d", a.MaxEffects, MaxEffectsAnnotation)
	}
	var effects map[int]bool
	curFn := -1
	for i, s := range a.Sites {
		if i > 0 && compareSites(a.Sites[i-1], s) >= 0 {
			return bad(int(s.Func), int(s.CodeOffset), "sites not sorted or duplicated")
		}
		if int(s.Func) >= len(m.Funcs) {
			return bad(-1, -1, "site names function %d", s.Func)
		}
		if int(s.Func) != curFn {
			curFn, effects = int(s.Func), effectOffsets(m.Funcs[s.Func].Code)
		}
		if !effects[int(s.CodeOffset)] {
			return bad(int(s.Func), int(s.CodeOffset), "site is not an EFFECT instruction")
		}
		if s.Timeout < 0 || s.BackoffInitial < 0 || s.BackoffMax < s.BackoffInitial {
			return bad(int(s.Func), int(s.CodeOffset), "invalid timeout or backoff")
		}
		if s.RetryMax == 0 && (s.BackoffInitial != 0 || s.BackoffMax != 0) {
			return bad(int(s.Func), int(s.CodeOffset), "backoff without retry")
		}
	}
	return nil
}

// MaxEffectsAnnotation is the largest #[effects(max = N)] (spec 8.7).
const MaxEffectsAnnotation = 1_000_000

// effectOffsets returns the offsets of the EFFECT instructions of code
// (decoding stops at the first invalid opcode; Verify rejects those).
func effectOffsets(code []byte) map[int]bool {
	out := map[int]bool{}
	for pc := 0; pc < len(code); {
		o := Opcode(code[pc])
		if !o.Valid() {
			break
		}
		if o == OpEffect {
			out[pc] = true
		}
		pc += o.Size()
	}
	return out
}
