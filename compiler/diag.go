package compiler

import "github.com/genuinebnt/flowforge/dsl"

// Diagnostic codes the compiler adds to the front end's (spec 7.7).
const (
	// CodeTooLarge is E0602: the program exceeds a module limit (4 MiB,
	// 65,535 locals, constants or functions, 255 arguments or captures).
	CodeTooLarge = dsl.CodeTooLarge
	// CodeInternal is E0699: a defect of the compiler itself, for example
	// output the bytecode verifier rejects. It is never the program's
	// fault; tests treat it as a bug.
	CodeInternal = "E0699"
)

func diagnostic(code, file string, sp dsl.Span, label, msg string) dsl.Diagnostic {
	return dsl.Diagnostic{
		Code: code, Severity: dsl.SevError, Message: msg,
		Primary:   dsl.SourceSpan{File: file, Start: sp.Start, End: sp.End, Label: label},
		Secondary: []dsl.SourceSpan{},
	}
}

func internalError(file string, sp dsl.Span, msg string) dsl.Diagnostic {
	d := diagnostic(CodeInternal, file, sp, "internal compiler error", "internal compiler error: "+msg)
	d.Help = "This is a bug in the Flowforge compiler, not in the program; please report it with the source."
	return d
}
