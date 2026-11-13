package diff

import "fmt"

// Git-style change compaction, a port of xdl_change_compact in git's
// xdiff/xdiffi.c without the indent heuristic (git diff
// --no-indent-heuristic). Git runs it after every algorithm, first over the
// old file's changed lines and then over the new file's. Each group of
// changed lines is slid up as far as possible, then down as far as
// possible, merging with groups it bumps into; it is then left at the
// lowest position unless it can line up with a change in the other file,
// in which case it is moved back up to the last such position.

// cfile is one side's change marks with sentinels: rchg[i+1] is line i,
// and lines -1 and n are never changed.
type cfile struct {
	recs []uint32
	rchg []bool
}

func newCfile(recs []uint32) *cfile { return &cfile{recs: recs, rchg: make([]bool, len(recs)+2)} }

func (f *cfile) n() int            { return len(f.recs) }
func (f *cfile) ch(i int) bool     { return f.rchg[i+1] }
func (f *cfile) set(i int, v bool) { f.rchg[i+1] = v }

// cgroup is git's struct xdlgroup: the half-open range of a run of changed
// lines (possibly empty, between two unchanged lines).
type cgroup struct{ start, end int }

// groupInit, groupNext, groupPrevious, slideDown and slideUp are git's
// group_init, group_next, group_previous, group_slide_down and
// group_slide_up.
func (f *cfile) groupInit() cgroup {
	g := cgroup{}
	for f.ch(g.end) {
		g.end++
	}
	return g
}

func (f *cfile) groupNext(g *cgroup) bool {
	if g.end == f.n() {
		return false
	}
	g.start = g.end + 1
	for g.end = g.start; f.ch(g.end); g.end++ {
	}
	return true
}

func (f *cfile) groupPrevious(g *cgroup) bool {
	if g.start == 0 {
		return false
	}
	g.end = g.start - 1
	for g.start = g.end; f.ch(g.start - 1); g.start-- {
	}
	return true
}

func (f *cfile) slideDown(g *cgroup) bool {
	if g.end < f.n() && f.recs[g.start] == f.recs[g.end] {
		f.set(g.start, false)
		g.start++
		f.set(g.end, true)
		g.end++
		for f.ch(g.end) {
			g.end++
		}
		return true
	}
	return false
}

func (f *cfile) slideUp(g *cgroup) bool {
	if g.start > 0 && f.recs[g.start-1] == f.recs[g.end-1] {
		g.start--
		f.set(g.start, true)
		g.end--
		f.set(g.end, false)
		for f.ch(g.start - 1) {
			g.start--
		}
		return true
	}
	return false
}

// compact is xdl_change_compact(xdf, xdfo) without the indent heuristic.
func compact(xdf, xdfo *cfile) error {
	g, gOther := xdf.groupInit(), xdfo.groupInit()
	for {
		if g.end != g.start {
			var earliestEnd, endMatchingOther int
			for {
				size := g.end - g.start
				endMatchingOther = -1
				for xdf.slideUp(&g) {
					if !xdfo.groupPrevious(&gOther) {
						return fmt.Errorf("%w: group sync broken sliding up", ErrInvalidScript)
					}
				}
				earliestEnd = g.end
				if gOther.end > gOther.start {
					endMatchingOther = g.end
				}
				for xdf.slideDown(&g) {
					if !xdfo.groupNext(&gOther) {
						return fmt.Errorf("%w: group sync broken sliding down", ErrInvalidScript)
					}
					if gOther.end > gOther.start {
						endMatchingOther = g.end
					}
				}
				if size == g.end-g.start {
					break
				}
			}
			if g.end != earliestEnd && endMatchingOther != -1 {
				for gOther.end == gOther.start {
					if !xdf.slideUp(&g) {
						return fmt.Errorf("%w: match disappeared", ErrInvalidScript)
					}
					if !xdfo.groupPrevious(&gOther) {
						return fmt.Errorf("%w: group sync broken sliding to match", ErrInvalidScript)
					}
				}
			}
		}
		if !xdf.groupNext(&g) {
			return nil
		}
		if !xdfo.groupNext(&gOther) {
			return fmt.Errorf("%w: group sync broken moving to next group", ErrInvalidScript)
		}
	}
}

// CompactGit applies git's change compaction to a script computed from a
// to b, returning an equivalent canonical script with the same distance
// whose change groups sit where git would put them.
func CompactGit(a, b []uint32, s EditScript) (EditScript, error) {
	if err := s.Validate(); err != nil {
		return EditScript{}, err
	}
	if len(a) != s.OldLen || len(b) != s.NewLen {
		return EditScript{}, fmt.Errorf("%w: %d/%d tokens, script expects %d/%d", ErrMismatch, len(a), len(b), s.OldLen, s.NewLen)
	}
	f1, f2 := newCfile(a), newCfile(b)
	for _, op := range s.Ops {
		for i := op.OldStart; i < op.OldEnd && op.Kind == OpDelete; i++ {
			f1.set(i, true)
		}
		for j := op.NewStart; j < op.NewEnd && op.Kind == OpInsert; j++ {
			f2.set(j, true)
		}
	}
	if err := compact(f1, f2); err != nil {
		return EditScript{}, err
	}
	if err := compact(f2, f1); err != nil {
		return EditScript{}, err
	}
	var bld builder
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		k := 0
		for i < len(a) && j < len(b) && !f1.ch(i) && !f2.ch(j) {
			i, j, k = i+1, j+1, k+1
		}
		bld.equal(k)
		d, n := 0, 0
		for i < len(a) && f1.ch(i) {
			i, d = i+1, d+1
		}
		for j < len(b) && f2.ch(j) {
			j, n = j+1, n+1
		}
		if k == 0 && d == 0 && n == 0 {
			return EditScript{}, fmt.Errorf("%w: unchanged lines do not pair up", ErrInvalidScript)
		}
		bld.del(d)
		bld.ins(n)
	}
	out := bld.finish(a, b)
	for opi, op := range out.Ops { // equal runs must really be equal
		if op.Kind == OpEqual && !equalU32s(a[op.OldStart:op.OldEnd], b[op.NewStart:op.NewEnd]) {
			return EditScript{}, fmt.Errorf("%w: compaction produced a false equality at op %d", ErrInvalidScript, opi)
		}
	}
	return out, nil
}

func equalU32s(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
