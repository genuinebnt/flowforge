package jsparse

import (
	"fmt"
	"sort"
	"testing"
)

func fuzzSeeds(f *testing.F) {
	for _, s := range []string{
		"a / b / c", "x = /re/g; `t${1}x${/r/}`", "async (a, {b = 1}) => await a ?? b",
		"class A extends B { #x = 1; static { super.y } m() { return #x in this } }",
		"<!-- html\n--> close\nlet [a, ...b] = c", "import x, * as y from 'm' with { type: 'json' }; export default 1",
		"for await (const x of y) label: { break label }", "0x1_F + 1_000n + 017 + 08.5 + .5e-3",
		"'\\u{1F600}\\x41\\101' + `\\unicode`", "#!/usr/bin/env node\nf()", "a\n++b\nreturn\nx",
		"/[/]/.test(s) /* c */ // d\n", "({a = 1} = {}); [a, , b] = [,]", "\xff\xfe\x00   ",
	} {
		f.Add([]byte(s), uint8(0))
		f.Add([]byte(s), uint8(1))
	}
}

// lexTiling lexes src with goals drawn from goals (cycled) and checks
// that tokens plus trivia tile the input (JS-INV-01 for the lexer alone)
// and that the lexer terminates.
func lexTiling(src []byte, module bool, goals []byte) error {
	l := NewLexer(src, module)
	l.RecordTrivia(true)
	var toks []Token
	for i := 0; ; i++ {
		g := GoalDiv
		if len(goals) > 0 {
			g = Goal(goals[i%len(goals)] % 4)
		}
		t := l.Next(g)
		if t.Kind == TEOF {
			break
		}
		if t.End <= t.Start {
			return fmt.Errorf("empty token %v at %d", t.Kind, t.Start)
		}
		toks = append(toks, t)
		if i > len(src)+2 {
			return fmt.Errorf("lexer does not terminate")
		}
	}
	all := append(toks, l.Trivia()...)
	sort.Slice(all, func(i, j int) bool { return all[i].Start < all[j].Start })
	pos := int32(0)
	for _, t := range all {
		if t.Start != pos {
			return fmt.Errorf("gap or overlap at %d (%v starts at %d)", pos, t.Kind, t.Start)
		}
		pos = t.End
	}
	if int(pos) != len(src) {
		return fmt.Errorf("tokens end at %d of %d", pos, len(src))
	}
	return nil
}

// TestJSInv10_LexerTiling: under any goal sequence the lexer alone tiles
// the input with tokens and trivia and terminates.
func TestJSInv10_LexerTiling(t *testing.T) {
	r := newTestRng(t)
	for i, c := range corpus(t, 100) {
		goals := make([]byte, 1+r.Uint64()%7)
		for j := range goals {
			goals[j] = byte(r.Uint64())
		}
		if err := lexTiling(c.src, c.st == SourceModule, goals); err != nil {
			t.Fatalf("case %d goals %v: %v", i, goals, err)
		}
	}
}

func FuzzJSLexer(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte, mode uint8) {
		goals := []byte{mode, mode >> 2, mode >> 4}
		if err := lexTiling(data, mode&1 != 0, goals); err != nil {
			t.Fatal(err)
		}
	})
}

func FuzzJSParse(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte, mode uint8) {
		if err := checkTolerant(data, SourceType(mode&1)); err != nil {
			t.Fatal(err)
		}
	})
}

func FuzzJSPrintReparse(f *testing.F) {
	fuzzSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte, mode uint8) {
		st := SourceType(mode & 1)
		prog, err := Parse(data, Options{SourceType: st})
		if err != nil {
			return
		}
		want := string(prog.JSON(JSONOptions{}))
		for _, min := range []bool{false, true} {
			out := Print(prog, PrintOptions{Minify: min})
			again, err := Parse(out, Options{SourceType: st})
			if err != nil {
				t.Fatalf("minify=%v: reparse: %v\nprinted: %q", min, err, out)
			}
			if got := string(again.JSON(JSONOptions{})); got != want {
				t.Fatalf("minify=%v: AST changed\nprinted: %q\nwant %s\ngot  %s", min, out, want, got)
			}
		}
	})
}
