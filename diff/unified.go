package diff

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// noNewline is the marker line after a line that lacks its terminator.
const noNewline = "\\ No newline at end of file\n"

// UnifiedOptions configures Unified.
type UnifiedOptions struct {
	// OldName and NewName, when either is non-empty, produce the "--- " and
	// "+++ " header lines.
	OldName, NewName string
	// Context is the number of context lines (DefaultContext is git's 3).
	Context int
}

// Unified renders a line-level script in the unified format of GNU diff and
// git. oldLines and newLines are the sequences the script was computed
// from, as SplitLines returns them (each line keeps its "\n"). A hunk
// header is "@@ -l,s +l,s @@": s is the number of lines on that side and
// is omitted when 1; l is the 1-based first line, or, for an empty side,
// the number of the line after which the change applies (0 at the start).
// A line without a trailing newline is followed by "\ No newline at end of
// file". A script without changes renders as nothing.
func Unified(oldLines, newLines []string, s EditScript, opts UnifiedOptions) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if len(oldLines) != s.OldLen || len(newLines) != s.NewLen {
		return nil, fmt.Errorf("%w: %d/%d lines, script expects %d/%d", ErrMismatch, len(oldLines), len(newLines), s.OldLen, s.NewLen)
	}
	hunks := Hunks(s, opts.Context)
	if len(hunks) == 0 {
		return nil, nil
	}
	var buf bytes.Buffer
	if opts.OldName != "" || opts.NewName != "" {
		buf.WriteString("--- " + opts.OldName + "\n+++ " + opts.NewName + "\n")
	}
	for _, h := range hunks {
		buf.WriteString(HunkHeader(h))
		buf.WriteByte('\n')
		for _, op := range h.Ops {
			switch op.Kind {
			case OpEqual:
				for _, l := range oldLines[op.OldStart:op.OldEnd] {
					writeLine(&buf, ' ', l)
				}
			case OpDelete:
				for _, l := range oldLines[op.OldStart:op.OldEnd] {
					writeLine(&buf, '-', l)
				}
			case OpInsert:
				for _, l := range newLines[op.NewStart:op.NewEnd] {
					writeLine(&buf, '+', l)
				}
			}
		}
	}
	return buf.Bytes(), nil
}

func writeLine(buf *bytes.Buffer, prefix byte, line string) {
	buf.WriteByte(prefix)
	buf.WriteString(line)
	if !strings.HasSuffix(line, "\n") {
		buf.WriteString("\n" + noNewline)
	}
}

// HunkHeader returns the "@@ -l,s +l,s @@" line of h (no newline).
func HunkHeader(h Hunk) string {
	return "@@ -" + rangeText(h.OldStart, h.OldEnd-h.OldStart) + " +" + rangeText(h.NewStart, h.NewEnd-h.NewStart) + " @@"
}

func rangeText(start, n int) string {
	switch n {
	case 0:
		return strconv.Itoa(start) + ",0"
	case 1:
		return strconv.Itoa(start + 1)
	default:
		return strconv.Itoa(start+1) + "," + strconv.Itoa(n)
	}
}

// Patch is a parsed single-file unified diff.
type Patch struct {
	OldName, NewName string
	Hunks            []PatchHunk
}

// PatchHunk is one parsed hunk. Starts and lengths are as written in the
// header (1-based, or the line before an empty side).
type PatchHunk struct {
	OldStart, OldLen int
	NewStart, NewLen int
	Lines            []PatchLine
}

// PatchLine is one hunk body line: Kind is ' ', '-' or '+', and Text is the
// line without its prefix, keeping its "\n" unless a "\ No newline at end
// of file" marker followed it.
type PatchLine struct {
	Kind byte
	Text string
}

// Unified-diff parser states.
type patchState uint8

const (
	stPreamble patchState = iota // before "---" or the first "@@"
	stOldName                    // after "---", expecting "+++"
	stHunkHead                   // expecting "@@" or end of input
	stHunkBody                   // inside a hunk, lines still owed
)

// ParseUnified parses one file's unified diff: optional preamble lines
// (ignored until "--- " or "@@"), optional "--- "/"+++ " names, then hunks.
// Each hunk body must hold exactly the header's line counts; a "\ No
// newline at end of file" marker strips the previous line's "\n". Errors
// wrap ErrMalformedPatch and name the 1-based input line.
func ParseUnified(text []byte) (Patch, error) {
	var p Patch
	state := stPreamble
	var cur *PatchHunk
	oldLeft, newLeft := 0, 0
	lines := SplitLines(text)
	bad := func(i int, msg string) error {
		return fmt.Errorf("%w: line %d: %s", ErrMalformedPatch, i+1, msg)
	}
	for i, raw := range lines {
		if !strings.HasSuffix(raw, "\n") {
			return p, bad(i, "missing newline")
		}
		line := raw[:len(raw)-1]
		switch state {
		case stPreamble:
			switch {
			case line+"\n" == noNewline:
				return p, bad(i, "no-newline marker before any hunk")
			case strings.HasPrefix(line, "--- "):
				p.OldName = line[4:]
				state = stOldName
			case strings.HasPrefix(line, "@@"):
				h, err := parseHunkHeader(line)
				if err != nil {
					return p, bad(i, err.Error())
				}
				p.Hunks = append(p.Hunks, h)
				cur = &p.Hunks[len(p.Hunks)-1]
				oldLeft, newLeft = h.OldLen, h.NewLen
				state = stHunkBody
				if oldLeft == 0 && newLeft == 0 {
					return p, bad(i, "empty hunk")
				}
			}
		case stOldName:
			if !strings.HasPrefix(line, "+++ ") {
				return p, bad(i, `expected "+++ "`)
			}
			p.NewName = line[4:]
			state = stHunkHead
		case stHunkHead:
			switch {
			case strings.HasPrefix(line, "@@"):
				h, err := parseHunkHeader(line)
				if err != nil {
					return p, bad(i, err.Error())
				}
				if h.OldLen == 0 && h.NewLen == 0 {
					return p, bad(i, "empty hunk")
				}
				p.Hunks = append(p.Hunks, h)
				cur = &p.Hunks[len(p.Hunks)-1]
				oldLeft, newLeft = h.OldLen, h.NewLen
				state = stHunkBody
			case line+"\n" == noNewline:
				if cur == nil || len(cur.Lines) == 0 || !strings.HasSuffix(cur.Lines[len(cur.Lines)-1].Text, "\n") {
					return p, bad(i, "misplaced no-newline marker")
				}
				last := &cur.Lines[len(cur.Lines)-1]
				last.Text = last.Text[:len(last.Text)-1]
			default:
				return p, bad(i, `expected "@@"`)
			}
		case stHunkBody:
			if line == "" {
				return p, bad(i, "empty hunk line")
			}
			if line+"\n" == noNewline {
				if len(cur.Lines) == 0 || !strings.HasSuffix(cur.Lines[len(cur.Lines)-1].Text, "\n") {
					return p, bad(i, "misplaced no-newline marker")
				}
				last := &cur.Lines[len(cur.Lines)-1]
				last.Text = last.Text[:len(last.Text)-1]
				continue
			}
			kind := line[0]
			switch kind {
			case ' ':
				oldLeft--
				newLeft--
			case '-':
				oldLeft--
			case '+':
				newLeft--
			default:
				return p, bad(i, fmt.Sprintf("bad line prefix %q", kind))
			}
			if oldLeft < 0 || newLeft < 0 {
				return p, bad(i, "hunk longer than its header")
			}
			cur.Lines = append(cur.Lines, PatchLine{Kind: kind, Text: line[1:] + "\n"})
			if oldLeft == 0 && newLeft == 0 {
				state = stHunkHead
			}
		}
	}
	switch state {
	case stOldName:
		return p, fmt.Errorf("%w: missing \"+++ \" line", ErrMalformedPatch)
	case stHunkBody:
		return p, fmt.Errorf("%w: truncated hunk", ErrMalformedPatch)
	}
	return p, nil
}

func parseHunkHeader(line string) (PatchHunk, error) {
	rest, ok := strings.CutPrefix(line, "@@ -")
	if !ok {
		return PatchHunk{}, fmt.Errorf("bad hunk header %q", line)
	}
	oldPart, rest, ok := strings.Cut(rest, " +")
	if !ok {
		return PatchHunk{}, fmt.Errorf("bad hunk header %q", line)
	}
	newPart, tail, ok := strings.Cut(rest, " @@")
	if !ok || (tail != "" && tail[0] != ' ') {
		return PatchHunk{}, fmt.Errorf("bad hunk header %q", line)
	}
	var h PatchHunk
	var err error
	if h.OldStart, h.OldLen, err = parseRange(oldPart); err != nil {
		return h, err
	}
	if h.NewStart, h.NewLen, err = parseRange(newPart); err != nil {
		return h, err
	}
	return h, nil
}

func parseRange(s string) (start, n int, err error) {
	a, b, hasLen := strings.Cut(s, ",")
	if start, err = parseCount(a); err != nil {
		return 0, 0, err
	}
	n = 1
	if hasLen {
		if n, err = parseCount(b); err != nil {
			return 0, 0, err
		}
	}
	if n > 0 && start == 0 {
		return 0, 0, fmt.Errorf("range %q starts at line 0", s)
	}
	return start, n, nil
}

func parseCount(s string) (int, error) {
	if s == "" || len(s) > 10 {
		return 0, fmt.Errorf("bad number %q", s)
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("bad number %q", s)
		}
	}
	return strconv.Atoi(s)
}

// ApplyUnified applies p to oldLines (as SplitLines returns them) and
// returns the new lines. Application is strict, without git's offset or
// fuzz search: every hunk must start exactly where its header says, hunks
// must be in order and non-overlapping, and every context and deleted line
// must match. Errors wrap ErrMismatch.
func ApplyUnified(oldLines []string, p Patch) ([]string, error) {
	var out []string
	pos := 0
	for hi, h := range p.Hunks {
		start := h.OldStart - 1
		if h.OldLen == 0 {
			start = h.OldStart
		}
		if start < pos || start > len(oldLines) {
			return nil, fmt.Errorf("%w: hunk %d starts at old line %d, cursor %d of %d", ErrMismatch, hi+1, h.OldStart, pos, len(oldLines))
		}
		out = append(out, oldLines[pos:start]...)
		pos = start
		newStart := h.NewStart - 1
		if h.NewLen == 0 {
			newStart = h.NewStart
		}
		if len(out) != newStart {
			return nil, fmt.Errorf("%w: hunk %d starts at new line %d, output has %d", ErrMismatch, hi+1, h.NewStart, len(out))
		}
		for _, l := range h.Lines {
			switch l.Kind {
			case ' ', '-':
				if pos >= len(oldLines) || oldLines[pos] != l.Text {
					return nil, fmt.Errorf("%w: hunk %d: old line %d differs", ErrMismatch, hi+1, pos+1)
				}
				if l.Kind == ' ' {
					out = append(out, l.Text)
				}
				pos++
			case '+':
				out = append(out, l.Text)
			}
		}
	}
	return append(out, oldLines[pos:]...), nil
}
