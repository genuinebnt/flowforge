package diff

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Span is a half-open byte range [Start, End) within one line.
type Span struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// RefineLine diffs two versions of a line at word level (SplitWords tokens,
// Myers, then CleanupSemantic with TextScorer) and returns the changed byte
// ranges of each side, sorted and disjoint. Removing the spans from both
// lines leaves the same text.
func RefineLine(oldLine, newLine string) (oldSpans, newSpans []Span) {
	ow, nw := SplitWords(oldLine), SplitWords(newLine)
	in := NewInterner()
	a, b := in.InternAll(ow), in.InternAll(nw)
	r, err := Diff(a, b, Options{Algorithm: Myers})
	if err != nil {
		return nil, nil
	}
	s, err := CleanupSemantic(a, r.Script, CleanupOptions{Score: TextScorer(in)})
	if err != nil {
		s = r.Script
	}
	return tokenSpans(ow, nw, s)
}

// tokenSpans converts the changed token ranges of s into byte spans.
func tokenSpans(ow, nw []string, s EditScript) (oldSpans, newSpans []Span) {
	offs := func(toks []string) []int {
		o := make([]int, len(toks)+1)
		for i, t := range toks {
			o[i+1] = o[i] + len(t)
		}
		return o
	}
	oo, no := offs(ow), offs(nw)
	for _, op := range s.Ops {
		switch op.Kind {
		case OpDelete:
			oldSpans = append(oldSpans, Span{oo[op.OldStart], oo[op.OldEnd]})
		case OpInsert:
			newSpans = append(newSpans, Span{no[op.NewStart], no[op.NewEnd]})
		}
	}
	return oldSpans, newSpans
}

// Row kinds of the side-by-side form.
const (
	RowContext = "context"
	RowDelete  = "delete"
	RowInsert  = "insert"
	RowChange  = "change"
)

// SideBySide is the structured side-by-side form of a line diff, for
// viewers (the gateway's diff endpoint). Its JSON encoding is
// deterministic.
type SideBySide struct {
	Hunks []SideHunk `json:"hunks"`
}

// SideHunk is one hunk: its unified header and its rows.
type SideHunk struct {
	Header string    `json:"header"`
	Rows   []SideRow `json:"rows"`
}

// SideRow is one row. OldNo/NewNo are 1-based line numbers, 0 when the
// side is empty. Old/New are the raw lines (with their "\n"). A "change"
// row pairs the i-th deleted with the i-th inserted line of a change
// region and carries word-level spans; surplus lines are "delete" or
// "insert" rows.
type SideRow struct {
	Kind     string `json:"kind"`
	OldNo    int    `json:"old_no"`
	NewNo    int    `json:"new_no"`
	Old      string `json:"old"`
	New      string `json:"new"`
	OldSpans []Span `json:"old_spans,omitempty"`
	NewSpans []Span `json:"new_spans,omitempty"`
}

// SideBySideOf builds the side-by-side form of a line-level script.
func SideBySideOf(oldLines, newLines []string, s EditScript, context int) (SideBySide, error) {
	if err := s.Validate(); err != nil {
		return SideBySide{}, err
	}
	if len(oldLines) != s.OldLen || len(newLines) != s.NewLen {
		return SideBySide{}, fmt.Errorf("%w: %d/%d lines, script expects %d/%d", ErrMismatch, len(oldLines), len(newLines), s.OldLen, s.NewLen)
	}
	out := SideBySide{Hunks: []SideHunk{}}
	for _, h := range Hunks(s, context) {
		sh := SideHunk{Header: HunkHeader(h)}
		for i := 0; i < len(h.Ops); i++ {
			op := h.Ops[i]
			switch op.Kind {
			case OpEqual:
				for k := 0; k < op.Len(); k++ {
					sh.Rows = append(sh.Rows, SideRow{Kind: RowContext, OldNo: op.OldStart + k + 1, NewNo: op.NewStart + k + 1, Old: oldLines[op.OldStart+k], New: newLines[op.NewStart+k]})
				}
			case OpInsert:
				for k := op.NewStart; k < op.NewEnd; k++ {
					sh.Rows = append(sh.Rows, SideRow{Kind: RowInsert, NewNo: k + 1, New: newLines[k]})
				}
			case OpDelete:
				var ins Op
				if i+1 < len(h.Ops) && h.Ops[i+1].Kind == OpInsert {
					ins = h.Ops[i+1]
					i++
				}
				nd, ni := op.Len(), ins.NewEnd-ins.NewStart
				for k := 0; k < max(nd, ni); k++ {
					switch {
					case k < nd && k < ni:
						ol, nl := oldLines[op.OldStart+k], newLines[ins.NewStart+k]
						os, ns := RefineLine(ol, nl)
						sh.Rows = append(sh.Rows, SideRow{Kind: RowChange, OldNo: op.OldStart + k + 1, NewNo: ins.NewStart + k + 1, Old: ol, New: nl, OldSpans: os, NewSpans: ns})
					case k < nd:
						sh.Rows = append(sh.Rows, SideRow{Kind: RowDelete, OldNo: op.OldStart + k + 1, Old: oldLines[op.OldStart+k]})
					default:
						sh.Rows = append(sh.Rows, SideRow{Kind: RowInsert, NewNo: ins.NewStart + k + 1, New: newLines[ins.NewStart+k]})
					}
				}
			}
		}
		out.Hunks = append(out.Hunks, sh)
	}
	return out, nil
}

// MarshalSideBySide encodes the side-by-side form as compact JSON without
// HTML escaping.
func MarshalSideBySide(v SideBySide) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
