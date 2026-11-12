package compiler

import (
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/dsl"
)

// Span tables: before the instruction that starts an expression, and again
// before the instruction that performs an operation that can fail (an
// arithmetic op, INDEX, CALL, EFFECT, TRY, FAIL, ...), the compiler
// records the expression's source range (bytecode.FuncBuilder.Span). The
// VM reports a failure with the closest span at or before the failing
// instruction (vm.Failed.Span), and SourceSpan turns that back into a
// line/column range of the compiled source.

// SourceSpan converts a source byte range of the compiled source (as in
// vm.Span.SourceOffset and Len) into a labelled span with 1-based line and
// column (columns count code points, as in diagnostics, spec 7.7).
func (r *Result) SourceSpan(offset, length uint32, label string) dsl.SourceSpan {
	start := r.position(int(offset))
	end := r.position(int(offset) + int(length))
	return dsl.SourceSpan{File: r.file, Start: start, End: end, Label: label}
}

// position returns the line/column of a byte offset, clamped to the
// source.
func (r *Result) position(off int) dsl.Pos {
	src := r.Source
	off = min(max(off, 0), len(src))
	line, col := 1, 1
	for i := 0; i < off; {
		c, size := utf8.DecodeRuneInString(src[i:])
		if i+size > off {
			break
		}
		if c == '\n' {
			line, col = line+1, 1
		} else {
			col++
		}
		i += size
	}
	return dsl.Pos{Line: line, Col: col, Offset: off}
}
