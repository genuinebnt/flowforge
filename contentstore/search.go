package contentstore

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/sketch"
	"github.com/genuinebnt/flowforge/textindex"
)

// Search integration (V2 §7, §12.14). One textindex.Index per (tenant,
// program). Documents are diff hunks (removed and added sides separately,
// field side), semantic change records (one per level-3 change) and,
// optionally, the latest normalized snapshot of each key. Indexing is
// incremental from a watermark (every diff with Seq <= watermark is
// indexed) and always rebuildable from the stored diffs: document ids are
// a pure function of the diff (Seq<<20 | position), and documents are
// written with Update, so re-indexing a diff after a crash between the
// index refresh and the watermark write is idempotent (CSTORE-INV-05).
// The tokenizer is textindex's analyzer, which is §7's: split on
// non-alphanumerics and on camelCase and snake_case boundaries, keep the
// original token, lower-case, no stemming.

// IndexMode selects when diffs are indexed (a pluggable decision,
// docs/ALGORITHMS.md "contentstore: indexing"; benchmark
// BenchmarkCStoreAlgo/index/<mode>).
type IndexMode uint8

const (
	// IndexInline (default) indexes right after each ingest: a capture's
	// changes are searchable when Ingest returns.
	IndexInline IndexMode = iota
	// IndexBatched leaves indexing to IndexPending calls (a background
	// loop): ingest is cheaper and segments are larger, search lags by
	// the loop interval.
	IndexBatched
)

// String returns "inline" or "batched".
func (m IndexMode) String() string {
	if m == IndexBatched {
		return "batched"
	}
	return "inline"
}

// ParseIndexMode is the inverse of String.
func ParseIndexMode(s string) (IndexMode, error) {
	switch s {
	case "", "inline":
		return IndexInline, nil
	case "batched":
		return IndexBatched, nil
	}
	return 0, invalid("unknown index mode %q (inline, batched)", s)
}

// Schema is the search schema: §7's fields plus the §12 classification
// (vendor, class, newness, cause, profile, baseline) as keyword filters,
// the decoded text of embedded blobs as its own text field (searched by
// unqualified queries; decoded_from names the encodings, e.g.
// "base64>gzip"), and stored references back to the diff.
func Schema() textindex.Schema {
	return textindex.Schema{
		Text: []string{"text", "url", "decoded"},
		Keyword: []string{"program", "url_template", "kind", "side", "feature", "mime", "vendor", "class", "newness", "cause", "profile",
			"baseline", "decoded_from"},
		Numeric: []string{"at"},
		Stored:  []string{"diff", "snapshot", "from", "doc", "header", "score"},
	}
}

// Document kinds and sides.
const (
	DocKindHunk     = "hunk"
	DocKindSnapshot = "snapshot"
	SideAdded       = "added"
	SideRemoved     = "removed"
)

// Bounds of indexed documents.
const (
	maxDocText    = 64 << 10 // bytes of text per document
	maxDiffDocs   = 1 << 20  // documents per diff (the id's low bits)
	maxBlobExtra  = 64 << 10 // decoded blob bytes per document
	maxBlobs      = 16       // decoded blobs per document
	firstHitLimit = 10000
)

// Indexes manages the per-(tenant, program) indexes. Safe for concurrent
// use; indexing and tombstoning of one program are serialized.
type Indexes struct {
	Mode IndexMode
	// Dirs returns the directory of one index (nil: memory directories,
	// kept for the process lifetime).
	Dirs func(tenant, program core.UUID) (textindex.Directory, error)
	// IndexLatest also indexes each key's latest normalized snapshot (kind
	// "snapshot"), replaced on every new version.
	IndexLatest bool
	// MergeFactor and MaxRegexCandidates are passed to textindex.Config.
	MergeFactor        int
	MaxRegexCandidates int

	mu   sync.Mutex
	open map[string]*progIndex
	mem  map[string]*textindex.MemDirectory
}

type progIndex struct {
	mu  sync.Mutex
	dir textindex.Directory
	ix  *textindex.Index
}

func (x *Indexes) get(tenant, program core.UUID) (*progIndex, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.open == nil {
		x.open, x.mem = map[string]*progIndex{}, map[string]*textindex.MemDirectory{}
	}
	k := tenant.String() + "/" + program.String()
	if pi, ok := x.open[k]; ok {
		return pi, nil
	}
	var dir textindex.Directory
	if x.Dirs != nil {
		d, err := x.Dirs(tenant, program)
		if err != nil {
			return nil, internal("index directory", err)
		}
		dir = d
	} else {
		md, ok := x.mem[k]
		if !ok {
			md = textindex.NewMemDirectory()
			x.mem[k] = md
		}
		dir = md
	}
	pi := &progIndex{dir: dir}
	if err := pi.reopen(x); err != nil {
		return nil, err
	}
	x.open[k] = pi
	return pi, nil
}

func (pi *progIndex) reopen(x *Indexes) error {
	ix, err := textindex.Open(textindex.Config{Schema: Schema(), Dir: pi.dir, MergeFactor: x.MergeFactor, MaxRegexCandidates: x.MaxRegexCandidates})
	if err != nil {
		return internal("open index", err)
	}
	pi.ix = ix
	return nil
}

// Close closes every open index.
func (x *Indexes) Close() error {
	x.mu.Lock()
	defer x.mu.Unlock()
	var first error
	for k, pi := range x.open {
		if err := pi.ix.Close(); err != nil && first == nil {
			first = err
		}
		delete(x.open, k)
	}
	return first
}

// docID is the id of a diff's i-th document.
func docID(seq int64, i int) uint64 { return uint64(seq)<<20 | uint64(i) }

// latestDocID is the id of a key's latest-snapshot document: the top bit
// set (diff documents never reach it below seq 2^43) and FNV-1a of the key.
func latestDocID(url, profile string) uint64 {
	return 1<<63 | sketch.FNV1a64(url+"\x00"+profile)>>1
}

func boolKW(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// diffDocuments derives a diff's documents: per artifact hunk its removed
// and added sides, then per level-3 change one semantic record. Pure and
// deterministic; the ids are docID(d.Seq, position).
func diffDocuments(d Diff, to Snapshot, hr hunksRecord, sr semanticRecord) []textindex.Document {
	cs := sr.ChangeSet
	common := func(kind, side, feature, mime, url string, vendor bool) []textindex.FieldValue {
		return []textindex.FieldValue{{Name: "program", Value: core.PublicID(core.PrefixProgram, d.Program)},
			{Name: "url_template", Value: to.URLTemplate}, {Name: "kind", Value: kind}, {Name: "side", Value: side},
			{Name: "feature", Value: feature}, {Name: "mime", Value: mime}, {Name: "vendor", Value: boolKW(vendor)},
			{Name: "class", Value: cs.Class}, {Name: "newness", Value: cs.Newness}, {Name: "cause", Value: cs.Cause},
			{Name: "profile", Value: d.Profile}, {Name: "baseline", Value: boolKW(cs.Baseline)}}
	}
	from := ""
	if !d.From.IsZero() {
		from = core.PublicID(PrefixSnapshot, d.From)
	}
	stored := func(doc, header string) []textindex.FieldValue {
		return []textindex.FieldValue{{Name: "diff", Value: d.PublicID()}, {Name: "snapshot", Value: core.PublicID(PrefixSnapshot, d.To)},
			{Name: "from", Value: from}, {Name: "doc", Value: doc}, {Name: "header", Value: header},
			{Name: "score", Value: fmt.Sprintf("%.4f", cs.Score.Value)}}
	}
	var out []textindex.Document
	add := func(text, url string, kw, st []textindex.FieldValue) {
		if len(out) >= maxDiffDocs {
			return
		}
		tf, from := textFields(indexText(text), url)
		out = append(out, textindex.Document{ID: docID(d.Seq, len(out)), Text: tf, Keyword: append(kw, from),
			Numeric: []textindex.NumericValue{{Name: "at", Value: d.At.UnixMilli()}}, Stored: st})
	}
	for _, a := range hr.Artifacts {
		for _, h := range a.Hunks {
			removed, added := hunkSides(h)
			for _, side := range []struct {
				name  string
				lines []string
			}{{SideRemoved, removed}, {SideAdded, added}} {
				if len(side.lines) == 0 {
					continue
				}
				add(strings.Join(side.lines, "\n"), a.DocID, common(DocKindHunk, side.name, "", string(a.Kind), d.URL, a.Vendor), stored(a.DocID, h.Header))
			}
		}
	}
	for _, c := range cs.Changes {
		f := c.Feature
		side := c.Op
		if side == "returned" {
			side = SideAdded
		}
		var b strings.Builder
		b.WriteString(f.Key)
		if f.Value != "" {
			b.WriteString(" " + f.Value)
		}
		for _, at := range f.Attrs {
			if at.Value != "" {
				b.WriteString(" " + at.Name + "=" + at.Value)
			}
		}
		url := d.URL
		if f.Doc != "" {
			url = d.URL + f.Doc
		}
		add(b.String(), url, common(string(f.Kind)+"_"+c.Op, side, string(f.Kind), to.Kind, d.URL, false), stored(f.Doc, ""))
	}
	return out
}

// indexedText is a document's text fields: the (bounded, redacted)
// text, and the decoded text of the blobs content.DecodeBlobs finds in it
// (base64 in either alphabet, hex, gzip, zlib, zstd) (V2 §12.14, content.DecodeBlobs: bounded, never inside a
// secret, redacted) with the encodings it was decoded from.
type indexedText struct {
	text, decoded, from string
}

// indexText bounds a document's text, decodes the blobs in it and redacts
// both (V2 §12.11: nothing credential-shaped is indexed).
//
// Every caller passes text that ingest already redacted (hunk lines and
// normalized texts are redacted, feature keys of secrets are hashes or
// previews), so the cut can only split a placeholder, never expose part
// of a secret; the Redact here is a second pass.
func indexText(s string) indexedText {
	if len(s) > maxDocText {
		s = s[:maxDocText]
		for !utf8.ValidString(s) && len(s) > 0 {
			s = s[:len(s)-1]
		}
	}
	r, _ := content.Redact([]byte(s))
	out := indexedText{text: string(r)}
	var texts, paths []string
	total := 0
	for _, b := range content.DecodeBlobs([]byte(s), content.Limits{MaxResponseBytes: maxBlobExtra, MaxInnerDocuments: maxBlobs}) {
		if b.Text == "" || total >= maxBlobExtra {
			continue
		}
		t := b.Text
		if total+len(t) > maxBlobExtra {
			t = t[:maxBlobExtra-total]
			for !utf8.ValidString(t) && len(t) > 0 {
				t = t[:len(t)-1]
			}
		}
		texts, paths, total = append(texts, t), append(paths, b.Path), total+len(t)
	}
	if len(texts) > 0 {
		d, _ := content.Redact([]byte(strings.Join(texts, "\n")))
		slices.Sort(paths)
		out.decoded, out.from = string(d), strings.Join(slices.Compact(paths), ",")
	}
	return out
}

// textFields are a document's text fields and its decoded_from flag.
func textFields(it indexedText, url string) ([]textindex.FieldValue, textindex.FieldValue) {
	return []textindex.FieldValue{{Name: "text", Value: it.text}, {Name: "url", Value: url}, {Name: "decoded", Value: it.decoded}},
		textindex.FieldValue{Name: "decoded_from", Value: it.from}
}

// IndexReport is what IndexPending did.
type IndexReport struct {
	Diffs     int
	Docs      int
	Watermark int64
}

// IndexPending indexes every diff of the program above the watermark and
// advances it (V2 §7 "incremental from content change events with a
// watermark"). An index found empty while the watermark says otherwise
// (a lost memory directory) is rebuilt from the stored diffs.
func (s *Store) IndexPending(ctx context.Context, tenant, program core.UUID) (IndexReport, error) {
	if s.cfg.Index == nil {
		return IndexReport{}, nil
	}
	pi, err := s.cfg.Index.get(tenant, program)
	if err != nil {
		return IndexReport{}, err
	}
	pi.mu.Lock()
	defer pi.mu.Unlock()
	return s.indexLocked(ctx, tenant, program, pi)
}

func (s *Store) indexLocked(ctx context.Context, tenant, program core.UUID, pi *progIndex) (IndexReport, error) {
	tc, err := s.cfg.Chunks.open(ctx, tenant)
	if err != nil {
		return IndexReport{}, err
	}
	var st IndexState
	if err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		var err error
		st, err = tx.IndexState(ctx, program)
		return err
	}); err != nil {
		return IndexReport{}, internal("index state", err)
	}
	if ixs := pi.ix.Stats(); ixs.Live == 0 && ixs.Pending == 0 && st.Docs > 0 {
		st.Watermark, st.Docs = 0, 0
	}
	oldBytes := st.Bytes
	rep := IndexReport{Watermark: st.Watermark}
	for {
		var ds []Diff
		snaps := map[core.UUID]Snapshot{}
		if err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
			var err error
			if ds, err = tx.DiffsAfter(ctx, program, rep.Watermark, 256); err != nil {
				return err
			}
			for _, d := range ds {
				sn, ok, err := tx.GetSnapshot(ctx, d.To)
				if err != nil {
					return err
				}
				if ok {
					snaps[d.To] = sn
				}
			}
			return nil
		}); err != nil {
			return rep, internal("index read", err)
		}
		if len(ds) == 0 {
			break
		}
		for _, d := range ds {
			to := snaps[d.To]
			hr, sr, err := s.loadDiff(ctx, tc, d, to)
			if err != nil {
				return rep, err
			}
			for _, doc := range diffDocuments(d, to, hr, sr) {
				if err := pi.ix.Update(doc); err != nil {
					return rep, internal("index update", err)
				}
				rep.Docs++
			}
			if s.cfg.Index.IndexLatest && to.Open() {
				nb, err := tc.get(ctx, to.NormalizedManifest)
				if err != nil {
					return rep, err
				}
				if err := pi.ix.Update(latestDocument(to, nb)); err != nil {
					return rep, internal("index update", err)
				}
			}
			rep.Diffs++
			rep.Watermark = d.Seq
		}
	}
	if rep.Diffs == 0 && st.Watermark == rep.Watermark && st.Bytes == int64(pi.ix.Stats().Bytes) {
		return rep, nil
	}
	if err := pi.ix.Refresh(); err != nil {
		return rep, internal("index refresh", err)
	}
	ixs := pi.ix.Stats()
	st.Watermark, st.Docs, st.Bytes = rep.Watermark, int64(ixs.Live), int64(ixs.Bytes)
	return rep, s.saveIndexState(ctx, tenant, st, oldBytes)
}

// saveIndexState writes the watermark and moves the tenant's index-bytes
// usage by the program's change.
func (s *Store) saveIndexState(ctx context.Context, tenant core.UUID, st IndexState, oldBytes int64) error {
	err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		if err := tx.PutIndexState(ctx, st); err != nil {
			return err
		}
		u, err := tx.Usage(ctx)
		if err != nil {
			return err
		}
		u.IndexBytes = max(0, u.IndexBytes+st.Bytes-oldBytes)
		return tx.PutUsage(ctx, u)
	})
	if err != nil {
		return internal("index state", err)
	}
	return nil
}

// latestDocument is a key's latest-snapshot document.
func latestDocument(to Snapshot, normalized []byte) textindex.Document {
	tf, from := textFields(indexText(string(normalized)), to.URL)
	return textindex.Document{ID: latestDocID(to.URL, to.Profile), Text: tf,
		Keyword: []textindex.FieldValue{from, {Name: "program", Value: core.PublicID(core.PrefixProgram, to.Program)},
			{Name: "url_template", Value: to.URLTemplate}, {Name: "kind", Value: DocKindSnapshot}, {Name: "mime", Value: to.Kind},
			{Name: "vendor", Value: boolKW(vendorURL(to.URL))}, {Name: "profile", Value: to.Profile}},
		Numeric: []textindex.NumericValue{{Name: "at", Value: to.FetchedAt.UnixMilli()}},
		Stored:  []textindex.FieldValue{{Name: "snapshot", Value: to.PublicID()}, {Name: "doc", Value: to.URL}}}
}

// Rebuild drops a program's index and re-indexes every stored diff.
func (s *Store) Rebuild(ctx context.Context, tenant, program core.UUID) (IndexReport, error) {
	if s.cfg.Index == nil {
		return IndexReport{}, nil
	}
	pi, err := s.cfg.Index.get(tenant, program)
	if err != nil {
		return IndexReport{}, err
	}
	pi.mu.Lock()
	defer pi.mu.Unlock()
	if err := pi.ix.Close(); err != nil {
		return IndexReport{}, internal("close index", err)
	}
	names, err := pi.dir.List()
	if err != nil {
		return IndexReport{}, internal("list index", err)
	}
	for _, n := range names {
		if err := pi.dir.Remove(n); err != nil {
			return IndexReport{}, internal("remove index file", err)
		}
	}
	if err := pi.reopen(s.cfg.Index); err != nil {
		return IndexReport{}, err
	}
	var old IndexState
	if err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		var err error
		old, err = tx.IndexState(ctx, program)
		return err
	}); err != nil {
		return IndexReport{}, internal("index state", err)
	}
	if err := s.saveIndexState(ctx, tenant, IndexState{Program: program}, old.Bytes); err != nil {
		return IndexReport{}, err
	}
	return s.indexLocked(ctx, tenant, program, pi)
}

// tombstone removes the documents of deleted diffs (retention).
func (s *Store) tombstone(ctx context.Context, tenant, program core.UUID, ds []Diff) error {
	if s.cfg.Index == nil || len(ds) == 0 {
		return nil
	}
	pi, err := s.cfg.Index.get(tenant, program)
	if err != nil {
		return err
	}
	pi.mu.Lock()
	defer pi.mu.Unlock()
	var st IndexState
	if err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		var err error
		st, err = tx.IndexState(ctx, program)
		return err
	}); err != nil {
		return internal("index state", err)
	}
	n := 0
	for _, d := range ds {
		if d.Seq > st.Watermark {
			continue // not indexed yet, and its rows are gone
		}
		for i := 0; i < d.IndexDocs; i++ {
			if err := pi.ix.Delete(docID(d.Seq, i)); err != nil {
				return internal("index delete", err)
			}
			n++
		}
	}
	if n == 0 {
		return nil
	}
	if err := pi.ix.Refresh(); err != nil {
		return internal("index refresh", err)
	}
	old := st.Bytes
	ixs := pi.ix.Stats()
	st.Docs, st.Bytes = int64(ixs.Live), int64(ixs.Bytes)
	return s.saveIndexState(ctx, tenant, st, old)
}

// SearchOptions are per-query parameters.
type SearchOptions struct {
	K int
	// IncludeVendor lifts the default exclusion of vendored bundles and
	// library modules (V2 §12.14). A query that names the vendor field
	// itself is left as written.
	IncludeVendor bool
}

// ChangeHit is one search hit.
type ChangeHit struct {
	DocID       uint64
	Score       float64
	At          time.Time
	DiffID      string
	SnapshotID  string
	FromID      string
	URL         string
	Doc         string
	Header      string
	Kind        string
	Side        string
	Feature     string
	Mime        string
	URLTemplate string
	Class       string
	Newness     string
	Cause       string
	Vendor      bool
	// DecodedFrom names the encodings of blobs decoded into the
	// document's "decoded" field ("" when none).
	DecodedFrom string
	// Text is the matched document's text (redacted), cut at 1 KiB.
	Text string
}

// SearchResult is a ranked page of hits.
type SearchResult struct {
	Hits  []ChangeHit
	Total int
}

// queryError maps a textindex query error to VALIDATION_FAILED with its
// stable code and span (textindex README "Errors").
func queryError(err error) error {
	var qe *textindex.QueryError
	if errors.As(err, &qe) {
		return &core.Error{Kind: core.KindInvalidArgument, Reason: core.ReasonValidationFailed, Message: qe.Msg,
			Meta: map[string]string{"code": string(qe.Code), "span": fmt.Sprintf("%d-%d", qe.Span.Start, qe.Span.End)}, Err: err}
	}
	return internal("search", err)
}

// withoutVendor adds "-vendor:1" as a last conjunct of q (in place of a
// query string rewrite, so error spans keep pointing into the caller's
// text). The arena stays in pre-order: the new nodes are appended after
// the root's subtree.
func withoutVendor(q *textindex.Query) *textindex.Query {
	for _, n := range q.Nodes {
		if n.Field == "vendor" {
			return q
		}
	}
	end := textindex.Span{Start: len(q.Text), End: len(q.Text)}
	out := &textindex.Query{Text: q.Text}
	root := q.Root
	if q.Nodes[root].Kind == textindex.NodeAnd {
		out.Nodes, out.Root = slices.Clone(q.Nodes), root
		last := q.Nodes[root].Child
		for out.Nodes[last].Next >= 0 {
			last = out.Nodes[last].Next
		}
		not := textindex.NodeID(len(out.Nodes))
		out.Nodes[last].Next = not
		out.Nodes = append(out.Nodes, textindex.Node{Kind: textindex.NodeNot, Span: end, Child: not + 1, Next: -1},
			textindex.Node{Kind: textindex.NodeTerm, Span: end, Field: "vendor", Value: "1", Child: -1, Next: -1})
		return out
	}
	// New And root at 0; the old nodes shift by one.
	out.Nodes = append(out.Nodes, textindex.Node{Kind: textindex.NodeAnd, Span: textindex.Span{Start: 0, End: len(q.Text)}, Child: 1, Next: -1})
	shift := func(id textindex.NodeID) textindex.NodeID {
		if id < 0 {
			return id
		}
		return id + 1
	}
	for _, n := range q.Nodes {
		n.Child, n.Next = shift(n.Child), shift(n.Next)
		out.Nodes = append(out.Nodes, n)
	}
	not := textindex.NodeID(len(out.Nodes))
	out.Nodes[shift(root)].Next = not
	out.Nodes = append(out.Nodes, textindex.Node{Kind: textindex.NodeNot, Span: end, Child: not + 1, Next: -1},
		textindex.Node{Kind: textindex.NodeTerm, Span: end, Field: "vendor", Value: "1", Child: -1, Next: -1})
	return out
}

// Search runs a query over a program's change history (V2 §7 query
// language). Vendored code is excluded unless asked for (V2 §12.14).
func (s *Store) Search(ctx context.Context, tenant, program core.UUID, query string, opts SearchOptions) (SearchResult, error) {
	q, err := textindex.Parse(query)
	if err != nil {
		return SearchResult{}, queryError(err)
	}
	if !opts.IncludeVendor {
		q = withoutVendor(q)
	}
	if s.cfg.Index == nil {
		return SearchResult{}, &core.Error{Kind: core.KindFailedPrecondition, Reason: core.ReasonUnavailable, Message: "search is not configured"}
	}
	pi, err := s.cfg.Index.get(tenant, program)
	if err != nil {
		return SearchResult{}, err
	}
	k := opts.K
	if k <= 0 {
		k = 20
	}
	res, err := pi.ix.SearchQuery(q, textindex.SearchOptions{K: min(k, 1000), Now: s.cfg.Clock.Now().UnixMilli()})
	if err != nil {
		return SearchResult{}, queryError(err)
	}
	out := SearchResult{Total: res.Total}
	for _, h := range res.Hits {
		hit, err := hitOf(pi.ix, h)
		if err != nil {
			return SearchResult{}, err
		}
		out.Hits = append(out.Hits, hit)
	}
	_ = ctx
	return out, nil
}

func hitOf(ix *textindex.Index, h textindex.Hit) (ChangeHit, error) {
	d, ok, err := ix.Get(h.ID)
	if err != nil {
		return ChangeHit{}, internal("index get", err)
	}
	hit := ChangeHit{DocID: h.ID, Score: h.Score}
	if h.HasAt {
		hit.At = time.UnixMilli(h.At).UTC()
	}
	if !ok {
		return hit, nil
	}
	val := func(fs []textindex.FieldValue, name string) string {
		for _, f := range fs {
			if f.Name == name {
				return f.Value
			}
		}
		return ""
	}
	hit.Text = val(d.Text, "text")
	if len(hit.Text) > 1024 {
		hit.Text = hit.Text[:1024]
		for !utf8.ValidString(hit.Text) {
			hit.Text = hit.Text[:len(hit.Text)-1]
		}
	}
	hit.URL = val(d.Text, "url")
	hit.Kind, hit.Side, hit.Feature, hit.Mime = val(d.Keyword, "kind"), val(d.Keyword, "side"), val(d.Keyword, "feature"), val(d.Keyword, "mime")
	hit.URLTemplate, hit.Class, hit.Newness, hit.Cause = val(d.Keyword, "url_template"), val(d.Keyword, "class"), val(d.Keyword, "newness"), val(d.Keyword, "cause")
	hit.Vendor = val(d.Keyword, "vendor") == "1"
	hit.DecodedFrom = val(d.Keyword, "decoded_from")
	hit.DiffID, hit.SnapshotID, hit.FromID = val(d.Stored, "diff"), val(d.Stored, "snapshot"), val(d.Stored, "from")
	hit.Doc, hit.Header = val(d.Stored, "doc"), val(d.Stored, "header")
	return hit, nil
}

// FirstAppeared returns the earliest snapshot whose diff added text
// containing s (case-sensitive), served from the diff history (V2 §12.14:
// baselines are diffs against the empty document, so content present
// from the first capture is found too). found is false when no stored
// diff adds it. Candidates come from a phrase query (or a regex for text
// without word tokens) on added sides, at most 10,000, verified exactly.
func (s *Store) FirstAppeared(ctx context.Context, tenant, program core.UUID, text string, opts SearchOptions) (ChangeHit, bool, error) {
	if strings.TrimSpace(text) == "" {
		return ChangeHit{}, false, invalid("text is required")
	}
	if len(text) > 1024 {
		return ChangeHit{}, false, invalid("text longer than 1024 bytes")
	}
	var q string
	if len(textindex.Analyze([]byte(text))) > 0 {
		q = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(text) + `" side:added`
	} else {
		q = "/" + strings.ReplaceAll(regexp.QuoteMeta(text), "/", `\/`) + "/ side:added"
	}
	if s.cfg.Index == nil {
		return ChangeHit{}, false, &core.Error{Kind: core.KindFailedPrecondition, Reason: core.ReasonUnavailable, Message: "search is not configured"}
	}
	parsed, err := textindex.Parse(q)
	if err != nil {
		return ChangeHit{}, false, queryError(err)
	}
	if !opts.IncludeVendor {
		parsed = withoutVendor(parsed)
	}
	pi, err := s.cfg.Index.get(tenant, program)
	if err != nil {
		return ChangeHit{}, false, err
	}
	res, err := pi.ix.SearchQuery(parsed, textindex.SearchOptions{K: firstHitLimit, Now: s.cfg.Clock.Now().UnixMilli()})
	if err != nil {
		return ChangeHit{}, false, queryError(err)
	}
	hits := slices.Clone(res.Hits)
	slices.SortFunc(hits, func(a, b textindex.Hit) int { return cmp.Or(cmp.Compare(a.At, b.At), cmp.Compare(a.ID, b.ID)) })
	for _, h := range hits {
		hit, err := hitOf(pi.ix, h)
		if err != nil {
			return ChangeHit{}, false, err
		}
		d, _, err := pi.ix.Get(h.ID)
		if err != nil {
			return ChangeHit{}, false, internal("index get", err)
		}
		for _, f := range d.Text {
			if f.Name == "text" && strings.Contains(f.Value, text) {
				return hit, true, nil
			}
		}
	}
	_ = ctx
	return ChangeHit{}, false, nil
}
