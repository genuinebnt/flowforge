package diff

// Myers' O(ND) difference algorithm (E. W. Myers, "An O(ND) Difference
// Algorithm and Its Variations", Algorithmica 1(2), 1986). Both variants
// here compute a shortest edit script (insertions and deletions only).
//
// Frontier arrays V hold, per diagonal k = x - y, the furthest x reached
// by a D-path. Unlike the paper's pseudo-code, every stored value is a
// point inside the (n+1) x (m+1) edit graph, or -1 when diagonal k is not
// reachable at this step: a "down" move (insertion) is taken only from a
// point with y < m and a "right" move (deletion) only from a point with
// x < n. Only the diagonals k ≡ d (mod 2) in [-min(d,m), min(d,n)] are
// computed at step d. That keeps the overlap test of the linear-space
// variant exact.

// stepRange returns the diagonals computed at step d: [lo, hi] with the
// parity of d, clipped to [-m, n].
func stepRange(d, n, m int) (lo, hi int) {
	lo, hi = -d, d
	if lo < -m {
		lo = -m
		if (lo+d)&1 != 0 {
			lo++
		}
	}
	if hi > n {
		hi = n
		if (hi+d)&1 != 0 {
			hi--
		}
	}
	return lo, hi
}

// startX returns the x a (d)-path on diagonal k starts its final snake at,
// given the previous step's frontier v over diagonals [plo, phi], and
// whether it got there by a down move. -1 means unreachable. Ties go to
// the down move, as in the paper (down when V[k-1] < V[k+1]).
func startX(v []int, off, plo, phi, k, n, m int) (x int, down bool) {
	xd, xr := -1, -1
	if k+1 >= plo && k+1 <= phi {
		if p := v[off+k+1]; p >= 0 && p-(k+1) < m {
			xd = p
		}
	}
	if k-1 >= plo && k-1 <= phi {
		if p := v[off+k-1]; p >= 0 && p < n {
			xr = p + 1
		}
	}
	if xd >= xr {
		return xd, true
	}
	return xr, false
}

// fallback emits the common prefix, a delete and insert of the middle, and
// the common suffix: the capped result.
func fallback(bld *builder, a, b []uint32, aLo, aHi, bLo, bHi int) {
	pre := 0
	for aLo < aHi && bLo < bHi && a[aLo] == b[bLo] {
		aLo++
		bLo++
		pre++
	}
	suf := 0
	for aHi > aLo && bHi > bLo && a[aHi-1] == b[bHi-1] {
		aHi--
		bHi--
		suf++
	}
	bld.equal(pre)
	bld.del(aHi - aLo)
	bld.ins(bHi - bLo)
	bld.equal(suf)
}

// myersGreedy is the paper's basic greedy forward algorithm with the
// frontier of every step kept for the backtrack: O((N+M)D) time and O(D^2)
// memory. limit < 0 means unlimited; when the distance exceeds limit it
// returns false and emits nothing.
func myersGreedy(bld *builder, a, b []uint32, aLo, aHi, bLo, bHi, limit int) bool {
	n, m := aHi-aLo, bHi-bLo
	maxD := n + m
	if limit >= 0 && limit < maxD {
		maxD = limit
	}
	off := m + 1
	v := make([]int, n+m+3)
	type snap struct {
		lo int
		v  []int
	}
	var trace []snap
	plo, phi := 0, -1
	endD := -1
	for d := 0; d <= maxD && endD < 0; d++ {
		lo, hi := stepRange(d, n, m)
		for k := lo; k <= hi; k += 2 {
			x := 0
			if d > 0 {
				x, _ = startX(v, off, plo, phi, k, n, m)
			}
			if x < 0 {
				v[off+k] = -1
				continue
			}
			y := x - k
			for x < n && y < m && a[aLo+x] == b[bLo+y] {
				x++
				y++
			}
			v[off+k] = x
			if x == n && y == m {
				endD = d
			}
		}
		s := snap{lo: lo, v: make([]int, hi-lo+1)}
		copy(s.v, v[off+lo:off+hi+1])
		trace = append(trace, s)
		plo, phi = lo, hi
	}
	if endD < 0 {
		return false
	}
	// Backtrack from (n, m), collecting runs in reverse.
	type run struct {
		kind OpKind
		n    int
	}
	var rev []run
	x, y := n, m
	for d := endD; d > 0; d-- {
		k := x - y
		p := trace[d-1]
		xs, down := startXSnap(p.v, p.lo, k, n, m)
		rev = append(rev, run{OpEqual, x - xs})
		if down {
			rev = append(rev, run{OpInsert, 1})
			x, y = xs, xs-k-1
		} else {
			rev = append(rev, run{OpDelete, 1})
			x, y = xs-1, xs-k
		}
	}
	rev = append(rev, run{OpEqual, x})
	for i := len(rev) - 1; i >= 0; i-- {
		switch rev[i].kind {
		case OpEqual:
			bld.equal(rev[i].n)
		case OpInsert:
			bld.ins(rev[i].n)
		case OpDelete:
			bld.del(rev[i].n)
		}
	}
	return true
}

// startXSnap is startX over a stored step (values for diagonals lo..).
func startXSnap(sv []int, lo, k, n, m int) (int, bool) {
	xd, xr := -1, -1
	if i := k + 1 - lo; i >= 0 && i < len(sv) {
		if p := sv[i]; p >= 0 && p-(k+1) < m {
			xd = p
		}
	}
	if i := k - 1 - lo; i >= 0 && i < len(sv) {
		if p := sv[i]; p >= 0 && p < n {
			xr = p + 1
		}
	}
	if xd >= xr {
		return xd, true
	}
	return xr, false
}

// linear is the linear-space refinement (paper section 4b): find the
// middle snake of an optimal path with a forward and a reverse search that
// meet halfway, emit it, and recurse on both sides. Two frontier arrays of
// N+M+3 entries are allocated once and reused by every call, so memory is
// O(N+M) beyond the output; recursion depth is O(log D).
type linear struct {
	a, b   []uint32
	vf, vb []int
	off    int
	bld    *builder
}

func newLinear(bld *builder, a, b []uint32) *linear {
	return &linear{a: a, b: b, vf: make([]int, len(a)+len(b)+3), vb: make([]int, len(a)+len(b)+3), off: len(b) + 1, bld: bld}
}

// run diffs a[aLo:aHi] against b[bLo:bHi]. limit < 0 is unlimited;
// otherwise a distance above limit returns false and emits nothing.
func (l *linear) run(aLo, aHi, bLo, bHi, limit int) bool {
	a, b := l.a, l.b
	pre := 0
	for aLo < aHi && bLo < bHi && a[aLo] == b[bLo] {
		aLo++
		bLo++
		pre++
	}
	suf := 0
	for aHi > aLo && bHi > bLo && a[aHi-1] == b[bHi-1] {
		aHi--
		bHi--
		suf++
	}
	switch {
	case aLo == aHi || bLo == bHi:
		if limit >= 0 && (aHi-aLo)+(bHi-bLo) > limit {
			return false
		}
		l.bld.equal(pre)
		l.bld.del(aHi - aLo)
		l.bld.ins(bHi - bLo)
	default:
		x0, y0, x1, y1, ok := l.midSnake(aLo, aHi, bLo, bHi, limit)
		if !ok {
			return false
		}
		l.bld.equal(pre)
		l.run(aLo, x0, bLo, y0, -1)
		l.bld.equal(x1 - x0)
		l.run(x1, aHi, y1, bHi, -1)
	}
	l.bld.equal(suf)
	return true
}

// midSnake returns the middle snake (x0, y0)-(x1, y1), in absolute indices,
// of a shortest path through a[aLo:aHi] x b[bLo:bHi]. Both ranges are
// non-empty with differing first and last tokens, so D >= 2 and both
// halves of the split have a smaller distance. ok is false when the
// distance exceeds limit (limit >= 0).
func (l *linear) midSnake(aLo, aHi, bLo, bHi, limit int) (x0, y0, x1, y1 int, ok bool) {
	a, b, vf, vb, off := l.a, l.b, l.vf, l.vb, l.off
	n, m := aHi-aLo, bHi-bLo
	delta := n - m
	odd := delta&1 != 0
	flo, fhi := 0, -1 // forward diagonals of the last completed step
	rlo, rhi := 0, -1 // reverse diagonals of the last completed step
	for d := 0; d <= (n+m+1)/2+1; d++ {
		if odd && d > 0 && limit >= 0 && 2*d-1 > limit {
			return 0, 0, 0, 0, false
		}
		// Forward step d.
		lo, hi := stepRange(d, n, m)
		for k := lo; k <= hi; k += 2 {
			x := 0
			if d > 0 {
				x, _ = startX(vf, off, flo, fhi, k, n, m)
			}
			if x < 0 {
				vf[off+k] = -1
				continue
			}
			y := x - k
			sx, sy := x, y
			for x < n && y < m && a[aLo+x] == b[bLo+y] {
				x++
				y++
			}
			vf[off+k] = x
			if odd && d > 0 {
				if kr := delta - k; kr >= rlo && kr <= rhi {
					if xr := vb[off+kr]; xr >= 0 && x+xr >= n {
						return aLo + sx, bLo + sy, aLo + x, bLo + y, true
					}
				}
			}
		}
		flo, fhi = lo, hi
		if !odd && d > 0 && limit >= 0 && 2*d > limit {
			return 0, 0, 0, 0, false
		}
		// Reverse step d, in coordinates mirrored at (n, m).
		lo, hi = stepRange(d, n, m)
		for kr := lo; kr <= hi; kr += 2 {
			x := 0
			if d > 0 {
				x, _ = startX(vb, off, rlo, rhi, kr, n, m)
			}
			if x < 0 {
				vb[off+kr] = -1
				continue
			}
			y := x - kr
			sx, sy := x, y
			for x < n && y < m && a[aHi-1-x] == b[bHi-1-y] {
				x++
				y++
			}
			vb[off+kr] = x
			if !odd {
				if k := delta - kr; k >= flo && k <= fhi {
					if xf := vf[off+k]; xf >= 0 && xf+x >= n {
						return aLo + n - x, bLo + m - y, aLo + n - sx, bLo + m - sy, true
					}
				}
			}
		}
		rlo, rhi = lo, hi
	}
	return 0, 0, 0, 0, false
}
