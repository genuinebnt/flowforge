package diff

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// PatchKind is an RFC 6902 operation.
type PatchKind uint8

// RFC 6902 operations (section 4).
const (
	PatchAdd PatchKind = iota + 1
	PatchRemove
	PatchReplace
	PatchMove
	PatchCopy
	PatchTest
)

var patchNames = [...]string{PatchAdd: "add", PatchRemove: "remove", PatchReplace: "replace", PatchMove: "move", PatchCopy: "copy", PatchTest: "test"}

// String returns the operation's RFC 6902 name.
func (k PatchKind) String() string {
	if int(k) < len(patchNames) && patchNames[k] != "" {
		return patchNames[k]
	}
	return fmt.Sprintf("PatchKind(%d)", uint8(k))
}

// PatchOp is one RFC 6902 operation. Path and From are RFC 6901 JSON
// Pointers; From is used by move and copy, Value by add, replace and test.
type PatchOp struct {
	Op    PatchKind
	Path  string
	From  string
	Value Value
}

// JSONOptions configures JSONDiff.
type JSONOptions struct {
	// MaxArrayEditDistance caps the Myers search over each array's element
	// ids; an array whose cap is hit is replaced as a whole. Zero means
	// unlimited.
	MaxArrayEditDistance int
}

// JSONDiff returns an RFC 6902 patch turning a into b, using only add,
// remove and replace: ApplyPatch(a, JSONDiff(a, b)) is Equal to b.
// Structurally equal values (Equal) produce no operation. Objects are
// compared key by key in bytewise key order: a key only in a is removed, a
// key only in b is added, a key in both is diffed recursively. Arrays are
// aligned by linear-space Myers over hash-consed element ids; within a
// change region the i-th deleted and i-th inserted elements are paired
// (recursively diffed when both are objects or both arrays, replaced
// otherwise), surplus deletions are removed and surplus insertions added.
// Array indices in paths are those of the partially patched array, so the
// operations apply in order. A changed scalar or a kind change is a
// replace; the root is replaced with path "".
func JSONDiff(a, b Value, opts JSONOptions) []PatchOp {
	d := jsonDiffer{h: newHashCons(), opts: opts}
	d.value("", a, b)
	return d.ops
}

type jsonDiffer struct {
	h    *hashCons
	opts JSONOptions
	ops  []PatchOp
}

func (d *jsonDiffer) value(path string, a, b Value) {
	if Equal(a, b) {
		return
	}
	switch {
	case a.Kind == KindObject && b.Kind == KindObject:
		d.object(path, a, b)
	case a.Kind == KindArray && b.Kind == KindArray:
		d.array(path, a, b)
	default:
		d.ops = append(d.ops, PatchOp{Op: PatchReplace, Path: path, Value: cloneValue(b)})
	}
}

func (d *jsonDiffer) object(path string, a, b Value) {
	keys := make([]string, 0, len(a.Members)+len(b.Members))
	for _, m := range a.Members {
		keys = append(keys, m.Key)
	}
	for _, m := range b.Members {
		keys = append(keys, m.Key)
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	for _, k := range keys {
		av, inA := a.Get(k)
		bv, inB := b.Get(k)
		p := path + "/" + EscapePointerToken(k)
		switch {
		case inA && inB:
			d.value(p, av, bv)
		case inA:
			d.ops = append(d.ops, PatchOp{Op: PatchRemove, Path: p})
		default:
			d.ops = append(d.ops, PatchOp{Op: PatchAdd, Path: p, Value: cloneValue(bv)})
		}
	}
}

func (d *jsonDiffer) array(path string, a, b Value) {
	ia := make([]uint32, len(a.Items))
	for i, v := range a.Items {
		ia[i] = d.h.id(v)
	}
	ib := make([]uint32, len(b.Items))
	for i, v := range b.Items {
		ib[i] = d.h.id(v)
	}
	res, err := Diff(ia, ib, Options{Algorithm: Myers, MaxEditDistance: max(d.opts.MaxArrayEditDistance, 0)})
	if err != nil || res.Capped {
		d.ops = append(d.ops, PatchOp{Op: PatchReplace, Path: path, Value: cloneValue(b)})
		return
	}
	idx := 0 // index in the partially patched array
	ops := res.Script.Ops
	for i := 0; i < len(ops); i++ {
		op := ops[i]
		switch op.Kind {
		case OpEqual:
			idx += op.Len()
		case OpInsert:
			for k := op.NewStart; k < op.NewEnd; k++ {
				d.ops = append(d.ops, PatchOp{Op: PatchAdd, Path: path + "/" + strconv.Itoa(idx), Value: cloneValue(b.Items[k])})
				idx++
			}
		case OpDelete:
			var ins Op
			if i+1 < len(ops) && ops[i+1].Kind == OpInsert {
				ins = ops[i+1]
				i++
			}
			nd, ni := op.Len(), ins.NewEnd-ins.NewStart
			for k := 0; k < min(nd, ni); k++ {
				av, bv := a.Items[op.OldStart+k], b.Items[ins.NewStart+k]
				p := path + "/" + strconv.Itoa(idx)
				if av.Kind == bv.Kind && (av.Kind == KindObject || av.Kind == KindArray) {
					d.value(p, av, bv)
				} else {
					d.ops = append(d.ops, PatchOp{Op: PatchReplace, Path: p, Value: cloneValue(bv)})
				}
				idx++
			}
			for k := ni; k < nd; k++ {
				d.ops = append(d.ops, PatchOp{Op: PatchRemove, Path: path + "/" + strconv.Itoa(idx)})
			}
			for k := nd; k < ni; k++ {
				d.ops = append(d.ops, PatchOp{Op: PatchAdd, Path: path + "/" + strconv.Itoa(idx), Value: cloneValue(b.Items[ins.NewStart+k])})
				idx++
			}
		}
	}
}

// EscapePointerToken escapes one reference token for a JSON Pointer
// (RFC 6901 section 3): "~" becomes "~0" and "/" becomes "~1".
func EscapePointerToken(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1")
}

// ParsePointer splits an RFC 6901 JSON Pointer into unescaped reference
// tokens. "" is the whole document (no tokens). Errors wrap
// ErrInvalidPointer.
func ParsePointer(p string) ([]string, error) {
	if p == "" {
		return nil, nil
	}
	if p[0] != '/' {
		return nil, fmt.Errorf("%w: %q does not start with '/'", ErrInvalidPointer, p)
	}
	parts := strings.Split(p[1:], "/")
	for i, t := range parts {
		for j := 0; j < len(t); j++ {
			if t[j] == '~' && (j+1 >= len(t) || (t[j+1] != '0' && t[j+1] != '1')) {
				return nil, fmt.Errorf("%w: bad escape in %q", ErrInvalidPointer, p)
			}
		}
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
	}
	return parts, nil
}

// arrayIndex parses an RFC 6901 array index ("0" or a digit string without
// a leading zero) bounded by limit (inclusive).
func arrayIndex(tok string, limit int) (int, bool) {
	if tok == "" || len(tok) > 10 || (len(tok) > 1 && tok[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(tok); i++ {
		if tok[i] < '0' || tok[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(tok)
	if err != nil || n > limit {
		return 0, false
	}
	return n, true
}

// MarshalPatch encodes ops as an RFC 6902 JSON document with members in
// the order op, path, from, value.
func MarshalPatch(ops []PatchOp) []byte {
	v := Value{Kind: KindArray, Items: []Value{}}
	for _, op := range ops {
		o := Value{Kind: KindObject, Members: []Member{
			{Key: "op", Value: StringValue(op.Op.String())},
			{Key: "path", Value: StringValue(op.Path)},
		}}
		if op.Op == PatchMove || op.Op == PatchCopy {
			o.Members = append(o.Members, Member{Key: "from", Value: StringValue(op.From)})
		}
		if op.Op == PatchAdd || op.Op == PatchReplace || op.Op == PatchTest {
			o.Members = append(o.Members, Member{Key: "value", Value: op.Value})
		}
		v.Items = append(v.Items, o)
	}
	return EncodeJSON(v)
}

// ParsePatch decodes an RFC 6902 document: an array of objects, each with a
// known "op", a "path", a "from" for move and copy and a "value" for add,
// replace and test. Unknown members are ignored (RFC 6902 section 4);
// duplicate members are an error (ParseJSON rejects them). Errors wrap
// ErrInvalidPatch, ErrInvalidJSON or ErrInvalidPointer.
func ParsePatch(b []byte) ([]PatchOp, error) {
	doc, err := ParseJSON(b)
	if err != nil {
		return nil, err
	}
	if doc.Kind != KindArray {
		return nil, fmt.Errorf("%w: document is not an array", ErrInvalidPatch)
	}
	ops := make([]PatchOp, 0, len(doc.Items))
	for i, it := range doc.Items {
		if it.Kind != KindObject {
			return nil, fmt.Errorf("%w: operation %d is not an object", ErrInvalidPatch, i)
		}
		str := func(key string) (string, bool, error) {
			v, ok := it.Get(key)
			if !ok {
				return "", false, nil
			}
			if v.Kind != KindString {
				return "", true, fmt.Errorf("%w: operation %d: %q is not a string", ErrInvalidPatch, i, key)
			}
			return v.Text, true, nil
		}
		name, ok, err := str("op")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: operation %d has no \"op\"", ErrInvalidPatch, i)
		}
		var op PatchOp
		for k, n := range patchNames {
			if n != "" && n == name {
				op.Op = PatchKind(k)
			}
		}
		if op.Op == 0 {
			return nil, fmt.Errorf("%w: operation %d: unknown op %q", ErrInvalidPatch, i, name)
		}
		if op.Path, ok, err = str("path"); err != nil {
			return nil, err
		} else if !ok {
			return nil, fmt.Errorf("%w: operation %d has no \"path\"", ErrInvalidPatch, i)
		}
		if _, err := ParsePointer(op.Path); err != nil {
			return nil, err
		}
		if op.Op == PatchMove || op.Op == PatchCopy {
			if op.From, ok, err = str("from"); err != nil {
				return nil, err
			} else if !ok {
				return nil, fmt.Errorf("%w: operation %d has no \"from\"", ErrInvalidPatch, i)
			}
			if _, err := ParsePointer(op.From); err != nil {
				return nil, err
			}
		}
		if op.Op == PatchAdd || op.Op == PatchReplace || op.Op == PatchTest {
			v, ok := it.Get("value")
			if !ok {
				return nil, fmt.Errorf("%w: operation %d has no \"value\"", ErrInvalidPatch, i)
			}
			op.Value = v
		}
		ops = append(ops, op)
	}
	return ops, nil
}

// ApplyPatch applies ops in order to a copy of doc (RFC 6902 section 4)
// and returns the result; doc is not modified. The first failing operation
// stops the patch: a missing target wraps ErrPatchTarget, a failed test
// ErrPatchTest, a bad pointer ErrInvalidPointer.
func ApplyPatch(doc Value, ops []PatchOp) (Value, error) {
	root := cloneValue(doc)
	for i, op := range ops {
		if err := applyOp(&root, op); err != nil {
			return Value{}, fmt.Errorf("operation %d (%s %s): %w", i, op.Op, op.Path, err)
		}
	}
	return root, nil
}

func applyOp(root *Value, op PatchOp) error {
	path, err := ParsePointer(op.Path)
	if err != nil {
		return err
	}
	switch op.Op {
	case PatchAdd:
		return addAt(root, path, cloneValue(op.Value))
	case PatchRemove:
		_, err := removeAt(root, path)
		return err
	case PatchReplace:
		t, err := locate(root, path)
		if err != nil {
			return err
		}
		*t = cloneValue(op.Value)
		return nil
	case PatchMove:
		from, err := ParsePointer(op.From)
		if err != nil {
			return err
		}
		if op.From == op.Path {
			_, err := locate(root, from)
			return err
		}
		if len(from) < len(path) && slices.Equal(from, path[:len(from)]) {
			return fmt.Errorf("%w: cannot move %q into its own child %q", ErrPatchTarget, op.From, op.Path)
		}
		v, err := removeAt(root, from)
		if err != nil {
			return err
		}
		return addAt(root, path, v)
	case PatchCopy:
		from, err := ParsePointer(op.From)
		if err != nil {
			return err
		}
		v, err := locate(root, from)
		if err != nil {
			return err
		}
		return addAt(root, path, cloneValue(*v))
	case PatchTest:
		t, err := locate(root, path)
		if err != nil {
			return err
		}
		if !Equal(*t, op.Value) {
			return fmt.Errorf("%w: value at %q differs", ErrPatchTest, op.Path)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown op %d", ErrInvalidPatch, op.Op)
	}
}

// locate returns the value at path.
func locate(root *Value, path []string) (*Value, error) {
	cur := root
	for _, tok := range path {
		switch cur.Kind {
		case KindObject:
			found := false
			for i := range cur.Members {
				if cur.Members[i].Key == tok {
					cur = &cur.Members[i].Value
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("%w: no member %q", ErrPatchTarget, tok)
			}
		case KindArray:
			i, ok := arrayIndex(tok, len(cur.Items)-1)
			if !ok {
				return nil, fmt.Errorf("%w: bad array index %q", ErrPatchTarget, tok)
			}
			cur = &cur.Items[i]
		default:
			return nil, fmt.Errorf("%w: %s has no child %q", ErrPatchTarget, cur.Kind, tok)
		}
	}
	return cur, nil
}

func addAt(root *Value, path []string, v Value) error {
	if len(path) == 0 {
		*root = v
		return nil
	}
	parent, err := locate(root, path[:len(path)-1])
	if err != nil {
		return err
	}
	last := path[len(path)-1]
	switch parent.Kind {
	case KindObject:
		for i := range parent.Members {
			if parent.Members[i].Key == last {
				parent.Members[i].Value = v
				return nil
			}
		}
		parent.Members = append(parent.Members, Member{Key: last, Value: v})
		return nil
	case KindArray:
		if last == "-" {
			parent.Items = append(parent.Items, v)
			return nil
		}
		i, ok := arrayIndex(last, len(parent.Items))
		if !ok {
			return fmt.Errorf("%w: bad array index %q", ErrPatchTarget, last)
		}
		parent.Items = slices.Insert(parent.Items, i, v)
		return nil
	default:
		return fmt.Errorf("%w: cannot add to a %s", ErrPatchTarget, parent.Kind)
	}
}

func removeAt(root *Value, path []string) (Value, error) {
	if len(path) == 0 {
		return Value{}, fmt.Errorf("%w: cannot remove the document root", ErrPatchTarget)
	}
	parent, err := locate(root, path[:len(path)-1])
	if err != nil {
		return Value{}, err
	}
	last := path[len(path)-1]
	switch parent.Kind {
	case KindObject:
		for i := range parent.Members {
			if parent.Members[i].Key == last {
				v := parent.Members[i].Value
				parent.Members = slices.Delete(parent.Members, i, i+1)
				return v, nil
			}
		}
		return Value{}, fmt.Errorf("%w: no member %q", ErrPatchTarget, last)
	case KindArray:
		i, ok := arrayIndex(last, len(parent.Items)-1)
		if !ok {
			return Value{}, fmt.Errorf("%w: bad array index %q", ErrPatchTarget, last)
		}
		v := parent.Items[i]
		parent.Items = slices.Delete(parent.Items, i, i+1)
		return v, nil
	default:
		return Value{}, fmt.Errorf("%w: %s has no child %q", ErrPatchTarget, parent.Kind, last)
	}
}
