// Temporary generator for conformance/diff (deleted after use).
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/diff"
)

func dump(path string, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		panic(err)
	}
}

func lcsDist(a, b []uint32) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
			} else {
				cur[j] = max(prev[j], cur[j-1])
			}
		}
		prev, cur = cur, prev
	}
	return len(a) + len(b) - 2*prev[len(b)]
}

type scriptCase struct {
	Name     string            `json:"name"`
	Old      []string          `json:"old"`
	New      []string          `json:"new"`
	Distance int               `json:"distance"`
	Expect   map[string]string `json:"expect"`
}

type gitCase struct {
	Name, Old, New string
	Git            map[string]string
}

type unifiedCase struct {
	Name      string `json:"name"`
	Old       string `json:"old"`
	New       string `json:"new"`
	Algorithm string `json:"algorithm"`
	Context   int    `json:"context"`
	Expect    string `json:"expect"`
}

func chars(s string) []string {
	var out []string
	for _, r := range s {
		out = append(out, string(r))
	}
	if out == nil {
		out = []string{}
	}
	return out
}

func main() {
	dir, scratch := os.Args[1], os.Args[2]
	// scripts.json
	var sc []scriptCase
	add := func(name string, a, b []string) {
		c := scriptCase{Name: name, Old: a, New: b, Expect: map[string]string{}}
		for _, alg := range diff.Algorithms() {
			r, in, err := diff.DiffStrings(a, b, diff.Options{Algorithm: alg})
			if err != nil {
				panic(err)
			}
			c.Expect[alg.String()] = r.Script.String()
			ia, ib := in.InternAll(a), in.InternAll(b)
			c.Distance = lcsDist(ia, ib)
			g, _, _ := diff.DiffStrings(a, b, diff.Options{Algorithm: alg, GitCompact: true})
			c.Expect[alg.String()+"+git-compact"] = g.Script.String()
		}
		sc = append(sc, c)
	}
	add("paper_abcabba_cbabac", chars("ABCABBA"), chars("CBABAC"))
	add("both_empty", []string{}, []string{})
	add("old_empty", []string{}, chars("abc"))
	add("new_empty", chars("abc"), []string{})
	add("identical", chars("abcdef"), chars("abcdef"))
	add("disjoint", chars("abc"), chars("xyz"))
	add("single_insert_middle", chars("abd"), chars("abcd"))
	add("repeated_tokens", chars("aaaa"), chars("aa"))
	add("repeated_runs", chars("abababab"), chars("baba"))
	add("prefix_suffix", chars("xxabcyy"), chars("xxacyy"))
	add("dmp_example", strings.Fields("The quick brown fox jumps over the lazy dog"), strings.Fields("The quick red fox leaps over the lazy cat"))
	add("braces", strings.Split("{|a|}|{|b|}|{|c|}", "|"), strings.Split("{|a|}|{|x|}|{|c|}|{|d|}", "|"))
	add("moved_block", chars("abcdefgh"), chars("efgabcdh"))
	r := core.NewSplitMix64(0x44494646) // "DIFF"
	alph := []string{"a", "b", "c", "d", "e", "f", "{", "}"}
	for k := range 30 {
		n := 1 + core.IntN(r, 8)
		var a []string
		for range core.IntN(r, 25) {
			a = append(a, alph[core.IntN(r, n)])
		}
		b := append([]string(nil), a...)
		for range 1 + core.IntN(r, 5) {
			switch core.IntN(r, 3) {
			case 0:
				if len(b) > 0 {
					b[core.IntN(r, len(b))] = alph[core.IntN(r, n)]
				}
			case 1:
				i := core.IntN(r, len(b)+1)
				b = append(b[:i], append([]string{alph[core.IntN(r, n)]}, b[i:]...)...)
			default:
				if len(b) > 0 {
					i := core.IntN(r, len(b))
					b = append(b[:i], b[i+1:]...)
				}
			}
		}
		if a == nil {
			a = []string{}
		}
		if b == nil {
			b = []string{}
		}
		add(fmt.Sprintf("random_%02d", k), a, b)
	}
	dump(dir+"/scripts.json", map[string]any{
		"id":          "diff_scripts_001",
		"description": "Edit scripts per algorithm over string tokens (each string is one token). 'distance' is the brute-force n+m-2*LCS (myers-linear, myers and reference must equal it). Scripts are EditScript.String(): space-separated runs '=k' equal, '-k' delete, '+k' insert, in canonical form (one delete then one insert per change region), so ranges follow by tiling. '<alg>+git-compact' is the same after CompactGit. Pins the reference implementation's alignment choices for the port.",
		"scripts":     sc,
	})

	// unified_git.json
	var uc []unifiedCase
	load := func(p string) []gitCase {
		b, err := os.ReadFile(p)
		if err != nil {
			panic(err)
		}
		var cs []gitCase
		if err := json.Unmarshal(b, &cs); err != nil {
			panic(err)
		}
		return cs
	}
	mismatch := 0
	consider := func(c gitCase, alg diff.Algorithm, ctx int, required bool) {
		gitName := map[diff.Algorithm]string{diff.MyersLinearSpace: "myers", diff.Patience: "patience", diff.Histogram: "histogram"}[alg]
		key := fmt.Sprintf("%s/%d", gitName, ctx)
		want, ok := c.Git[key]
		if !ok {
			return
		}
		ol, nl := diff.SplitLines([]byte(c.Old)), diff.SplitLines([]byte(c.New))
		res, _, _ := diff.DiffStrings(ol, nl, diff.Options{Algorithm: alg, GitCompact: true})
		u, _ := diff.Unified(ol, nl, res.Script, diff.UnifiedOptions{Context: ctx})
		if string(u) != want {
			if required {
				fmt.Fprintf(os.Stderr, "MISMATCH %s %s\n", c.Name, key)
				mismatch++
			}
			return
		}
		uc = append(uc, unifiedCase{Name: c.Name, Old: c.Old, New: c.New, Algorithm: alg.String(), Context: ctx, Expect: want})
	}
	for _, c := range load(scratch + "/git/git.json") {
		for _, alg := range []diff.Algorithm{diff.MyersLinearSpace, diff.Patience, diff.Histogram} {
			for _, ctx := range []int{0, 1, 3} {
				consider(c, alg, ctx, !(alg == diff.MyersLinearSpace && c.Name == "patience_c_functions"))
			}
		}
	}
	rand := load(scratch + "/gitr/rand.json")
	nh, np, nm := 0, 0, 0
	for _, c := range rand {
		before := len(uc)
		if nh < 60 {
			consider(c, diff.Histogram, 2, true)
			nh += len(uc) - before
		}
		before = len(uc)
		if np < 40 {
			consider(c, diff.Patience, 2, false)
			np += len(uc) - before
		}
		before = len(uc)
		if nm < 25 {
			consider(c, diff.MyersLinearSpace, 2, false)
			nm += len(uc) - before
		}
	}
	for i := range uc {
		if strings.HasPrefix(uc[i].Name, "rand") {
			uc[i].Name = "random_" + strings.TrimPrefix(uc[i].Name, "rand")
		}
	}
	if mismatch > 0 {
		fmt.Fprintf(os.Stderr, "%d mismatches\n", mismatch)
	}
	dump(dir+"/unified_git.json", map[string]any{
		"id":          "diff_unified_git_001",
		"description": "Unified hunks produced by git 2.43 'git diff --no-index --no-indent-heuristic --diff-algorithm=<algorithm> -U<context>' (file headers and the section heading after the second '@@' removed). The implementation must reproduce them byte for byte with SplitLines, Diff(Options{Algorithm, GitCompact: true}) and Unified(Context). Histogram cases (constructed and random) always match git; patience cases are those without a Myers fallback range; myers-linear cases (git's --diff-algorithm=myers) are those whose shortest alignment git and MyersLinearSpace agree on (see diff/OPEN_QUESTIONS.md).",
		"unified":     uc,
	})

	// json_diff.json
	type jd struct {
		Name  string          `json:"name"`
		A     json.RawMessage `json:"a"`
		B     json.RawMessage `json:"b"`
		Patch json.RawMessage `json:"patch"`
	}
	var jds []jd
	jadd := func(name, a, b string) {
		va, err := diff.ParseJSON([]byte(a))
		if err != nil {
			panic(err)
		}
		vb, err := diff.ParseJSON([]byte(b))
		if err != nil {
			panic(err)
		}
		p := diff.JSONDiff(va, vb, diff.JSONOptions{})
		got, err := diff.ApplyPatch(va, p)
		if err != nil || !diff.Equal(got, vb) {
			panic("json round trip " + name)
		}
		jds = append(jds, jd{name, json.RawMessage(a), json.RawMessage(b), json.RawMessage(diff.MarshalPatch(p))})
	}
	jadd("equal", `{"a":1,"b":[1,2]}`, `{"b":[1,2],"a":1}`)
	jadd("numeric_equal", `{"n":1.0}`, `{"n":1}`)
	jadd("scalar_replace", `{"a":1}`, `{"a":2}`)
	jadd("kind_change", `{"a":[1]}`, `{"a":{"0":1}}`)
	jadd("root_scalar", `1`, `"one"`)
	jadd("object_add_remove", `{"keep":true,"old":1}`, `{"keep":true,"new":2}`)
	jadd("escaped_keys", `{"a/b":1,"m~n":2}`, `{"a/b":3,"m~n":2,"":0}`)
	jadd("array_insert_middle", `[1,2,4]`, `[1,2,3,4]`)
	jadd("array_delete_two", `[1,2,3,4,5]`, `[1,4,5]`)
	jadd("array_replace_pairs", `[1,2,3]`, `[1,"two",3]`)
	jadd("array_recurse_objects", `[{"id":1,"v":"a"},{"id":2,"v":"b"}]`, `[{"id":1,"v":"a"},{"id":2,"v":"B"}]`)
	jadd("array_surplus_inserts", `["x"]`, `["a","b","c"]`)
	jadd("array_to_empty", `[1,2,3]`, `[]`)
	jadd("nested", `{"servers":[{"host":"a","ports":[80,443]},{"host":"b","ports":[22]}],"v":1}`, `{"servers":[{"host":"a","ports":[443]},{"host":"c","ports":[22]},{"host":"d","ports":[]}],"v":2}`)
	jadd("unicode", `{"s":"café"}`, `{"s":"café"}`)
	dump(dir+"/json_diff.json", map[string]any{
		"id":          "diff_json_diff_001",
		"description": "JSONDiff(a, b) expected RFC 6902 patches (MarshalPatch encoding: members op, path, value). Objects: keys in bytewise order, a-only removed, b-only added, common recursed. Arrays: Myers over hash-consed element ids; in a change region the i-th deleted and inserted elements pair up (recursed when both objects or both arrays, else replaced), surplus removed or added at the running index. Applying the patch to a must give a value structurally equal to b.",
		"json_diff":   jds,
	})

	// bounded.json
	type bc struct {
		Name     string `json:"name"`
		Old      string `json:"old"`
		New      string `json:"new"`
		Bounds   [3]int `json:"bounds"`
		Level    string `json:"level"`
		Fallback string `json:"fallback"`
		Capped   bool   `json:"capped"`
		Script   string `json:"script"`
	}
	var bcs []bc
	badd := func(name, a, b string, bo [3]int) {
		res, err := diff.BoundedDiff([]byte(a), []byte(b), diff.Bounds{MaxEditDistance: bo[0], MaxInputBytes: bo[1], MaxLineInputBytes: bo[2]})
		if err != nil {
			panic(err)
		}
		s := ""
		if res.Level != diff.LevelHash {
			s = res.Script.String()
		}
		bcs = append(bcs, bc{name, a, b, bo, res.Level.String(), res.Fallback.String(), res.Capped, s})
	}
	t1 := "fetch('/api/v1/users');\nconst x = 1;\nconsole.log(x);\n"
	t2 := "fetch('/api/v2/users');\nconst x = 2;\nconsole.log(x);\n"
	badd("tokens_default", t1, t2, [3]int{})
	badd("identical", t1, t1, [3]int{})
	badd("edit_distance_cap_to_lines", t1, t2, [3]int{2, 0, 0})
	badd("input_size_to_lines", t1, t2, [3]int{0, 16, 0})
	badd("input_size_to_hash", t1, t2, [3]int{0, 16, 16})
	badd("lines_capped", "a\nb\nc\nd\n", "w\nx\ny\nz\n", [3]int{1, 0, 0})
	dump(dir+"/bounded.json", map[string]any{
		"id":          "diff_bounded_001",
		"description": "BoundedDiff levels (V2_DESIGN §5). bounds = [MaxEditDistance, MaxInputBytes, MaxLineInputBytes], 0 meaning the default (10000, 4 MiB, 64 MiB). level is tokens (Myers over SplitWords), lines (histogram over SplitLines) or hash; fallback is none, edit_distance or input_size; script is EditScript.String() at that level (empty for hash).",
		"bounded":     bcs,
	})
}
