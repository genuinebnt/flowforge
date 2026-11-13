package diff

// DefaultContext is the number of context lines git and GNU diff use.
const DefaultContext = 3

// Hunk is a group of nearby change regions with up to context equal tokens
// before, between and after them. Ranges are 0-based and half-open.
type Hunk struct {
	OldStart, OldEnd int
	NewStart, NewEnd int
	// Ops are the script's ops restricted to the hunk: leading and trailing
	// Equal ops are clipped to the context.
	Ops []Op
}

// Hunks groups the changes of s with context equal tokens on each side.
// Two change regions share a hunk when at most 2*context equal tokens
// separate them (git's rule), so distinct hunks are separated by more than
// 2*context equal tokens and their contexts never overlap or touch. A
// negative context is treated as 0. A script without changes has no hunks.
func Hunks(s EditScript, context int) []Hunk {
	context = max(context, 0)
	var out []Hunk
	ops := s.Ops
	for i := 0; i < len(ops); {
		if ops[i].Kind == OpEqual {
			i++
			continue
		}
		// ops[i] starts a hunk; find its last change op.
		first, last := i, i
		for j := i + 1; j < len(ops); j++ {
			if ops[j].Kind != OpEqual {
				last = j
				continue
			}
			if ops[j].Len() > 2*context || j == len(ops)-1 {
				break
			}
		}
		var h Hunk
		if first > 0 {
			pre := ops[first-1]
			k := min(context, pre.Len())
			if k > 0 {
				h.Ops = append(h.Ops, Op{Kind: OpEqual, OldStart: pre.OldEnd - k, OldEnd: pre.OldEnd, NewStart: pre.NewEnd - k, NewEnd: pre.NewEnd})
			}
		}
		h.Ops = append(h.Ops, ops[first:last+1]...)
		if last+1 < len(ops) {
			post := ops[last+1]
			k := min(context, post.Len())
			if k > 0 {
				h.Ops = append(h.Ops, Op{Kind: OpEqual, OldStart: post.OldStart, OldEnd: post.OldStart + k, NewStart: post.NewStart, NewEnd: post.NewStart + k})
			}
		}
		h.OldStart, h.NewStart = h.Ops[0].OldStart, h.Ops[0].NewStart
		h.OldEnd, h.NewEnd = h.Ops[len(h.Ops)-1].OldEnd, h.Ops[len(h.Ops)-1].NewEnd
		out = append(out, h)
		i = last + 1
	}
	return out
}
