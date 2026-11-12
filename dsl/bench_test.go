package dsl

import (
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

// BenchmarkDSL01_FrontEnd measures lex + parse + check throughput on
// generated 10 KB and 1 MB programs (b_dsl_01; MB/s via SetBytes).
func BenchmarkDSL01_FrontEnd(b *testing.B) {
	for _, size := range []struct {
		name  string
		bytes int
	}{{"10KB", 10 << 10}, {"1MB", 1 << 20}} {
		src := []byte(genProgram(core.NewSplitMix64(1), size.bytes))
		f, diags := Parse("bench.ff", src)
		if _, cd := Check(f, Options{}); len(diags) > 0 || HasErrors(cd) {
			b.Fatalf("generated program does not check: %v %v", diags, cd)
		}
		b.Run(size.name, func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				f, _ := Parse("bench.ff", src)
				Check(f, Options{})
			}
		})
	}
}
