package compiler

import (
	"github.com/genuinebnt/flowforge/bytecode"
	"github.com/genuinebnt/flowforge/dsl"
)

// Annotations reach the engine through the module's ANNOTS section
// (bytecode.Annotations): the workflow's #[timeout], #[effects(max)],
// #[on_unknown(retry)] and #[idempotent], plus one entry per EFFECT
// instruction that is lexically inside an annotated let statement (its
// #[timeout] and #[retry]) or that calls an #[idempotent] tool or sits in
// an #[idempotent] fn. "Lexically inside" includes the bodies of lambdas
// written in the statement, which are compiled as separate functions (the
// spec 7.1 recon workflow retries the httpx calls of its par_map closure).
// Effects in fns the statement calls are not covered: their sites are
// shared by every caller. The engine finds a suspension's policy with
// Annotations.Site(s.Site.Func, s.Site.CodeOffset).

// letAnnot is the annotation of the let statement being compiled.
type letAnnot struct {
	sp             dsl.Span
	timeout        int64
	retryMax       int64
	backoffInitial int64
	backoffMax     int64
}

// letAnnotation returns the annotation of s, or inherited when s has none
// (nested statements inside a lambda of an annotated statement keep the
// outer policy unless they declare their own).
func letAnnotation(s *dsl.LetStmt, inherited *letAnnot) *letAnnot {
	a := s.Attrs
	if a.Timeout == 0 && a.RetryMax == 0 {
		return inherited
	}
	return &letAnnot{sp: s.Sp, timeout: a.Timeout, retryMax: a.RetryMax,
		backoffInitial: a.BackoffInitial, backoffMax: a.BackoffMax}
}

// effect emits EFFECT kind argc, records its mask bits and, when a policy
// applies, its site annotation. tool is the called tool for tool_call.
func (x *fnCtx) effect(kind bytecode.EffectKind, argc int, tool *dsl.Tool, n dsl.Node) {
	if argc > 255 {
		x.c.limitf(n, "an effect takes more than 255 arguments")
		argc = 255
	}
	off := x.fb.Offset()
	x.span(n)
	x.fb.Effect(kind, uint8(argc))
	x.c.ops |= 1 << kind
	idem := x.idempotent
	if tool != nil {
		x.c.mask |= bytecode.EffectMask(tool.Effects)
		idem = idem || x.c.toolIdempotent(tool)
	} else {
		x.c.mask |= kind.Mask()
	}
	a := x.annot
	if a == nil && !idem {
		return
	}
	site := bytecode.SiteAnnotation{Func: x.fb.Index(), CodeOffset: uint32(off), Idempotent: idem}
	if a != nil {
		site.SourceOffset = uint32(max(a.sp.Start.Offset, 0))
		site.SourceLen = uint32(max(a.sp.End.Offset-a.sp.Start.Offset, 0))
		site.Timeout = a.timeout
		site.RetryMax = uint32(min(max(a.retryMax, 0), 1<<31))
		if site.RetryMax > 0 {
			site.BackoffInitial, site.BackoffMax = a.backoffInitial, a.backoffMax
		}
	}
	x.c.annots.Sites = append(x.c.annots.Sites, site)
}

// setAnnotations stores the ANNOTS section when there is anything to
// tell the engine.
func (c *compiler) setAnnotations() {
	a := c.wf.Attrs
	c.annots.Timeout = a.Timeout
	c.annots.MaxEffects = uint32(min(max(a.MaxEffects, 0), bytecode.MaxEffectsAnnotation))
	c.annots.OnUnknownRetry = a.OnUnknownRetry
	c.annots.Idempotent = a.Idempotent
	if c.annots.Timeout == 0 && c.annots.MaxEffects == 0 && !c.annots.OnUnknownRetry &&
		!c.annots.Idempotent && len(c.annots.Sites) == 0 {
		return
	}
	c.b.SetAnnotations(&c.annots)
}
