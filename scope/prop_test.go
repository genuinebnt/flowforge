package scope

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// Property tests of spec 25.4 (ff-scope).

func TestScopeProp_IdempotentNormalize(t *testing.T) {
	r := testkit.Rng(t)
	valid := 0
	for range testkit.Cases() {
		raw, kind := genTarget(r)
		first, err := Normalize(raw, kind)
		if err != nil {
			continue
		}
		valid++
		second, err := Normalize(first.String(), kind)
		if err != nil {
			t.Fatalf("Normalize(%q, %s) = %q, which no longer normalizes: %v", raw, kind, first, err)
		}
		if second.String() != first.String() || second.Kind != first.Kind {
			t.Fatalf("not idempotent: %q -> %q (%s) -> %q (%s)", raw, first, first.Kind, second, second.Kind)
		}
	}
	if valid < testkit.Cases()/2 {
		t.Fatalf("only %d of %d generated targets were valid; generator is too noisy", valid, testkit.Cases())
	}
}

// TestScopeProp_DenyMonotone: adding a deny rule never turns a deny into
// an allow, and removing an allow rule never turns a deny into an allow
// (for Check and for the passive Status).
func TestScopeProp_DenyMonotone(t *testing.T) {
	r := testkit.Rng(t)
	ctx := context.Background()
	for i := range testkit.Cases() {
		doc := genDocument(r, 24)
		base, err := Compile(doc, WithResolver(hashResolver{}))
		if err != nil {
			t.Fatalf("generated document does not compile: %v", err)
		}
		req := genRequest(r)
		before := base.Check(ctx, req)
		statusBefore := base.Status(req.Target, req.Kind)

		withDeny := doc
		extra := genRule(r, fmt.Sprintf("extra%d", i))
		extra.Effect = EffectDeny
		withDeny.Rules = slices.Insert(slices.Clone(doc.Rules), core.IntN(r, len(doc.Rules)+1), extra)
		assertNoNewAllow(t, "adding deny "+extra.Value, withDeny, req, before, statusBefore)

		var allows []int
		for j, rule := range doc.Rules {
			if rule.Effect == EffectAllow {
				allows = append(allows, j)
			}
		}
		if len(allows) > 0 {
			j := pick(r, allows)
			withoutAllow := doc
			withoutAllow.Rules = slices.Delete(slices.Clone(doc.Rules), j, j+1)
			assertNoNewAllow(t, "removing allow "+doc.Rules[j].Value, withoutAllow, req, before, statusBefore)
		}
	}
}

func assertNoNewAllow(t *testing.T, change string, doc Document, req Request, before Decision, statusBefore Status) {
	t.Helper()
	p, err := Compile(doc, WithResolver(hashResolver{}))
	if err != nil {
		t.Fatalf("%s: compile: %v", change, err)
	}
	after := p.Check(context.Background(), req)
	if !before.Allowed() && after.Allowed() {
		t.Fatalf("%s turned %+v into %+v for %+v", change, before, after, req)
	}
	statusAfter := p.Status(req.Target, req.Kind)
	if statusBefore != StatusInScope && statusAfter == StatusInScope {
		t.Fatalf("%s turned status %s into %s for %q", change, statusBefore, statusAfter, req.Target)
	}
}

// TestScopeProp_NeverPanics feeds arbitrary targets, kinds, classes and
// rule values through every entry point; any panic fails the test, and
// every decision satisfies the structural invariants.
func TestScopeProp_NeverPanics(t *testing.T) {
	r := testkit.Rng(t)
	ctx := context.Background()
	for range testkit.Cases() {
		doc := genDocument(r, 12)
		for j := range doc.Rules {
			if core.IntN(r, 4) == 0 {
				doc.Rules[j].Value = genGarbage(r)
			}
			if core.IntN(r, 20) == 0 {
				doc.Rules[j].Kind = RuleKind(core.IntN(r, 8) - 1)
			}
		}
		p, err := Compile(doc, WithResolver(hashResolver{}))
		if err != nil {
			continue
		}
		for range 4 {
			req := genRequest(r)
			if core.IntN(r, 2) == 0 {
				req.Target = genGarbage(r)
			}
			d := p.Check(ctx, req)
			checkDecisionInvariants(t, d)
			_ = p.Status(req.Target, req.Kind)
			_ = p.Allows(req.Target, req.Kind)
			_ = p.CheckChain(ctx, req, []Hop{{Target: genGarbage(r)}, {Target: req.Target}})
		}
	}
}

// TestScopeProp_IndexedMatchesNaive is the differential test: the indexed
// matcher and the linear reference agree on every decision and status for
// generated rule sets and targets.
func TestScopeProp_IndexedMatchesNaive(t *testing.T) {
	r := testkit.Rng(t)
	ctx := context.Background()
	for range testkit.Cases() {
		doc := genDocument(r, 60)
		indexed, err := Compile(doc, WithResolver(hashResolver{}))
		if err != nil {
			t.Fatal(err)
		}
		linear, err := Compile(doc, WithResolver(hashResolver{}), withLinearMatcher())
		if err != nil {
			t.Fatal(err)
		}
		for range 8 {
			req := genRequest(r)
			got, want := indexed.Check(ctx, req), linear.Check(ctx, req)
			if !got.Equal(want) {
				t.Fatalf("decision mismatch for %+v\nindexed %+v\nlinear  %+v\nrules %+v", req, got, want, doc.Rules)
			}
			if gs, ws := indexed.Status(req.Target, req.Kind), linear.Status(req.Target, req.Kind); gs != ws {
				t.Fatalf("status mismatch for %q: indexed %s, linear %s\nrules %+v", req.Target, gs, ws, doc.Rules)
			}
		}
	}
}
