package content

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/genuinebnt/flowforge/diff"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/sketch"
)

// Diff outcomes (spec 33.10; V2 §12.5 counted outcomes). DiffChanged and
// DiffStructural are changes (ChangeSet.IsChange); the others are counted
// for the noise metric (masked_only and reordered are not changes).
const (
	DiffNoChange        = "no_change"        // identical bytes
	DiffNormalizedEqual = "normalized_equal" // bytes differ, normalized text equal, same masked values
	DiffMaskedOnly      = "masked_only"      // only masked volatile values changed
	DiffReordered       = "reordered"        // same features and lines (or modules), different order
	DiffStructural      = "structural"       // text changed, no level-3 change
	DiffChanged         = "changed"          // a level-3 change
	DiffDegraded        = "degraded"         // the new capture is degraded: no diff
)

// Newness of a change (spec 33.10; V2 §12.13): likely_new against a
// complete previous capture, first_seen_only without one, pre_existing for
// the baseline and for returns.
const (
	NewnessLikelyNew     = "likely_new"
	NewnessPreExisting   = "pre_existing"
	NewnessFirstSeenOnly = "first_seen_only"
)

// FeatureChange is one level-3 change.
type FeatureChange struct {
	// Op is added, removed, modified or returned (added again after it
	// was absent from the previous version but present in history).
	Op      string   `json:"op"`
	Feature Feature  `json:"feature"`       // the new version (the old one for removed)
	Old     *Feature `json:"old,omitempty"` // the previous version, for modified
	Newness string   `json:"newness"`       // a Newness* value
}

// Hunk is one presented hunk: the unified header and the changed lines.
type Hunk struct {
	Header string   `json:"header"`
	Lines  []string `json:"lines"`
}

// ArtifactDiff is the comparison of two versions of one artifact.
type ArtifactDiff struct {
	DocID string `json:"doc_id"`
	Kind  Kind   `json:"kind"`
	// Levels that differ: hash, text, semantic (oam level names).
	Levels  []string `json:"levels,omitempty"`
	Outcome string   `json:"outcome"`
	// Algorithm, BoundLevel and Fallback record how the text diff ran
	// (diff.BoundedDiff level and fallback reason).
	Algorithm  string `json:"algorithm,omitempty"`
	BoundLevel string `json:"bound_level,omitempty"`
	Fallback   string `json:"fallback,omitempty"`
	Capped     bool   `json:"capped,omitempty"`
	// Inserted and Deleted count lines of the presented line diff.
	Inserted       int             `json:"inserted"`
	Deleted        int             `json:"deleted"`
	Hunks          []Hunk          `json:"hunks,omitempty"`
	SideBySide     diff.SideBySide `json:"side_by_side"`
	FeatureChanges []FeatureChange `json:"feature_changes,omitempty"`
	Modules        *ModuleDiff     `json:"modules,omitempty"`
}

// HashOnly reports a diff that fell back to comparing hashes.
func (d ArtifactDiff) HashOnly() bool { return d.BoundLevel == diff.LevelHash.String() }

// DiffOptions configures DiffArtifacts and Compare.
type DiffOptions struct {
	Policy DiffPolicy
	// Bounds are the §5 caps (zero fields: diff's defaults).
	Bounds  diff.Bounds
	Context int // hunk context lines (0: 3)
	// History holds the feature ids (kind|key) seen in any version before
	// the previous one, for "returned". It must be sorted ascending
	// (featureChanges binary-searches it).
	History []string
	// Comparable says the previous version was a complete capture of the
	// same (URL, profile), so an absence there is a real negative
	// (newness likely_new rather than first_seen_only).
	Comparable bool
	// Rename allows diffing different doc ids (a lineage predecessor).
	Rename bool
}

// DiffArtifacts compares two versions of one artifact (V2 §5 change
// classification): level hash when the bytes differ, text when the
// normalized texts differ, semantic when the level-3 features differ. For
// bundles the module diff only refines the outcome (all modules matched:
// reordered); it never adds a semantic level. The text diff goes through
// diff.BoundedDiff under the policy, then is presented line by line.
func DiffArtifacts(old, new *Artifact, opts DiffOptions) (ArtifactDiff, error) {
	if old.DocID != new.DocID && !opts.Rename || old.Kind != new.Kind {
		return ArtifactDiff{}, fmt.Errorf("%w: %s (%s) vs %s (%s)", ErrIncomparable, old.DocID, old.Kind, new.DocID, new.Kind)
	}
	d := ArtifactDiff{DocID: new.DocID, Kind: new.Kind, SideBySide: diff.SideBySide{Hunks: []diff.SideHunk{}}}
	hashDiff := old.BodySHA256 != new.BodySHA256
	textDiff := old.NormalizedSHA256 != new.NormalizedSHA256
	d.FeatureChanges = featureChanges(old.Features.Features, new.Features.Features, opts)
	if new.Kind == KindJS && (len(old.Modules) > 0 || len(new.Modules) > 0) {
		md := DiffModules(old.Modules, new.Modules)
		d.Modules = &md
	}
	// Module-level lineage refines the text level (V2 §12.6): an unchanged
	// module is no change wherever it moved. It does not create level-3
	// changes; those come from the features.
	semantic := len(d.FeatureChanges) > 0
	if hashDiff {
		d.Levels = append(d.Levels, oam.LevelHash)
	}
	if textDiff {
		d.Levels = append(d.Levels, oam.LevelText)
		if err := d.textDiff(old.Normalized.Text, new.Normalized.Text, opts); err != nil {
			return ArtifactDiff{}, err
		}
	}
	if semantic {
		d.Levels = append(d.Levels, oam.LevelSemantic)
	}
	switch {
	case semantic:
		d.Outcome = DiffChanged
	case !hashDiff && !textDiff:
		d.Outcome = DiffNoChange
	case !textDiff && old.Normalized.MaskDigest != new.Normalized.MaskDigest:
		d.Outcome = DiffMaskedOnly
	case !textDiff:
		d.Outcome = DiffNormalizedEqual
	case sameLineMultiset(old.Normalized.Text, new.Normalized.Text) || d.Modules != nil && len(d.Modules.Changes) == 0 && len(new.Modules) > 0:
		d.Outcome = DiffReordered
	default:
		d.Outcome = DiffStructural
	}
	return d, nil
}

// textDiff runs the bounded text diff of a and b and fills the presented
// hunks and side-by-side form (nothing past the hash level).
func (d *ArtifactDiff) textDiff(a, b []byte, opts DiffOptions) error {
	bounds := opts.Policy.Bounds(max(len(a), len(b)), opts.Bounds)
	r, err := diff.BoundedDiff(a, b, bounds)
	if err != nil {
		return fmt.Errorf("content: diff: %w", err)
	}
	d.BoundLevel, d.Fallback, d.Capped = r.Level.String(), r.Fallback.String(), r.Capped
	switch r.Level {
	case diff.LevelTokens:
		d.Algorithm = bounds.TokenAlgorithm.String()
	case diff.LevelLines:
		d.Algorithm = bounds.LineAlgorithm.String()
	case diff.LevelHash:
		d.Algorithm = "hash"
		return nil
	}
	// Presentation is line based: reuse a line-level script, or diff the
	// lines (with the same token algorithm, input already within bounds).
	var oldL, newL []string
	var script diff.EditScript
	if r.Level == diff.LevelLines {
		oldL, newL, script = r.Units.Values(r.Old), r.Units.Values(r.New), r.Script
	} else {
		in := diff.NewInterner()
		oldL, newL = diff.SplitLines(a), diff.SplitLines(b)
		res, err := diff.Diff(in.InternAll(oldL), in.InternAll(newL), diff.Options{Algorithm: bounds.TokenAlgorithm})
		if err != nil {
			return fmt.Errorf("content: diff lines: %w", err)
		}
		script = res.Script
	}
	ctx := opts.Context
	if ctx <= 0 {
		ctx = diff.DefaultContext
	}
	for _, h := range diff.Hunks(script, ctx) {
		hk := Hunk{Header: diff.HunkHeader(h)}
		for _, op := range h.Ops {
			switch op.Kind {
			case diff.OpEqual:
				for _, l := range oldL[op.OldStart:op.OldEnd] {
					hk.Lines = append(hk.Lines, " "+strings.TrimSuffix(l, "\n"))
				}
			case diff.OpDelete:
				d.Deleted += op.OldEnd - op.OldStart
				for _, l := range oldL[op.OldStart:op.OldEnd] {
					hk.Lines = append(hk.Lines, "-"+strings.TrimSuffix(l, "\n"))
				}
			case diff.OpInsert:
				d.Inserted += op.NewEnd - op.NewStart
				for _, l := range newL[op.NewStart:op.NewEnd] {
					hk.Lines = append(hk.Lines, "+"+strings.TrimSuffix(l, "\n"))
				}
			}
		}
		d.Hunks = append(d.Hunks, hk)
	}
	sbs, err := diff.SideBySideOf(oldL, newL, script, ctx)
	if err != nil {
		return fmt.Errorf("content: side by side: %w", err)
	}
	d.SideBySide = sbs
	return nil
}

// sameLineMultiset reports two texts with the same lines in a different
// order.
func sameLineMultiset(a, b []byte) bool {
	la, lb := diff.SplitLines(a), diff.SplitLines(b)
	if len(la) != len(lb) {
		return false
	}
	slices.Sort(la)
	slices.Sort(lb)
	return slices.Equal(la, lb)
}

// featureChanges compares feature sets by id (kind|key). The document a
// feature came from is not part of its identity.
func featureChanges(old, new []Feature, opts DiffOptions) []FeatureChange {
	om, nm := byID(old), byID(new)
	newness := NewnessFirstSeenOnly
	if opts.Comparable {
		newness = NewnessLikelyNew
	}
	var out []FeatureChange
	for id, f := range nm {
		o, had := om[id]
		switch {
		case !had:
			op, nw := "added", newness
			if _, ok := slices.BinarySearch(opts.History, id); ok {
				op, nw = "returned", NewnessPreExisting
			}
			out = append(out, FeatureChange{Op: op, Feature: f, Newness: nw})
		case !f.sameContent(o):
			oc := o
			out = append(out, FeatureChange{Op: "modified", Feature: f, Old: &oc, Newness: newness})
		}
	}
	for id, f := range om {
		if _, ok := nm[id]; !ok {
			out = append(out, FeatureChange{Op: "removed", Feature: f, Newness: newness})
		}
	}
	// PORT: map iteration is random; sort for byte-identical output.
	slices.SortFunc(out, func(a, b FeatureChange) int {
		return cmp.Or(cmp.Compare(a.Feature.Kind, b.Feature.Kind), cmp.Compare(a.Feature.Key, b.Feature.Key), cmp.Compare(a.Op, b.Op))
	})
	return out
}

// ChangeSet is the comparison of two processed captures of one (URL,
// profile).
type ChangeSet struct {
	URL       string         `json:"url"`
	Profile   string         `json:"profile"`
	Artifacts []ArtifactDiff `json:"artifacts"`
	// Levels is the union of the artifacts' levels.
	Levels  []string `json:"levels,omitempty"`
	Outcome string   `json:"outcome"`
	Class   string   `json:"class,omitempty"`
	Newness string   `json:"newness,omitempty"`
	Cause   string   `json:"cause"`
	// Baseline marks the first complete capture: graph data, no feed
	// items or alerts.
	Baseline bool  `json:"baseline,omitempty"`
	Score    Score `json:"score"`
	// Changes are the capture-level feature changes, sorted.
	Changes []FeatureChange `json:"changes,omitempty"`
	// Summary holds the content_changed summary counts; zero counts are
	// left out and an empty summary is nil.
	//
	// PORT: encoding/json writes map keys sorted, so the JSON is stable;
	// a port serializes the keys in sorted order too.
	Summary map[string]int `json:"summary,omitempty"`
}

// IsChange reports a change worth a content_changed event: the outcome is
// changed or structural, or the change set is the baseline.
func (c ChangeSet) IsChange() bool {
	return c.Outcome == DiffChanged || c.Outcome == DiffStructural || c.Baseline
}

// Compare diffs every artifact of cur against the same doc id in prev
// (prev nil: the baseline), and classifies the result: class, newness,
// cause (causeOf), baseline and score. A degraded current capture gets no
// diff (V2 §12.5). Without prev the result is the baseline when coverage is
// complete, with outcome changed and every level set either way.
func Compare(prev, cur *Processed, opts DiffOptions) (ChangeSet, error) {
	cs := ChangeSet{URL: cur.URL, Profile: cur.Profile, Cause: oam.CauseTarget}
	if prev != nil && (prev.URL != cur.URL || prev.Profile != cur.Profile) {
		return cs, fmt.Errorf("%w: (%s, %s) vs (%s, %s)", ErrIncomparable, prev.URL, prev.Profile, cur.URL, cur.Profile)
	}
	if cur.Integrity.Degraded() {
		cs.Outcome = DiffDegraded
		return cs, nil
	}
	if prev == nil {
		cs.Baseline = cur.Coverage == oam.CoverageComplete
		cs.Outcome = DiffChanged
		cs.Levels = []string{oam.LevelHash, oam.LevelText, oam.LevelSemantic}
		cs.Newness = NewnessPreExisting
		cs.Class = ClassOf(nil, cur.Features())
		cs.Summary = map[string]int{"features": len(cur.Features())}
		return cs, nil
	}
	opts.Comparable = opts.Comparable || prev.Coverage == oam.CoverageComplete
	prevBy := map[string]*Artifact{}
	for i := range prev.Artifacts {
		prevBy[prev.Artifacts[i].DocID] = &prev.Artifacts[i]
	}
	empty := func(a *Artifact) *Artifact {
		e := &Artifact{DocID: a.DocID, Kind: a.Kind, Normalized: Normalized{Kind: a.Kind}}
		e.BodySHA256, e.NormalizedSHA256 = sha256Hex(nil), sha256Hex(nil)
		return e
	}
	seen := map[string]bool{}
	for i := range cur.Artifacts {
		a := &cur.Artifacts[i]
		seen[a.DocID] = true
		o, ok := prevBy[a.DocID]
		if !ok || o.Kind != a.Kind {
			o = empty(a)
		}
		d, err := DiffArtifacts(o, a, opts)
		if err != nil {
			return cs, err
		}
		cs.Artifacts = append(cs.Artifacts, d)
	}
	for i := range prev.Artifacts {
		o := &prev.Artifacts[i]
		if seen[o.DocID] {
			continue
		}
		d, err := DiffArtifacts(o, empty(o), opts)
		if err != nil {
			return cs, err
		}
		cs.Artifacts = append(cs.Artifacts, d)
	}
	cs.classify(prev, cur, opts)
	return cs, nil
}

// classify aggregates the artifact diffs. Level-3 changes are computed
// over the whole capture (body plus derived documents), so a feature that
// moved between documents (an endpoint from an inline script to an
// external one, a renumbered #script-n) is not a change.
func (cs *ChangeSet) classify(prev, cur *Processed, opts DiffOptions) {
	changes := featureChanges(prev.Features(), cur.Features(), opts)
	levels := map[string]bool{}
	outcome := DiffNoChange
	rank := map[string]int{DiffNoChange: 0, DiffNormalizedEqual: 1, DiffMaskedOnly: 2, DiffReordered: 3, DiffStructural: 4, DiffChanged: 5}
	headersOnly := true
	sum := map[string]int{}
	for _, d := range cs.Artifacts {
		for _, l := range d.Levels {
			if l != oam.LevelSemantic {
				levels[l] = true
			}
		}
		o := d.Outcome
		if o == DiffChanged {
			// Per-document level-3 differences are re-decided at capture
			// level below; here the document counts as a text change.
			o = DiffStructural
			if !slices.Contains(d.Levels, oam.LevelText) {
				o = DiffNormalizedEqual
			}
		}
		if rank[o] > rank[outcome] {
			outcome = o
		}
		if d.Outcome != DiffNoChange && d.Kind != KindHeaders {
			headersOnly = false
		}
		sum["hunks"] += len(d.Hunks)
		sum["inserted_lines"] += d.Inserted
		sum["deleted_lines"] += d.Deleted
		if d.Modules != nil {
			sum["modules_changed"] += len(d.Modules.Changes)
			sum["modules_moved"] += d.Modules.Moved
			sum["vendor_suppressed"] += d.Modules.VendorSuppressed
		}
	}
	for _, c := range changes {
		sum["features_"+c.Op]++
	}
	if len(changes) > 0 {
		outcome = DiffChanged
		levels[oam.LevelSemantic] = true
	}
	for _, l := range []string{oam.LevelHash, oam.LevelText, oam.LevelSemantic} {
		if levels[l] {
			cs.Levels = append(cs.Levels, l)
		}
	}
	cs.Outcome = outcome
	for k, v := range sum {
		if v == 0 {
			delete(sum, k)
		}
	}
	if len(sum) == 0 {
		sum = nil
	}
	cs.Summary = sum
	cs.Changes = changes
	if outcome != DiffChanged && outcome != DiffStructural {
		return
	}
	switch {
	case len(changes) == 0 && headersOnly:
		cs.Class = oam.ClassMetadata
	case len(changes) == 0:
		cs.Class = oam.ClassStructural
	default:
		cs.Class = classOfChanges(changes)
	}
	cs.Newness = NewnessPreExisting
	for _, c := range changes {
		switch c.Newness {
		case NewnessLikelyNew:
			cs.Newness = NewnessLikelyNew
		case NewnessFirstSeenOnly:
			if cs.Newness != NewnessLikelyNew {
				cs.Newness = NewnessFirstSeenOnly
			}
		}
	}
	cs.Cause = causeOf(prev, cur)
	cs.Score = ScoreChanges(scoreInputs(changes))
}

// ClassOf classifies the difference between two feature lists (the
// baseline uses old == nil).
func ClassOf(old, new []Feature) string {
	cs := featureChanges(old, new, DiffOptions{})
	if len(cs) == 0 {
		return oam.ClassStructural
	}
	return classOfChanges(cs)
}

// classOfChanges: semantic_endpoint when an endpoint, form, parameter or
// GraphQL operation changed; metadata when only headers changed;
// observation for every other level-3 change. Catalog (a new advisory
// against an unchanged library) is produced by the advisory matcher, not
// here (decision D4).
func classOfChanges(cs []FeatureChange) string {
	onlyHeaders := true
	for _, c := range cs {
		switch c.Feature.Kind {
		case FeatEndpoint, FeatSchemaEndpoint, FeatForm, FeatParameter, FeatGraphQLOperation:
			return oam.ClassSemanticEndpoint
		case FeatHeader:
		default:
			onlyHeaders = false
		}
	}
	if onlyHeaders {
		return oam.ClassMetadata
	}
	return oam.ClassObservation
}

// causeOf: when every current artifact has a previous one with the same
// raw body, a difference can only come from the readers: normalizer when a
// normalizer version or rule table differs, else parser when the extractor
// set differs. Otherwise the target changed, and so it did when only the
// number of artifacts differs (a previous artifact is gone) while the
// readers are the same.
func causeOf(prev, cur *Processed) string {
	pb := map[string]*Artifact{}
	for i := range prev.Artifacts {
		pb[prev.Artifacts[i].DocID] = &prev.Artifacts[i]
	}
	normDiff, exDiff := false, false
	for i := range cur.Artifacts {
		a := &cur.Artifacts[i]
		o, ok := pb[a.DocID]
		if !ok || o.BodySHA256 != a.BodySHA256 {
			return oam.CauseTarget
		}
		if o.Normalized.Version != a.Normalized.Version || o.Normalized.RuleTableSHA256 != a.Normalized.RuleTableSHA256 {
			normDiff = true
		}
		if !slices.Equal(o.ExtractorIDs(), a.ExtractorIDs()) {
			exDiff = true
		}
	}
	if len(prev.Artifacts) != len(cur.Artifacts) && !normDiff && !exDiff {
		return oam.CauseTarget
	}
	switch {
	case normDiff:
		return oam.CauseNormalizer
	case exDiff:
		return oam.CauseParser
	}
	return oam.CauseTarget
}

func attrMap(as []Attr) map[string]string {
	m := make(map[string]string, len(as))
	for _, a := range as {
		m[a.Name] = a.Value
	}
	return m
}

func scoreInputs(cs []FeatureChange) []ScoreChange {
	out := make([]ScoreChange, 0, len(cs))
	for _, c := range cs {
		s := ScoreChange{Op: c.Op, Kind: string(c.Feature.Kind), Key: c.Feature.Key, Value: c.Feature.Value, Attrs: attrMap(c.Feature.Attrs)}
		if c.Old != nil {
			s.OldValue, s.OldAttrs = c.Old.Value, attrMap(c.Old.Attrs)
		}
		out = append(out, s)
	}
	return out
}

// Patch returns the content_changed patch of the change set (V2 §3.4),
// validated by oam.
func (cs ChangeSet) Patch(from, to, diffID string) (oam.ContentChangePatch, error) {
	p := oam.ContentChangePatch{SnapshotFrom: from, SnapshotTo: to, DiffID: diffID, Levels: cs.Levels,
		Summary: cs.Summary, Cause: cs.Cause, Baseline: cs.Baseline, Class: cs.Class, Score: cs.Score.Value}
	for _, r := range cs.Score.Reasons {
		p.ScoreFeatures = append(p.ScoreFeatures, oam.ScoreFeature{Name: r.Name, Weight: r.Weight})
	}
	return p.Normalize()
}

// NoiseCounter accumulates the noise metric (V2 §5): false_change_rate is
// the share of compared captures whose text changed with no level-3 change,
// on pages whose features are stable. Masked-only and reordered outcomes
// are counted separately; they are not changes.
type NoiseCounter struct {
	Compared   int `json:"compared"`
	TextOnly   int `json:"text_only"`
	MaskedOnly int `json:"masked_only"`
	Reordered  int `json:"reordered"`
}

// Add counts one change set.
func (n *NoiseCounter) Add(cs ChangeSet) {
	if cs.Outcome == DiffDegraded || cs.Baseline {
		return
	}
	n.Compared++
	switch cs.Outcome {
	case DiffStructural:
		n.TextOnly++
	case DiffMaskedOnly:
		n.MaskedOnly++
	case DiffReordered:
		n.Reordered++
	}
}

// FalseChangeRate is TextOnly / Compared (0 when nothing was compared).
func (n NoiseCounter) FalseChangeRate() float64 {
	if n.Compared == 0 {
		return 0
	}
	return float64(n.TextOnly) / float64(n.Compared)
}

// AuditSample reports whether the masked diff with this id belongs to the
// audit sample (V2 §12.5 "a small random sample of masked diffs is kept
// for audit"): FNV-1a of the id modulo 1000 below perMille. Deterministic,
// so the store and a replay agree on the sample.
func AuditSample(diffID string, perMille int) bool {
	return int(sketch.FNV1a64(diffID)%1000) < perMille
}
