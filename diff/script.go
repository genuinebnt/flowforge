package diff

import (
	"fmt"
	"slices"
	"strings"
)

// OpKind is the kind of one edit operation.
type OpKind uint8

// Edit operation kinds. The zero value is not a valid kind.
const (
	// OpEqual keeps old[OldStart:OldEnd] (== new[NewStart:NewEnd]).
	OpEqual OpKind = iota + 1
	// OpDelete removes old[OldStart:OldEnd]; NewStart == NewEnd.
	OpDelete
	// OpInsert adds new[NewStart:NewEnd]; OldStart == OldEnd.
	OpInsert
)

// String returns "equal", "delete" or "insert".
func (k OpKind) String() string {
	switch k {
	case OpEqual:
		return "equal"
	case OpDelete:
		return "delete"
	case OpInsert:
		return "insert"
	default:
		return fmt.Sprintf("OpKind(%d)", uint8(k))
	}
}

// Op is one edit operation over half-open index ranges of the old and new
// sequences.
type Op struct {
	Kind     OpKind
	OldStart int
	OldEnd   int
	NewStart int
	NewEnd   int
}

// Len returns the number of tokens the op covers on its non-empty side.
func (o Op) Len() int {
	if o.Kind == OpInsert {
		return o.NewEnd - o.NewStart
	}
	return o.OldEnd - o.OldStart
}

// EditScript transforms an old token sequence into a new one. It is
// self-contained: besides the ops it carries the deleted and inserted
// tokens, so Apply needs only the old sequence and Invert needs nothing.
//
// Canonical form (Validate): ops are non-empty and tile [0, OldLen) and
// [0, NewLen) in order; no two Equal ops are adjacent; between two Equal
// ops (or the ends) there is one change region made of at most one Delete
// followed by at most one Insert. Every algorithm, Invert, Compose and
// CleanupSemantic return canonical scripts, so two scripts describe the
// same alignment exactly when they are equal.
type EditScript struct {
	OldLen int
	NewLen int
	Ops    []Op
	// Deleted holds the tokens of every Delete op, concatenated in op order.
	Deleted []uint32
	// Inserted holds the tokens of every Insert op, concatenated in op order.
	Inserted []uint32
}

// Distance returns the number of deleted plus inserted tokens, the edit
// distance (insertions and deletions only) the script realises.
func (s EditScript) Distance() int { return len(s.Deleted) + len(s.Inserted) }

// Changes returns the number of change regions.
func (s EditScript) Changes() int {
	n := 0
	for i, o := range s.Ops {
		if o.Kind != OpEqual && (i == 0 || s.Ops[i-1].Kind == OpEqual) {
			n++
		}
	}
	return n
}

// String renders the ops compactly ("=3 -1 +2": equal, delete and insert
// run lengths). For a canonical script this determines every range by
// tiling, so it is also the conformance format of scripts.json.
func (s EditScript) String() string {
	var b strings.Builder
	for i, o := range s.Ops {
		if i > 0 {
			b.WriteByte(' ')
		}
		switch o.Kind {
		case OpEqual:
			b.WriteByte('=')
		case OpDelete:
			b.WriteByte('-')
		case OpInsert:
			b.WriteByte('+')
		default:
			b.WriteByte('?')
		}
		fmt.Fprintf(&b, "%d", o.Len())
	}
	return b.String()
}

// Equal reports whether two scripts are identical.
func (s EditScript) Equal(t EditScript) bool {
	return s.OldLen == t.OldLen && s.NewLen == t.NewLen && slices.Equal(s.Ops, t.Ops) &&
		slices.Equal(s.Deleted, t.Deleted) && slices.Equal(s.Inserted, t.Inserted)
}

// Validate checks the canonical form described on EditScript.
func (s EditScript) Validate() error {
	o, n, del, ins := 0, 0, 0, 0
	prev := OpKind(0)
	for i, op := range s.Ops {
		if op.OldStart != o || op.NewStart != n {
			return fmt.Errorf("%w: op %d starts at (%d, %d), want (%d, %d)", ErrInvalidScript, i, op.OldStart, op.NewStart, o, n)
		}
		lo, ln := op.OldEnd-op.OldStart, op.NewEnd-op.NewStart
		switch op.Kind {
		case OpEqual:
			if lo <= 0 || lo != ln || prev == OpEqual {
				return fmt.Errorf("%w: op %d: bad equal", ErrInvalidScript, i)
			}
		case OpDelete:
			if lo <= 0 || ln != 0 || prev == OpDelete || prev == OpInsert {
				return fmt.Errorf("%w: op %d: bad delete", ErrInvalidScript, i)
			}
			del += lo
		case OpInsert:
			if ln <= 0 || lo != 0 || prev == OpInsert {
				return fmt.Errorf("%w: op %d: bad insert", ErrInvalidScript, i)
			}
			ins += ln
		default:
			return fmt.Errorf("%w: op %d: unknown kind %d", ErrInvalidScript, i, op.Kind)
		}
		o, n, prev = op.OldEnd, op.NewEnd, op.Kind
	}
	if o != s.OldLen || n != s.NewLen {
		return fmt.Errorf("%w: ops end at (%d, %d), want (%d, %d)", ErrInvalidScript, o, n, s.OldLen, s.NewLen)
	}
	if del != len(s.Deleted) || ins != len(s.Inserted) {
		return fmt.Errorf("%w: %d deleted / %d inserted tokens, ops need %d / %d", ErrInvalidScript, len(s.Deleted), len(s.Inserted), del, ins)
	}
	return nil
}

// Apply returns the new sequence: old transformed by s. It fails with
// ErrInvalidScript for a non-canonical script and ErrMismatch when old is
// not the sequence s was computed from (length, or a deleted token,
// differs).
func Apply(old []uint32, s EditScript) ([]uint32, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if len(old) != s.OldLen {
		return nil, fmt.Errorf("%w: old has %d tokens, script expects %d", ErrMismatch, len(old), s.OldLen)
	}
	out := make([]uint32, 0, s.NewLen)
	del, ins := 0, 0
	for _, op := range s.Ops {
		switch op.Kind {
		case OpEqual:
			out = append(out, old[op.OldStart:op.OldEnd]...)
		case OpDelete:
			k := op.OldEnd - op.OldStart
			if !slices.Equal(old[op.OldStart:op.OldEnd], s.Deleted[del:del+k]) {
				return nil, fmt.Errorf("%w: deleted tokens at %d differ", ErrMismatch, op.OldStart)
			}
			del += k
		case OpInsert:
			k := op.NewEnd - op.NewStart
			out = append(out, s.Inserted[ins:ins+k]...)
			ins += k
		}
	}
	return out, nil
}

// Invert returns the script that turns s's new sequence back into its old
// one: Apply(Apply(old, s), Invert(s)) == old. Invert(Invert(s)) == s.
func Invert(s EditScript) (EditScript, error) {
	if err := s.Validate(); err != nil {
		return EditScript{}, err
	}
	var b builder
	for _, op := range s.Ops {
		switch op.Kind {
		case OpEqual:
			b.equal(op.Len())
		case OpDelete:
			b.ins(op.Len())
		case OpInsert:
			b.del(op.Len())
		}
	}
	out := b.finishOps(s.NewLen, s.OldLen)
	// Each change region keeps one delete and one insert, so swapping the
	// token arrays keeps them in op order.
	out.Deleted = slices.Clone(s.Inserted)
	out.Inserted = slices.Clone(s.Deleted)
	return out, nil
}

// Compose returns the script for applying s1 (A→B) and then s2 (B→C): A→C.
// It needs neither A nor B. It fails with ErrMismatch when s1's new length
// differs from s2's old length or when s2 deletes a token of B that s1
// inserted with a different value.
func Compose(s1, s2 EditScript) (EditScript, error) {
	if err := s1.Validate(); err != nil {
		return EditScript{}, err
	}
	if err := s2.Validate(); err != nil {
		return EditScript{}, err
	}
	if s1.NewLen != s2.OldLen {
		return EditScript{}, fmt.Errorf("%w: first script yields %d tokens, second expects %d", ErrMismatch, s1.NewLen, s2.OldLen)
	}
	var b builder
	b.carry = true
	i, j := 0, 0   // op indices
	ri, rj := 0, 0 // tokens of the current op already consumed
	d1, n1 := 0, 0 // cursors into s1.Deleted / s1.Inserted
	d2, n2 := 0, 0 // cursors into s2.Deleted / s2.Inserted
	for i < len(s1.Ops) || j < len(s2.Ops) {
		switch {
		case i < len(s1.Ops) && s1.Ops[i].Kind == OpDelete:
			k := s1.Ops[i].Len()
			b.delTokens(s1.Deleted[d1 : d1+k])
			d1 += k
			i++
		case j < len(s2.Ops) && s2.Ops[j].Kind == OpInsert:
			k := s2.Ops[j].Len()
			b.insTokens(s2.Inserted[n2 : n2+k])
			n2 += k
			j++
		case i < len(s1.Ops) && j < len(s2.Ops):
			op1, op2 := s1.Ops[i], s2.Ops[j]
			k := min(op1.Len()-ri, op2.Len()-rj)
			switch {
			case op1.Kind == OpEqual && op2.Kind == OpEqual:
				b.equal(k)
			case op1.Kind == OpEqual: // s2 deletes tokens of A that s1 kept
				b.delTokens(s2.Deleted[d2 : d2+k])
				d2 += k
			case op2.Kind == OpEqual: // s2 keeps tokens s1 inserted
				b.insTokens(s1.Inserted[n1 : n1+k])
				n1 += k
			default: // s1 inserted them, s2 deletes them: they vanish
				if !slices.Equal(s1.Inserted[n1:n1+k], s2.Deleted[d2:d2+k]) {
					return EditScript{}, fmt.Errorf("%w: second script deletes tokens the first did not insert", ErrMismatch)
				}
				n1 += k
				d2 += k
			}
			ri += k
			rj += k
			if ri == op1.Len() {
				i, ri = i+1, 0
			}
			if rj == op2.Len() {
				j, rj = j+1, 0
			}
		default:
			return EditScript{}, fmt.Errorf("%w: scripts do not line up", ErrInvalidScript)
		}
	}
	return b.finishOps(s1.OldLen, s2.NewLen), nil
}

// builder assembles canonical scripts from a stream of equal/delete/insert
// runs given in order. Deletes and inserts between two equal runs collapse
// into one Delete followed by one Insert. With carry set, tokens come from
// delTokens/insTokens; otherwise finish copies them from the sequences.
type builder struct {
	ops      []Op
	o, n     int
	pd, pi   int
	carry    bool
	delBuf   []uint32
	insBuf   []uint32
	deleted  []uint32
	inserted []uint32
}

func (b *builder) equal(k int) {
	if k <= 0 {
		return
	}
	b.flush()
	if l := len(b.ops); l > 0 && b.ops[l-1].Kind == OpEqual {
		b.ops[l-1].OldEnd += k
		b.ops[l-1].NewEnd += k
	} else {
		b.ops = append(b.ops, Op{Kind: OpEqual, OldStart: b.o, OldEnd: b.o + k, NewStart: b.n, NewEnd: b.n + k})
	}
	b.o += k
	b.n += k
}

func (b *builder) del(k int) { b.pd += max(k, 0) }
func (b *builder) ins(k int) { b.pi += max(k, 0) }

func (b *builder) delTokens(t []uint32) {
	b.pd += len(t)
	b.delBuf = append(b.delBuf, t...)
}

func (b *builder) insTokens(t []uint32) {
	b.pi += len(t)
	b.insBuf = append(b.insBuf, t...)
}

func (b *builder) flush() {
	if b.pd > 0 {
		b.ops = append(b.ops, Op{Kind: OpDelete, OldStart: b.o, OldEnd: b.o + b.pd, NewStart: b.n, NewEnd: b.n})
		b.o += b.pd
	}
	if b.pi > 0 {
		b.ops = append(b.ops, Op{Kind: OpInsert, OldStart: b.o, OldEnd: b.o, NewStart: b.n, NewEnd: b.n + b.pi})
		b.n += b.pi
	}
	b.pd, b.pi = 0, 0
	if b.carry {
		b.deleted = append(b.deleted, b.delBuf...)
		b.inserted = append(b.inserted, b.insBuf...)
		b.delBuf, b.insBuf = b.delBuf[:0], b.insBuf[:0]
	}
}

// finishOps closes the script; with carry the token arrays are the ones
// collected, otherwise they are left empty for the caller.
func (b *builder) finishOps(oldLen, newLen int) EditScript {
	b.flush()
	return EditScript{OldLen: oldLen, NewLen: newLen, Ops: b.ops, Deleted: b.deleted, Inserted: b.inserted}
}

// finish closes the script and copies the changed tokens from a and bb.
func (b *builder) finish(a, bb []uint32) EditScript {
	s := b.finishOps(len(a), len(bb))
	nd, ni := 0, 0
	for _, op := range s.Ops {
		switch op.Kind {
		case OpDelete:
			nd += op.OldEnd - op.OldStart
		case OpInsert:
			ni += op.NewEnd - op.NewStart
		}
	}
	s.Deleted = make([]uint32, 0, nd)
	s.Inserted = make([]uint32, 0, ni)
	for _, op := range s.Ops {
		switch op.Kind {
		case OpDelete:
			s.Deleted = append(s.Deleted, a[op.OldStart:op.OldEnd]...)
		case OpInsert:
			s.Inserted = append(s.Inserted, bb[op.NewStart:op.NewEnd]...)
		}
	}
	return s
}
