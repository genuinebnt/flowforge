package toolrunner

import (
	"context"
	"encoding/json"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/services/internal/convert"
)

// The read-only content tools (V2_DESIGN §8): they read the content
// service and touch no target (effect class read).

func readSpec(name, desc, in, outType string) Spec {
	return Spec{Name: name, Version: "1.0.0", Description: desc, InputType: in, OutputType: outType, Class: core.ClassNone,
		Timeout: time.Minute, Idempotent: true, Idempotency: engine.IdempotencyIdempotent,
		Retry: RetryContent}
}

// Extract is the read tool extract: `(Snapshot) -> Features`, the
// level-3 features of a stored snapshot (GetSnapshot with features), by
// kind. For a derived snapshot (an original source) only the features
// found in that document are returned. The graph already holds these
// facts: the content service published the capture's observation, with
// authority, when it was stored.
type Extract struct{ c *contentTools }

// Spec implements Adapter.
func (*Extract) Spec() Spec {
	s := readSpec("extract", "Level-3 features of a stored snapshot", "Snapshot", "Features")
	s.Observes = []string{oam.TypeEndpoint, oam.TypeFile, oam.TypeProductRelease}
	return s
}

// featuresRecord is the prelude Features record.
type featuresRecord struct {
	SnapshotID string   `json:"snapshot_id"`
	URL        string   `json:"url"`
	Coverage   string   `json:"coverage"`
	Links      []string `json:"links"`
	Scripts    []string `json:"scripts"`
	Styles     []string `json:"styles"`
	Frames     []string `json:"frames"`
	Sourcemaps []string `json:"sourcemaps"`
	Endpoints  []string `json:"endpoints"`
	Forms      []string `json:"forms"`
	Parameters []string `json:"parameters"`
	Headers    []string `json:"headers"`
	Libraries  []string `json:"libraries"`
	Secrets    []string `json:"secrets"`
}

// Run implements Adapter.
func (a *Extract) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	id, suffix, err := decodeSnapshotRef(input)
	if err != nil {
		return nil, err
	}
	s, err := a.c.getSnapshot(ctx, tc, id, true)
	if err != nil {
		return nil, err
	}
	fs, err := features(s)
	if err != nil {
		return nil, err
	}
	rec := featuresOf(s, fs, suffix)
	_ = out.Progress(1, 1, "")
	return json.Marshal(rec)
}

// featuresOf groups features by kind. URL-valued kinds keep only URLs
// that parse as canonical http(s) URLs (the DSL Url type).
func featuresOf(s *pb.ContentSnapshot, fs []content.Feature, suffix string) featuresRecord {
	r := featuresRecord{SnapshotID: s.GetId() + suffix, URL: s.GetUrl() + suffix, Coverage: s.GetCoverage(),
		Links: []string{}, Scripts: []string{}, Styles: []string{}, Frames: []string{}, Sourcemaps: []string{},
		Endpoints: []string{}, Forms: []string{}, Parameters: []string{}, Headers: []string{}, Libraries: []string{}, Secrets: []string{}}
	addURL := func(dst *[]string, f content.Feature) {
		if u, ok := featureURL(f); ok {
			*dst = appendUnique(*dst, u)
		}
	}
	for _, f := range fs {
		if suffix != "" && f.Doc != suffix {
			continue
		}
		switch f.Kind {
		case content.FeatLink, content.FeatSitemapEntry:
			addURL(&r.Links, f)
		case content.FeatScript:
			addURL(&r.Scripts, f)
		case content.FeatStyle, content.FeatCSSImport:
			addURL(&r.Styles, f)
		case content.FeatFrame:
			addURL(&r.Frames, f)
		case content.FeatSourceMap:
			addURL(&r.Sourcemaps, f)
		case content.FeatEndpoint, content.FeatSchemaEndpoint:
			r.Endpoints = appendUnique(r.Endpoints, f.Key)
		case content.FeatForm:
			r.Forms = appendUnique(r.Forms, f.Key)
		case content.FeatParameter, content.FeatGraphQLOperation:
			r.Parameters = appendUnique(r.Parameters, f.Key)
		case content.FeatHeader:
			r.Headers = appendUnique(r.Headers, f.Key)
		case content.FeatLibrary:
			r.Libraries = appendUnique(r.Libraries, f.Key+"@"+f.Value)
		case content.FeatGenerator:
			r.Libraries = appendUnique(r.Libraries, f.Value)
		case content.FeatSecret:
			// The preview (first 4 and last 2 characters, V2 §3.3), never
			// the secret: the content service redacts before it stores.
			r.Secrets = appendUnique(r.Secrets, f.Attr("rule")+":"+f.Value)
		case content.FeatSourceDisclosure:
			r.Secrets = appendUnique(r.Secrets, "source_disclosure:"+f.Key)
		}
	}
	return r
}

// UnpackSourcemap is the read tool unpack_sourcemap: `(Snapshot) ->
// List<Snapshot>`. For a source map snapshot, or a script whose
// sourceMappingURL names a stored source map, it returns one derived
// snapshot per original source embedded in the map (sourcesContent): the
// "#src/<path>" documents the content service normalized, extracted and
// diffs with the map (V2 §12.6). A derived snapshot's id and URL are the
// map's with the "#src/<path>" suffix; extract and diff_latest accept
// it. The map must have been collected (collect captures the maps of a
// page's scripts); otherwise the list is empty.
type UnpackSourcemap struct{ c *contentTools }

// Spec implements Adapter.
func (*UnpackSourcemap) Spec() Spec {
	return readSpec("unpack_sourcemap", "Original sources of a stored source map as derived snapshots", "Snapshot", "List<Snapshot>")
}

// Run implements Adapter.
func (a *UnpackSourcemap) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	id, suffix, err := decodeSnapshotRef(input)
	if err != nil {
		return nil, err
	}
	res := []snapshotRecord{}
	if suffix != "" {
		return json.Marshal(res) // an original source has no source map of its own
	}
	s, err := a.c.getSnapshot(ctx, tc, id, true)
	if err != nil {
		return nil, err
	}
	m, err := a.mapOf(ctx, tc, s)
	if err != nil || m == nil {
		if err != nil {
			return nil, err
		}
		return json.Marshal(res)
	}
	fs, err := features(m)
	if err != nil {
		return nil, err
	}
	base := snapshotOf(m, tc, []string{})
	for _, doc := range originalDocs(fs) {
		d := base
		d.ID, d.URL, d.Kind = m.GetId()+doc, m.GetUrl()+doc, string(originalKind(doc))
		// A derived document has no response of its own: no media type,
		// body hash or size, and no predecessor pointer (its history is
		// the map's).
		d.Mime, d.BodySHA256, d.RawSize, d.ParentID, d.LineageParentID = "", "", 0, nil, nil
		d.Redirects, d.Resources = []string{}, []snapshotRecord{}
		res = append(res, d)
	}
	_ = out.Progress(1, 1, "")
	return json.Marshal(res)
}

// mapOf returns the source map snapshot of s: s itself, or the stored
// open version of the map a script names (same profile).
func (a *UnpackSourcemap) mapOf(ctx context.Context, tc *ToolCtx, s *pb.ContentSnapshot) (*pb.ContentSnapshot, error) {
	switch content.Kind(s.GetKind()) {
	case content.KindSourceMap:
		return s, nil
	case content.KindJS:
	default:
		return nil, nil
	}
	fs, err := features(s)
	if err != nil {
		return nil, err
	}
	for _, f := range fs {
		if f.Kind != content.FeatSourceMap {
			continue
		}
		u, ok := featureURL(f)
		if !ok {
			continue
		}
		ls, err := a.c.cfg.Content.ListSnapshots(contentCtx(ctx, tc), &pb.ListSnapshotsRequest{ProgramId: convert.ID(core.PrefixProgram, tc.Program),
			Url: u, Profile: s.GetProfile(), OpenOnly: true, Page: &pb.PageRequest{PageSize: 1}})
		if err != nil {
			return nil, contentError("list snapshots "+u, err)
		}
		if len(ls.GetSnapshots()) == 0 {
			return nil, nil
		}
		return a.c.getSnapshot(ctx, tc, ls.GetSnapshots()[0].GetId(), true)
	}
	return nil, nil
}

// originalDocs lists the "#src/<path>" documents of a source map's
// features, sorted: the documents features were found in, and every
// source with embedded content. The path is the source URL without
// "webpack://" and leading slashes, as content names them (content
// sourceMapOriginals).
func originalDocs(fs []content.Feature) []string {
	var docs []string
	for _, f := range fs {
		if strings.HasPrefix(f.Doc, "#src/") {
			docs = appendUnique(docs, f.Doc)
		}
		if f.Kind == content.FeatSourceMapSource && f.Attr("content") == "true" {
			docs = appendUnique(docs, "#src/"+strings.TrimLeft(strings.ReplaceAll(f.Key, "webpack://", ""), "/"))
		}
	}
	slices.Sort(docs)
	return docs
}

// originalKind is the kind content gives an original source (decision
// D2: TypeScript, TSX and JSX are text).
func originalKind(doc string) content.Kind {
	switch strings.ToLower(path.Ext(doc)) {
	case ".ts", ".tsx", ".jsx", ".mts", ".cts":
		return content.KindTSText
	case ".js", ".mjs", ".cjs":
		return content.KindJS
	case ".css":
		return content.KindCSS
	}
	return content.KindText
}

// DiffLatest is the read tool diff_latest: `(Snapshot) -> ChangeSet`, the
// stored change of a snapshot against its predecessor (DiffSnapshots
// with from empty: the parent, a renamed bundle's lineage parent, or the
// empty document for a baseline). For a derived snapshot it is that
// document's change within the map's. The level-3 changes became graph
// changes and the content_changed event when the snapshot was stored.
type DiffLatest struct{ c *contentTools }

// Spec implements Adapter.
func (*DiffLatest) Spec() Spec {
	return readSpec("diff_latest", "Change of a snapshot against its stored predecessor", "Snapshot", "ChangeSet")
}

// changeSetRecord is the prelude ChangeSet record.
type changeSetRecord struct {
	SnapshotID     string         `json:"snapshot_id"`
	FromSnapshotID *string        `json:"from_snapshot_id"`
	DiffID         *string        `json:"diff_id"`
	URL            string         `json:"url"`
	Outcome        string         `json:"outcome"`
	Levels         []string       `json:"levels"`
	Semantic       bool           `json:"semantic"`
	Class          string         `json:"class"`
	Newness        string         `json:"newness"`
	Cause          string         `json:"cause"`
	Baseline       bool           `json:"baseline"`
	Alertable      bool           `json:"alertable"`
	Score          float64        `json:"score"`
	Changes        []string       `json:"changes"`
	Summary        map[string]int `json:"summary"`
}

// Run implements Adapter.
func (a *DiffLatest) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	id, suffix, err := decodeSnapshotRef(input)
	if err != nil {
		return nil, err
	}
	s, err := a.c.getSnapshot(ctx, tc, id, false)
	if err != nil {
		return nil, err
	}
	d, err := a.c.cfg.Content.DiffSnapshots(contentCtx(ctx, tc), &pb.DiffSnapshotsRequest{ToSnapshotId: id, Format: pb.DiffFormat_DIFF_FORMAT_UNIFIED})
	if err != nil {
		return nil, contentError("diff "+id, err)
	}
	rec := changeSetOf(s, d, suffix)
	_ = out.Progress(1, 1, "")
	return json.Marshal(rec)
}

// changeSetOf converts a diff view; with a suffix, of that document only.
func changeSetOf(s *pb.ContentSnapshot, d *pb.SnapshotDiff, suffix string) changeSetRecord {
	cs := d.GetChange()
	r := changeSetRecord{SnapshotID: s.GetId() + suffix, DiffID: optional(d.GetDiffId()), URL: s.GetUrl() + suffix,
		Outcome: cs.GetOutcome(), Levels: append([]string{}, cs.GetLevels()...), Class: cs.GetClass(), Newness: cs.GetNewness(),
		Cause: cs.GetCause(), Baseline: cs.GetBaseline(), Alertable: cs.GetAlertable(), Score: cs.GetScore(), Summary: map[string]int{}}
	if f := d.GetFromSnapshotId(); f != "" {
		r.FromSnapshotID = optional(f + suffix)
	}
	changes := cs.GetChanges()
	if suffix == "" {
		for k, v := range cs.GetSummary() {
			r.Summary[k] = int(v)
		}
	} else {
		r.Outcome, r.Levels, changes = content.DiffNoChange, []string{}, nil
		for _, av := range d.GetArtifacts() {
			if av.GetDocId() == s.GetUrl()+suffix {
				r.Outcome, r.Levels, changes = av.GetOutcome(), append([]string{}, av.GetLevels()...), av.GetFeatureChanges()
			}
		}
		// The document's own outcome decides; the map-level flags say
		// whether the map's change is shown at all.
		r.Alertable = r.Alertable && (r.Outcome == content.DiffChanged || r.Outcome == content.DiffStructural)
	}
	r.Semantic = slices.Contains(r.Levels, oam.LevelSemantic)
	r.Changes = []string{}
	for _, c := range changes {
		r.Changes = append(r.Changes, c.GetOp()+" "+c.GetKind()+"|"+c.GetKey())
		r.Summary[c.GetKind()+"_"+c.GetOp()]++
	}
	return r
}

// SearchChanges is the read tool search_changes: `(String, Int) ->
// List<ChangeHit>`, the V2 §7 query language over the program's change
// history (SearchChanges; vendored bundles excluded unless the query
// names vendor), at most limit hits (1 to 1000).
type SearchChanges struct{ c *contentTools }

// Spec implements Adapter.
func (*SearchChanges) Spec() Spec {
	return readSpec("search_changes", "Search the program's change history", "(String, Int)", "List<ChangeHit>")
}

// changeHitRecord is the prelude ChangeHit record.
type changeHitRecord struct {
	Score      float64 `json:"score"`
	At         string  `json:"at"`
	DiffID     string  `json:"diff_id"`
	SnapshotID string  `json:"snapshot_id"`
	URL        string  `json:"url"`
	Kind       string  `json:"kind"`
	Side       string  `json:"side"`
	Feature    string  `json:"feature"`
	Class      string  `json:"class"`
	Newness    string  `json:"newness"`
	Cause      string  `json:"cause"`
	Text       string  `json:"text"`
}

// Run implements Adapter.
func (a *SearchChanges) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	args, err := decodeArgs(input, 2)
	if err != nil {
		return nil, err
	}
	q, err := decodeString(args[0])
	if err != nil {
		return nil, err
	}
	var limit int
	if err := json.Unmarshal(args[1], &limit); err != nil || limit < 1 || limit > 1000 {
		return nil, Errorf("invalid_input", false, "the limit must be an integer from 1 to 1000")
	}
	if a.c.cfg.Content == nil {
		return nil, errNoContent
	}
	res, err := a.c.cfg.Content.SearchChanges(contentCtx(ctx, tc), &pb.SearchChangesRequest{ProgramId: convert.ID(core.PrefixProgram, tc.Program),
		Query: q, Limit: int32(limit)})
	if err != nil {
		return nil, contentError("search", err)
	}
	hits := []changeHitRecord{}
	for _, h := range res.GetHits() {
		hits = append(hits, changeHitRecord{Score: h.GetScore(), At: core.FormatTime(convert.Time(h.GetAt())), DiffID: h.GetDiffId(),
			SnapshotID: h.GetSnapshotId(), URL: h.GetUrl(), Kind: h.GetKind(), Side: h.GetSide(), Feature: h.GetFeature(),
			Class: h.GetClass(), Newness: h.GetNewness(), Cause: h.GetCause(), Text: h.GetText()})
	}
	_ = out.Progress(1, 1, "")
	return json.Marshal(hits)
}
