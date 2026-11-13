package jsparse

import (
	"os"
	"strconv"
	"testing"
)

func TestDbg2(t *testing.T) {
	src, _ := os.ReadFile(os.Getenv("DBGF"))
	at, _ := strconv.Atoi(os.Getenv("AT"))
	prog, _ := Parse(src, Options{Tokens: true, Tolerant: true})
	for _, tk := range prog.Tokens {
		if int(tk.Start) > at-400 && int(tk.Start) < at+30 && !tk.Kind.IsTrivia() {
			t.Logf("%d %v %q", tk.Start, tk.Kind, src[tk.Start:tk.End])
		}
	}
}
