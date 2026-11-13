package diff

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

// dmpOp is a diff-match-patch op: [kind, text] with kind "=", "-" or "+".
type dmpOp [2]string

// diffFile is the union of every conformance/diff/*.json shape; each file
// fills the sections it has.
type diffFile struct {
	ID      string `json:"id"`
	Scripts []struct {
		Name     string            `json:"name"`
		Old      []string          `json:"old"`
		New      []string          `json:"new"`
		Distance int               `json:"distance"`
		Expect   map[string]string `json:"expect"`
	} `json:"scripts"`
	Unified []struct {
		Name      string `json:"name"`
		Old       string `json:"old"`
		New       string `json:"new"`
		Algorithm string `json:"algorithm"`
		Context   int    `json:"context"`
		Expect    string `json:"expect"`
	} `json:"unified"`
	Parse []struct {
		Name  string `json:"name"`
		Old   string `json:"old"`
		Patch string `json:"patch"`
		New   string `json:"new"`
		Error bool   `json:"error"`
	} `json:"parse"`
	Cleanup []struct {
		Name   string  `json:"name"`
		Pass   string  `json:"pass"`
		Input  []dmpOp `json:"input"`
		Expect []dmpOp `json:"expect"`
	} `json:"cleanup"`
	Apply []struct {
		Name      string          `json:"name"`
		Doc       json.RawMessage `json:"doc"`
		Patch     json.RawMessage `json:"patch"`
		PatchText string          `json:"patch_text"`
		Expect    json.RawMessage `json:"expect"`
		Error     bool            `json:"error"`
	} `json:"apply"`
	JSONDiff []struct {
		Name  string          `json:"name"`
		A     json.RawMessage `json:"a"`
		B     json.RawMessage `json:"b"`
		Patch json.RawMessage `json:"patch"`
	} `json:"json_diff"`
	Trees []struct {
		Name     string `json:"name"`
		A        string `json:"a"`
		B        string `json:"b"`
		Distance int    `json:"distance"`
		Costs    *struct {
			Insert  int `json:"insert"`
			Delete  int `json:"delete"`
			Relabel int `json:"relabel"`
		} `json:"costs"`
	} `json:"trees"`
	Bounded []struct {
		Name     string `json:"name"`
		Old      string `json:"old"`
		New      string `json:"new"`
		Bounds   [3]int `json:"bounds"`
		Level    string `json:"level"`
		Fallback string `json:"fallback"`
		Capped   bool   `json:"capped"`
		Script   string `json:"script"`
	} `json:"bounded"`
}

func mustJSON(t *testing.T, raw []byte) Value {
	t.Helper()
	v, err := ParseJSON(raw)
	if err != nil {
		t.Fatalf("ParseJSON(%s): %v", raw, err)
	}
	return v
}

// TestDiffConformance runs every file of conformance/diff.
func TestDiffConformance(t *testing.T) {
	for _, cf := range testkit.LoadCases[diffFile](t, "diff") {
		c := cf.Data
		t.Run(c.ID, func(t *testing.T) {
			if c.ID == "" {
				t.Fatalf("%s has no id", cf.Name)
			}
			runScripts(t, c)
			runUnified(t, c)
			runParse(t, c)
			runCleanup(t, c)
			runJSON(t, c)
			runTrees(t, c)
			runBounded(t, c)
		})
	}
}

func runScripts(t *testing.T, c diffFile) {
	for _, sc := range c.Scripts {
		in := NewInterner()
		a, b := in.InternAll(sc.Old), in.InternAll(sc.New)
		if len(sc.Expect) != 2*len(Algorithms()) {
			t.Errorf("%s: expectations for %d variants, want %d", sc.Name, len(sc.Expect), 2*len(Algorithms()))
		}
		for _, alg := range Algorithms() {
			for _, compact := range []bool{false, true} {
				key := alg.String()
				if compact {
					key += "+git-compact"
				}
				want, ok := sc.Expect[key]
				if !ok {
					continue
				}
				res := mustDiff(t, a, b, Options{Algorithm: alg, GitCompact: compact})
				if got := res.Script.String(); got != want {
					t.Errorf("%s %s: %q, want %q", sc.Name, key, got, want)
				}
				if !equalU32(mustApply(t, a, res.Script), b) {
					t.Errorf("%s %s: not Apply-correct", sc.Name, key)
				}
				minimal := alg == MyersLinearSpace || alg == Myers || alg == Reference
				if d := res.Script.Distance(); (minimal && d != sc.Distance) || d < sc.Distance {
					t.Errorf("%s %s: distance %d, minimal %d", sc.Name, key, d, sc.Distance)
				}
			}
		}
	}
}

func runUnified(t *testing.T, c diffFile) {
	for _, u := range c.Unified {
		alg, err := ParseAlgorithm(u.Algorithm)
		if err != nil {
			t.Fatal(err)
		}
		ol, nl := SplitLines([]byte(u.Old)), SplitLines([]byte(u.New))
		res, _, err := DiffStrings(ol, nl, Options{Algorithm: alg, GitCompact: true})
		if err != nil {
			t.Fatal(err)
		}
		got, err := Unified(ol, nl, res.Script, UnifiedOptions{Context: u.Context})
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != u.Expect {
			t.Errorf("%s %s -U%d:\n--- got\n%s--- git\n%s", u.Name, u.Algorithm, u.Context, got, u.Expect)
			continue
		}
		if u.Expect == "" {
			continue
		}
		p, err := ParseUnified([]byte(u.Expect))
		if err != nil {
			t.Errorf("%s: parse git output: %v", u.Name, err)
			continue
		}
		if back, err := ApplyUnified(ol, p); err != nil || strings.Join(back, "") != u.New {
			t.Errorf("%s: applying git's hunks gave %q (%v)", u.Name, strings.Join(back, ""), err)
		}
	}
}

func runParse(t *testing.T, c diffFile) {
	for _, pc := range c.Parse {
		p, err := ParseUnified([]byte(pc.Patch))
		var out []string
		if err == nil {
			out, err = ApplyUnified(SplitLines([]byte(pc.Old)), p)
		}
		switch {
		case pc.Error && err == nil:
			t.Errorf("%s: accepted, got %q", pc.Name, strings.Join(out, ""))
		case !pc.Error && err != nil:
			t.Errorf("%s: %v", pc.Name, err)
		case !pc.Error && strings.Join(out, "") != pc.New:
			t.Errorf("%s: got %q, want %q", pc.Name, strings.Join(out, ""), pc.New)
		}
	}
}

// toCharSegs converts dmp ops into character-token segments.
func toCharSegs(in *Interner, ops []dmpOp) []seg {
	var out []seg
	for _, op := range ops {
		var toks []uint32
		for _, r := range op[1] {
			toks = append(toks, in.Intern(string(r)))
		}
		k := map[string]OpKind{"=": OpEqual, "-": OpDelete, "+": OpInsert}[op[0]]
		out = append(out, seg{k, toks})
	}
	return out
}

func fromCharSegs(in *Interner, segs []seg) []dmpOp {
	out := []dmpOp{}
	for _, s := range segs {
		var b strings.Builder
		for _, id := range s.toks {
			v, _ := in.Value(id)
			b.WriteString(v)
		}
		out = append(out, dmpOp{map[OpKind]string{OpEqual: "=", OpDelete: "-", OpInsert: "+"}[s.kind], b.String()})
	}
	return out
}

func runCleanup(t *testing.T, c diffFile) {
	for _, cc := range c.Cleanup {
		in := NewInterner()
		segs := toCharSegs(in, cc.Input)
		var got []seg
		switch cc.Pass {
		case "merge":
			got = cleanupMerge(segs)
		case "lossless":
			got = cleanupLossless(segs, TextScorer(in))
		case "semantic":
			got = cleanupSemanticSegs(segs, TextScorer(in))
		default:
			t.Fatalf("%s: unknown pass %q", cc.Name, cc.Pass)
		}
		if g := fromCharSegs(in, got); !slices.Equal(g, append([]dmpOp{}, cc.Expect...)) {
			t.Errorf("%s: %q, want %q", cc.Name, g, cc.Expect)
		}
		if cc.Pass != "semantic" {
			continue
		}
		// Through the public API: the input as a canonical script.
		var oldT, newT []uint32
		for _, s := range segs {
			if s.kind != OpInsert {
				oldT = append(oldT, s.toks...)
			}
			if s.kind != OpDelete {
				newT = append(newT, s.toks...)
			}
		}
		res := mustDiff(t, oldT, newT, Options{})
		cs, err := CleanupSemantic(oldT, res.Script, CleanupOptions{Score: TextScorer(in)})
		if err != nil || !equalU32(mustApply(t, oldT, cs), newT) {
			t.Errorf("%s: CleanupSemantic not Apply-correct (%v)", cc.Name, err)
		}
	}
}

func runJSON(t *testing.T, c diffFile) {
	for _, ac := range c.Apply {
		doc := mustJSON(t, ac.Doc)
		ptext := []byte(ac.PatchText)
		if len(ptext) == 0 {
			ptext = ac.Patch
		}
		ops, err := ParsePatch(ptext)
		var got Value
		if err == nil {
			got, err = ApplyPatch(doc, ops)
		}
		switch {
		case ac.Error && err == nil:
			t.Errorf("%s: accepted, got %s", ac.Name, EncodeJSON(got))
		case !ac.Error && err != nil:
			t.Errorf("%s: %v", ac.Name, err)
		case !ac.Error && !Equal(got, mustJSON(t, ac.Expect)):
			t.Errorf("%s: got %s, want %s", ac.Name, EncodeJSON(got), ac.Expect)
		}
	}
	for _, jc := range c.JSONDiff {
		a, b := mustJSON(t, jc.A), mustJSON(t, jc.B)
		p := JSONDiff(a, b, JSONOptions{})
		want := EncodeJSON(mustJSON(t, jc.Patch))
		if got := MarshalPatch(p); string(got) != string(want) {
			t.Errorf("%s: patch %s, want %s", jc.Name, got, want)
		}
		if got, err := ApplyPatch(a, p); err != nil || !Equal(got, b) {
			t.Errorf("%s: patch does not turn a into b (%v)", jc.Name, err)
		}
	}
}

func runTrees(t *testing.T, c diffFile) {
	for _, tc := range c.Trees {
		in := NewInterner()
		a, err := ParseBracket(tc.A, in)
		if err != nil {
			t.Fatal(err)
		}
		b, err := ParseBracket(tc.B, in)
		if err != nil {
			t.Fatal(err)
		}
		if FormatBracket(a, in) != tc.A || FormatBracket(b, in) != tc.B {
			t.Errorf("%s: bracket notation does not round trip", tc.Name)
		}
		costs := TreeCosts{Insert: 1, Delete: 1, Relabel: 1}
		if tc.Costs != nil {
			costs = TreeCosts{Insert: tc.Costs.Insert, Delete: tc.Costs.Delete, Relabel: tc.Costs.Relabel}
		}
		res := mustTreeDiff(t, a, b, TreeOptions{Costs: costs})
		if res.Distance != tc.Distance {
			t.Errorf("%s: distance %d, want %d", tc.Name, res.Distance, tc.Distance)
		}
		checkMapping(t, a, b, res, costs)
	}
}

func runBounded(t *testing.T, c diffFile) {
	for _, bc := range c.Bounded {
		res, err := BoundedDiff([]byte(bc.Old), []byte(bc.New), Bounds{MaxEditDistance: bc.Bounds[0], MaxInputBytes: bc.Bounds[1], MaxLineInputBytes: bc.Bounds[2]})
		if err != nil {
			t.Fatal(err)
		}
		script := ""
		if res.Level != LevelHash {
			script = res.Script.String()
		}
		if res.Level.String() != bc.Level || res.Fallback.String() != bc.Fallback || res.Capped != bc.Capped || script != bc.Script {
			t.Errorf("%s: %s/%s capped=%v %q, want %s/%s capped=%v %q", bc.Name, res.Level, res.Fallback, res.Capped, script, bc.Level, bc.Fallback, bc.Capped, bc.Script)
		}
	}
}

// TestDiffInv08_GitCompatibility is DIFF-INV-08: with GitCompact, unified
// output reproduces git 2.43's hunks byte for byte for the cases recorded
// in conformance/diff/unified_git.json (histogram always; patience without
// a Myers fallback; Myers where git picks the same alignment).
func TestDiffInv08_GitCompatibility(t *testing.T) {
	var ran int
	for _, cf := range testkit.LoadCases[diffFile](t, "diff") {
		if cf.Name != "unified_git.json" {
			continue
		}
		ran++
		runUnified(t, cf.Data)
	}
	if ran == 0 {
		t.Fatal("conformance/diff/unified_git.json is missing")
	}
}
