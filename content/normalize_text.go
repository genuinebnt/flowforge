package content

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// textRules is the text@1 rule table (text, robots.txt, security.txt,
// server-side source and, per decision D2, TypeScript/TSX/JSX).
var textRules = []string{
	"lines:cr lf and cr → lf; a utf-8 bom starting the first non-blank line dropped; trailing spaces and tabs trimmed",
	"lines:runs of blank lines → one blank line; leading and trailing blank lines dropped; final lf",
	"mask:iso-8601 date-times → timestamp",
	"d2:ts, tsx and jsx are text; they are never handed to the js parser",
}

// binaryRules is the binary@1 rule table.
var binaryRules = []string{
	"emit:one line \"binary sha256:<hex> size:<n>\"; binary bodies are compared by hash only",
}

func normalizeText(body []byte, _ Kind, m *masker) normResult {
	s := strings.ReplaceAll(strings.ReplaceAll(string(body), "\r\n", "\n"), "\r", "\n")
	var w lineWriter
	blank := false
	wrote := false
	for line := range strings.SplitSeq(s, "\n") {
		if !wrote {
			// The BOM is dropped wherever the first written line starts,
			// so leading blank lines before it cannot hide it.
			line = strings.TrimPrefix(line, "\xEF\xBB\xBF")
		}
		line = strings.TrimRight(line, " \t")
		if line == "" {
			blank = wrote
			continue
		}
		if blank {
			w.line("")
			blank = false
		}
		w.line(maskText(line, m))
		wrote = true
	}
	return normResult{text: w.bytes(), grade: GradeComplete}
}

func normalizeBinary(body []byte) normResult {
	if isBinaryLine(body) {
		// Already normalized: keep it (idempotence).
		return normResult{text: body, grade: GradeUnsupported, diags: []string{"binary_hash_only"}}
	}
	sum := sha256.Sum256(body)
	return normResult{
		text:  []byte("binary sha256:" + hex.EncodeToString(sum[:]) + " size:" + strconv.Itoa(len(body)) + "\n"),
		grade: GradeUnsupported, diags: []string{"binary_hash_only"},
	}
}

// isBinaryLine reports the binary@1 output form itself.
func isBinaryLine(b []byte) bool {
	s, ok := strings.CutPrefix(string(b), "binary sha256:")
	if !ok || len(s) < 64+7 || !isHexString(s[:64]) {
		return false
	}
	n, ok := strings.CutPrefix(s[64:], " size:")
	n, ok2 := strings.CutSuffix(n, "\n")
	return ok && ok2 && isAllDigits(n)
}
