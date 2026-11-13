package diff

// referenceDiff is the textbook dynamic program for the longest common
// subsequence (Wagner–Fischer / Hirschberg's formulation without the space
// refinement): L[i][j] = LCS of a[i:] and b[j:], filled from the end, then
// walked forward from (0, 0) preferring a match, then a deletion (when
// L[i+1][j] >= L[i][j+1]), then an insertion. The result is a shortest
// edit script. limit < 0 is unlimited; a distance above limit returns
// false and emits nothing. The caller bounds len(a)*len(b).
func referenceDiff(bld *builder, a, b []uint32, limit int) bool {
	n, m := len(a), len(b)
	w := m + 1
	l := make([]int32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			switch {
			case a[i] == b[j]:
				l[i*w+j] = l[(i+1)*w+j+1] + 1
			case l[(i+1)*w+j] >= l[i*w+j+1]:
				l[i*w+j] = l[(i+1)*w+j]
			default:
				l[i*w+j] = l[i*w+j+1]
			}
		}
	}
	if limit >= 0 && n+m-2*int(l[0]) > limit {
		return false
	}
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && a[i] == b[j]:
			bld.equal(1)
			i, j = i+1, j+1
		case j == m || (i < n && l[(i+1)*w+j] >= l[i*w+j+1]):
			bld.del(1)
			i++
		default:
			bld.ins(1)
			j++
		}
	}
	return true
}
