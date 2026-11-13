package diff

import (
	"bytes"
	"fmt"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// randJSON draws a small JSON value; small alphabets make equal subtrees
// and array repeats common.
func randJSON(r core.Rng, depth int) Value {
	k := core.IntN(r, 8)
	if depth <= 0 {
		k = core.IntN(r, 4)
	}
	switch k {
	case 0:
		return Null()
	case 1:
		return BoolValue(core.IntN(r, 2) == 0)
	case 2:
		return NumberValue([]string{"0", "1", "1.0", "-2", "3e2", "300", "0.5"}[core.IntN(r, 7)])
	case 3:
		return StringValue([]string{"", "a", "b", "~", "/", "é"}[core.IntN(r, 6)])
	case 4, 5:
		v := Value{Kind: KindArray, Items: []Value{}}
		for range core.IntN(r, 6) {
			v.Items = append(v.Items, randJSON(r, depth-1))
		}
		return v
	default:
		v := Value{Kind: KindObject, Members: []Member{}}
		keys := []string{"a", "b", "c", "a/b", "m~n", ""}
		core.Shuffle(r, len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		for _, key := range keys[:core.IntN(r, len(keys)+1)] {
			v.Members = append(v.Members, Member{Key: key, Value: randJSON(r, depth-1)})
		}
		return v
	}
}

// mutateJSON derives a related value.
func mutateJSON(r core.Rng, v Value, depth int) Value {
	v = cloneValue(v)
	switch {
	case v.Kind == KindArray && len(v.Items) > 0 && core.IntN(r, 4) > 0:
		i := core.IntN(r, len(v.Items))
		switch core.IntN(r, 3) {
		case 0:
			v.Items[i] = mutateJSON(r, v.Items[i], depth-1)
		case 1:
			v.Items = slices.Delete(v.Items, i, i+1)
		default:
			v.Items = slices.Insert(v.Items, i, randJSON(r, depth-1))
		}
	case v.Kind == KindObject && len(v.Members) > 0 && core.IntN(r, 4) > 0:
		i := core.IntN(r, len(v.Members))
		if core.IntN(r, 3) == 0 {
			v.Members = slices.Delete(v.Members, i, i+1)
		} else {
			v.Members[i].Value = mutateJSON(r, v.Members[i].Value, depth-1)
		}
	default:
		return randJSON(r, depth)
	}
	return v
}

// DIFF-INV-09: ApplyPatch(a, JSONDiff(a, b)) is structurally Equal to b;
// the patch survives MarshalPatch/ParsePatch unchanged; equal documents
// produce an empty patch; and the encoding of a parsed document parses
// back to an Equal value.
func TestDiffInv09_JSONPatchRoundTrip(t *testing.T) {
	r := testkit.Rng(t)
	for range testkit.Cases() {
		a := randJSON(r, 4)
		b := mutateJSON(r, a, 4)
		if core.IntN(r, 5) == 0 {
			b = randJSON(r, 4)
		}
		before := EncodeJSON(a)
		patch := JSONDiff(a, b, JSONOptions{})
		got, err := ApplyPatch(a, patch)
		if err != nil {
			t.Fatalf("ApplyPatch: %v\na=%s\nb=%s\npatch=%s", err, EncodeJSON(a), EncodeJSON(b), MarshalPatch(patch))
		}
		if !Equal(got, b) {
			t.Fatalf("round trip\na=%s\nb=%s\ngot=%s\npatch=%s", EncodeJSON(a), EncodeJSON(b), EncodeJSON(got), MarshalPatch(patch))
		}
		if Equal(a, b) != (len(patch) == 0) {
			t.Fatalf("equal=%v but patch has %d ops", Equal(a, b), len(patch))
		}
		enc := MarshalPatch(patch)
		back, err := ParsePatch(enc)
		if err != nil {
			t.Fatalf("ParsePatch(%s): %v", enc, err)
		}
		if !bytes.Equal(MarshalPatch(back), enc) {
			t.Fatalf("patch encoding not stable: %s vs %s", MarshalPatch(back), enc)
		}
		pa, err := ParseJSON(EncodeJSON(a))
		if err != nil || !Equal(pa, a) || !bytes.Equal(EncodeJSON(pa), EncodeJSON(a)) {
			t.Fatalf("EncodeJSON/ParseJSON round trip failed for %s: %v", EncodeJSON(a), err)
		}
		// The input document is never modified.
		if !bytes.Equal(EncodeJSON(a), before) {
			t.Fatal("JSONDiff or ApplyPatch modified its input")
		}
	}
}

// DIFF-INV-10: Equal is RFC 6902 test equality: reflexive, symmetric,
// order-insensitive for objects, numeric for numbers, and agrees with
// hash-consed ids (equal ids exactly when Equal).
func TestDiffInv10_JSONEquality(t *testing.T) {
	r := testkit.Rng(t)
	h := newHashCons()
	var vals []Value
	for range testkit.Cases() / 2 {
		vals = append(vals, randJSON(r, 3))
	}
	for i, a := range vals {
		if !Equal(a, a) || !Equal(a, Canonical(a)) {
			t.Fatalf("Equal not reflexive / order-insensitive for %s", EncodeJSON(a))
		}
		for _, b := range vals[max(0, i-20):i] {
			e := Equal(a, b)
			if e != Equal(b, a) {
				t.Fatal("Equal not symmetric")
			}
			if e != (h.id(a) == h.id(b)) {
				t.Fatalf("hash-cons ids disagree with Equal (%v) for %s / %s", e, EncodeJSON(a), EncodeJSON(b))
			}
			if e != bytes.Equal(EncodeJSON(canonicalNumbers(Canonical(a))), EncodeJSON(canonicalNumbers(Canonical(b)))) {
				t.Fatalf("Equal (%v) disagrees with canonical encoding for %s / %s", e, EncodeJSON(a), EncodeJSON(b))
			}
		}
	}
	for _, p := range [][2]string{{"1", "1.0"}, {"100", "1e2"}, {"-0", "0"}, {"0.50", "5e-1"}, {"12e+3", "12000"}} {
		if !Equal(NumberValue(p[0]), NumberValue(p[1])) {
			t.Errorf("%s != %s", p[0], p[1])
		}
	}
	for _, p := range [][2]string{{"1", "2"}, {"1", "-1"}, {"1e400", "1e401"}, {"10", "1"}} {
		if Equal(NumberValue(p[0]), NumberValue(p[1])) {
			t.Errorf("%s == %s", p[0], p[1])
		}
	}
}

// canonicalNumbers rewrites every number to its canonical text (test
// helper for the encoding cross-check).
func canonicalNumbers(v Value) Value {
	switch v.Kind {
	case KindNumber:
		v.Number = fmt.Sprintf("%q", canonicalNumber(v.Number))
		v.Kind = KindString
		v.Text = v.Number
	case KindArray:
		for i := range v.Items {
			v.Items[i] = canonicalNumbers(v.Items[i])
		}
	case KindObject:
		for i := range v.Members {
			v.Members[i].Value = canonicalNumbers(v.Members[i].Value)
		}
	}
	return v
}
