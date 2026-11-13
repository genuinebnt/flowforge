package contentstore

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/diff"
)

// Stored diff records (V2 §6.3: "diff hunks and semantic records are
// canonical JSON, zstd-compressed, stored as chunks").
//
// The hunks record holds each artifact's unified hunks in a compact form:
// the header plus a run-length script ("c3 d2 i1": 3 context lines, 2
// deleted, 1 inserted), whose lines are read back from the two snapshots'
// stored normalized texts (lines are stored once, in the snapshot). A
// hunk whose lines do not reproduce that way keeps them verbatim. A
// baseline diff (a key's first version against the empty document)
// stores no hunks and no changes at all: both are a pure function of the
// snapshot's stored processed capture and are recomputed. The semantic
// record holds the change set without hunks, side-by-side rows and
// per-artifact feature changes (Diff recomputes the full view; its hunks
// equal the stored ones, CSTORE-INV-04).

// hunksRecord is the hunks_ref blob, decoded to full hunks.
type hunksRecord struct {
	V         int             `json:"v"`
	Baseline  bool            `json:"baseline,omitempty"`
	Artifacts []artifactHunks `json:"artifacts"`
}

// artifactHunks are one artifact's unified hunks.
type artifactHunks struct {
	DocID string         `json:"doc_id"`
	Kind  content.Kind   `json:"kind"`
	Hunks []content.Hunk `json:"hunks,omitempty"`
	// Compact is the stored form of Hunks.
	Compact []compactHunk `json:"compact,omitempty"`
	// Vendor marks a vendored bundle or library module (V2 §12.14: excluded
	// from search unless asked for).
	Vendor bool `json:"vendor,omitempty"`
}

// compactHunk is a hunk by reference: header, run-length script, or the
// verbatim lines when the reference form does not reproduce them.
type compactHunk struct {
	Header string   `json:"h"`
	Ops    string   `json:"o,omitempty"`
	Lines  []string `json:"l,omitempty"`
}

// semanticRecord is the semantic_ref blob.
type semanticRecord struct {
	V         int               `json:"v"`
	ChangeSet content.ChangeSet `json:"change_set"`
}

// textLines splits a normalized text into lines without their "\n", as
// content hunks present them.
func textLines(b []byte) []string {
	ls := diff.SplitLines(b)
	for i := range ls {
		ls[i] = strings.TrimSuffix(ls[i], "\n")
	}
	return ls
}

// hunkStarts parses "@@ -A[,B] +C[,D] @@" into 0-based starts
// (diff.HunkHeader: an empty side names the line before it).
func hunkStarts(h string) (int, int, bool) {
	rest, ok := strings.CutPrefix(h, "@@ -")
	if !ok {
		return 0, 0, false
	}
	oldR, rest, ok := strings.Cut(rest, " +")
	newR, _, ok2 := strings.Cut(rest, " @@")
	if !ok || !ok2 {
		return 0, 0, false
	}
	start := func(r string) (int, bool) {
		a, b, has := strings.Cut(r, ",")
		n, err := strconv.Atoi(a)
		if err != nil || n < 0 {
			return 0, false
		}
		if has && b == "0" {
			return n, true
		}
		return n - 1, n > 0
	}
	o, ok := start(oldR)
	n, ok2 := start(newR)
	return o, n, ok && ok2
}

// expandHunk rebuilds a hunk's lines from the old and new texts.
func expandHunk(c compactHunk, old, new []string) (content.Hunk, bool) {
	if c.Ops == "" {
		return content.Hunk{Header: c.Header, Lines: c.Lines}, c.Lines != nil
	}
	o, n, ok := hunkStarts(c.Header)
	if !ok {
		return content.Hunk{}, false
	}
	h := content.Hunk{Header: c.Header}
	for op := range strings.FieldsSeq(c.Ops) {
		k, err := strconv.Atoi(op[1:])
		if err != nil || k <= 0 {
			return content.Hunk{}, false
		}
		switch op[0] {
		case 'c':
			if o+k > len(old) {
				return content.Hunk{}, false
			}
			for _, l := range old[o : o+k] {
				h.Lines = append(h.Lines, " "+l)
			}
			o, n = o+k, n+k
		case 'd':
			if o+k > len(old) {
				return content.Hunk{}, false
			}
			for _, l := range old[o : o+k] {
				h.Lines = append(h.Lines, "-"+l)
			}
			o += k
		case 'i':
			if n+k > len(new) {
				return content.Hunk{}, false
			}
			for _, l := range new[n : n+k] {
				h.Lines = append(h.Lines, "+"+l)
			}
			n += k
		default:
			return content.Hunk{}, false
		}
	}
	return h, true
}

// compactOf encodes a hunk by reference, falling back to its lines when
// the reference form would not reproduce it exactly.
func compactOf(h content.Hunk, old, new []string) compactHunk {
	var b strings.Builder
	for i := 0; i < len(h.Lines); {
		j := i
		for j < len(h.Lines) && h.Lines[j] != "" && h.Lines[i] != "" && h.Lines[j][0] == h.Lines[i][0] {
			j++
		}
		if j == i {
			return compactHunk{Header: h.Header, Lines: h.Lines}
		}
		op := map[byte]byte{' ': 'c', '-': 'd', '+': 'i'}[h.Lines[i][0]]
		if op == 0 {
			return compactHunk{Header: h.Header, Lines: h.Lines}
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteByte(op)
		b.WriteString(strconv.Itoa(j - i))
		i = j
	}
	c := compactHunk{Header: h.Header, Ops: b.String()}
	if got, ok := expandHunk(c, old, new); !ok || !slices.Equal(got.Lines, h.Lines) {
		return compactHunk{Header: h.Header, Lines: h.Lines}
	}
	return c
}

// artifactText returns the normalized text lines of doc in p (nil p or a
// missing doc: none).
func artifactText(p *content.Processed, doc string) []string {
	if p == nil {
		return nil
	}
	for i := range p.Artifacts {
		if p.Artifacts[i].DocID == doc {
			return textLines(p.Artifacts[i].Normalized.Text)
		}
	}
	return nil
}

// fullRecords are a change set's records with every line present (what
// the index reads and what ingest counts documents from).
func fullRecords(cs *content.ChangeSet, baseline bool) (hunksRecord, semanticRecord) {
	hr := hunksRecord{V: 1, Baseline: baseline, Artifacts: []artifactHunks{}}
	for _, a := range cs.Artifacts {
		if len(a.Hunks) > 0 {
			hr.Artifacts = append(hr.Artifacts, artifactHunks{DocID: a.DocID, Kind: a.Kind, Hunks: a.Hunks, Vendor: vendored(a)})
		}
	}
	sem := *cs
	sem.Artifacts = make([]content.ArtifactDiff, len(cs.Artifacts))
	for i, a := range cs.Artifacts {
		a.Hunks, a.SideBySide, a.FeatureChanges = nil, diff.SideBySide{}, nil
		sem.Artifacts[i] = a
	}
	return hr, semanticRecord{V: 1, ChangeSet: sem}
}

// diffRecords encodes a change set's two stored blobs (before the blob
// codec). prev is the old side, rebased to cur's URL for a lineage pair.
func diffRecords(cs *content.ChangeSet, baseline bool, prev, cur *content.Processed) (hunks, semantic []byte, err error) {
	hr, sr := fullRecords(cs, baseline)
	stored := hunksRecord{V: 1, Baseline: baseline, Artifacts: []artifactHunks{}}
	if baseline {
		sr.ChangeSet.Changes = nil
		sr.ChangeSet.Artifacts = nil
	} else {
		for _, a := range hr.Artifacts {
			old, new := artifactText(prev, a.DocID), artifactText(cur, a.DocID)
			sa := artifactHunks{DocID: a.DocID, Kind: a.Kind, Vendor: a.Vendor}
			for _, h := range a.Hunks {
				sa.Compact = append(sa.Compact, compactOf(h, old, new))
			}
			stored.Artifacts = append(stored.Artifacts, sa)
		}
	}
	if hunks, err = core.CanonicalJSON(stored); err != nil {
		return nil, nil, internal("encode hunks", err)
	}
	if semantic, err = core.CanonicalJSON(sr); err != nil {
		return nil, nil, internal("encode semantic record", err)
	}
	return hunks, semantic, nil
}

// expandRecords decodes stored records into full ones, given the two
// processed captures (prev rebased to cur's URL; nil for a baseline) and
// the diff options a baseline is recomputed with.
func expandRecords(hunks, semantic []byte, prev, cur *content.Processed, opts content.DiffOptions) (hunksRecord, semanticRecord, error) {
	var hr hunksRecord
	var sr semanticRecord
	if err := json.Unmarshal(hunks, &hr); err != nil {
		return hr, sr, integrity("hunks record: %v", err)
	}
	if err := json.Unmarshal(semantic, &sr); err != nil {
		return hr, sr, integrity("semantic record: %v", err)
	}
	if hr.Baseline {
		cs := sr.ChangeSet
		if err := baselineArtifacts(&cs, cur, opts); err != nil {
			return hr, sr, err
		}
		full, fsr := fullRecords(&cs, true)
		return full, fsr, nil
	}
	for i := range hr.Artifacts {
		a := &hr.Artifacts[i]
		old, new := artifactText(prev, a.DocID), artifactText(cur, a.DocID)
		for _, c := range a.Compact {
			h, ok := expandHunk(c, old, new)
			if !ok {
				return hr, sr, integrity("hunk %q of %s does not match the stored texts", c.Header, a.DocID)
			}
			a.Hunks = append(a.Hunks, h)
		}
		a.Compact = nil
	}
	return hr, sr, nil
}

// vendored reports a vendored bundle: a node_modules path, a bundler
// vendor chunk name (vendor, vendors~, chunk-vendors, framework,
// polyfills), or a JS artifact whose modules are all vendor or library
// modules (content module diff).
func vendored(a content.ArtifactDiff) bool {
	if vendorURL(a.DocID) {
		return true
	}
	if a.Modules != nil && a.Modules.VendorSuppressed > 0 && a.Modules.Unchanged == 0 && len(a.Modules.Changes) == 0 {
		return true
	}
	return false
}

// vendorURL applies the path rules of vendored.
func vendorURL(u string) bool {
	path := u
	if _, rest, ok := strings.Cut(u, "://"); ok {
		path = ""
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			path = rest[i:]
		}
	}
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	if strings.Contains(path, "/node_modules/") {
		return true
	}
	base := path[strings.LastIndexByte(path, '/')+1:]
	for _, p := range []string{"vendor.", "vendor-", "vendors.", "vendors-", "vendors~", "chunk-vendors", "framework.", "framework-", "polyfills.", "polyfills-"} {
		if strings.HasPrefix(base, p) {
			return true
		}
	}
	return false
}

// hunkSides splits a hunk's lines into the removed and added sides (the
// unified prefix stripped; context lines belong to neither).
func hunkSides(h content.Hunk) (removed, added []string) {
	for _, l := range h.Lines {
		if l == "" {
			continue
		}
		switch l[0] {
		case '-':
			removed = append(removed, l[1:])
		case '+':
			added = append(added, l[1:])
		}
	}
	return slices.Clip(removed), slices.Clip(added)
}
