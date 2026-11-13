package contentstore

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/genuinebnt/flowforge/chunkstore"
	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
)

// ChunkStores opens one chunkstore.Store per tenant over a shared
// backend, with the tenant UUID as the namespace: dedupe, presence,
// stats and GC never cross tenants (V2 §12.12 "no cross-tenant dedupe";
// CSTORE-INV-02). Safe for concurrent use.
//
// Each tenant's store has a reader/writer gate: ingests hold it shared
// from their first Put until their rows commit, GC holds it exclusively,
// so a GC never computes its live set between a Put and the commit that
// references it (chunkstore's grace covers one GC, the gate covers any
// number). The gate is per process: one content-service writer per
// tenant (chunkstore's single-writer rule).
type ChunkStores struct {
	Backend chunkstore.Backend
	Config  chunkstore.Config

	mu     sync.Mutex
	stores map[core.UUID]*tenantChunks
}

type tenantChunks struct {
	gate sync.RWMutex
	s    *chunkstore.Store
}

// NewChunkStores returns stores over b.
func NewChunkStores(b chunkstore.Backend, cfg chunkstore.Config) *ChunkStores {
	return &ChunkStores{Backend: b, Config: cfg, stores: map[core.UUID]*tenantChunks{}}
}

func (c *ChunkStores) open(ctx context.Context, tenant core.UUID) (*tenantChunks, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stores == nil {
		c.stores = map[core.UUID]*tenantChunks{}
	}
	if t, ok := c.stores[tenant]; ok {
		return t, nil
	}
	s, err := chunkstore.Open(ctx, c.Backend, tenant.String(), c.Config)
	if err != nil {
		return nil, internal("open chunk store", err)
	}
	t := &tenantChunks{s: s}
	c.stores[tenant] = t
	return t, nil
}

// Metrics returns the tenant store's metrics (CSTORE-INV-08 measures
// storage with them).
func (c *ChunkStores) Metrics(ctx context.Context, tenant core.UUID) (chunkstore.Metrics, error) {
	t, err := c.open(ctx, tenant)
	if err != nil {
		return chunkstore.Metrics{}, err
	}
	return t.s.Metrics(), nil
}

// Close closes every open store.
func (c *ChunkStores) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var first error
	for id, t := range c.stores {
		if err := t.s.Close(ctx); err != nil && first == nil {
			first = err
		}
		delete(c.stores, id)
	}
	return first
}

// put stores data and returns its manifest id (hex) and the record bytes
// it added.
func (t *tenantChunks) put(ctx context.Context, data []byte) (string, int64, error) {
	id, st, err := t.s.Put(ctx, bytes.NewReader(data))
	if err != nil {
		return "", 0, internal("chunk put", err)
	}
	return id.String(), int64(st.Stored), nil
}

// get reads a blob back (every chunk verified by chunkstore).
func (t *tenantChunks) get(ctx context.Context, ref string) ([]byte, error) {
	id, err := chunkstore.ParseManifestID(ref)
	if err != nil {
		return nil, integrity("manifest id %q: %v", ref, err)
	}
	rc, err := t.s.Get(ctx, id)
	if err != nil {
		return nil, internal("chunk get", err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, internal("chunk read", err)
	}
	return b, nil
}

// sealSpan is one redacted span: where it is (the body, or a header
// value of the capture record), its byte range in the original and the
// original bytes.
type sealSpan struct {
	Where  string `json:"where"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Rule   string `json:"rule"`
	Secret []byte `json:"secret"`
}

// redact replaces credential-shaped strings (content.Redact, V2 §12.11)
// and returns the spans to restore them, tagged with where.
func redact(where string, text []byte) ([]byte, []sealSpan) {
	out, fs := content.Redact(text)
	var spans []sealSpan
	for _, f := range fs {
		spans = append(spans, sealSpan{Where: where, Start: f.Start, End: f.End, Rule: f.RuleID, Secret: bytes.Clone(f.Secret)})
	}
	return out, spans
}

// restore reverses redact: the spans of one place, sorted by Start, are
// spliced back. The redacted text at each computed position must be the
// span's placeholder, or the record is corrupt.
func restore(redacted []byte, spans []sealSpan) ([]byte, error) {
	var b bytes.Buffer
	b.Grow(len(redacted) + 64*len(spans))
	pos := 0  // read position in redacted
	orig := 0 // write position in the original
	for _, sp := range spans {
		ph := content.RedactionPlaceholder(sp.Rule, sp.Secret)
		at := pos + (sp.Start - orig)
		if sp.Start < orig || at+len(ph) > len(redacted) || !bytes.Equal(redacted[at:at+len(ph)], []byte(ph)) {
			return nil, integrity("redaction seal does not match the stored text")
		}
		b.Write(redacted[pos:at])
		b.Write(sp.Secret)
		pos, orig = at+len(ph), sp.End
	}
	b.Write(redacted[pos:])
	return b.Bytes(), nil
}

// captureRecord is the stored capture metadata (headers_ref): everything
// of a content.Capture but the body, header values redacted.
type captureRecord struct {
	V             int              `json:"v"`
	URL           string           `json:"url"`
	Profile       content.Profile  `json:"profile"`
	Status        int              `json:"status"`
	Headers       []content.Header `json:"headers,omitempty"`
	FetchedAt     string           `json:"fetched_at"`
	RedirectChain []content.Hop    `json:"redirect_chain,omitempty"`
	Truncated     bool             `json:"truncated,omitempty"`
	Outcome       content.Outcome  `json:"outcome"`
}

// newCaptureRecord builds the record with every header value redacted;
// the spans are addressed "h/<i>" and "r/<hop>/<i>".
func newCaptureRecord(c *content.Capture) (captureRecord, []sealSpan) {
	r := captureRecord{V: 1, URL: c.URL, Profile: c.Profile, Status: c.Status, FetchedAt: core.FormatTime(c.FetchedAt),
		Truncated: c.Truncated, Outcome: c.Outcome}
	var spans []sealSpan
	hs := func(prefix string, in []content.Header) []content.Header {
		out := slices.Clone(in)
		for i := range out {
			v, sp := redact(prefix+strconv.Itoa(i), []byte(out[i].Value))
			out[i].Value = string(v)
			spans = append(spans, sp...)
		}
		return out
	}
	r.Headers = hs("h/", c.Headers)
	for j, h := range c.RedirectChain {
		h.Headers = hs("r/"+strconv.Itoa(j)+"/", h.Headers)
		r.RedirectChain = append(r.RedirectChain, h)
	}
	return r, spans
}

// capture rebuilds the capture (without body) from the record, restoring
// redacted header values from spans when given.
func (r captureRecord) capture(spans []sealSpan) (*content.Capture, error) {
	at, err := core.ParseTime(r.FetchedAt)
	if err != nil {
		return nil, integrity("capture record time: %v", err)
	}
	c := &content.Capture{URL: r.URL, Profile: r.Profile, Status: r.Status, FetchedAt: at, Truncated: r.Truncated, Outcome: r.Outcome}
	by := map[string][]sealSpan{}
	for _, s := range spans {
		by[s.Where] = append(by[s.Where], s)
	}
	fix := func(prefix string, in []content.Header) ([]content.Header, error) {
		out := slices.Clone(in)
		for i := range out {
			if sp := by[prefix+strconv.Itoa(i)]; len(sp) > 0 {
				v, err := restore([]byte(out[i].Value), sp)
				if err != nil {
					return nil, err
				}
				out[i].Value = string(v)
			}
		}
		return out, nil
	}
	if c.Headers, err = fix("h/", r.Headers); err != nil {
		return nil, err
	}
	for j, h := range r.RedirectChain {
		if h.Headers, err = fix("r/"+strconv.Itoa(j)+"/", h.Headers); err != nil {
			return nil, err
		}
		c.RedirectChain = append(c.RedirectChain, h)
	}
	return c, nil
}

// featuresRecord is the stored processed capture (features_ref): the
// content.Processed with every normalized text moved out (the main
// artifact's into normalized_manifest, the others into Texts) and the raw
// bodies of derived documents dropped, plus the feature-id history for
// "returned" changes.
type featuresRecord struct {
	V         int                `json:"v"`
	Processed *content.Processed `json:"processed"`
	Texts     []textEntry        `json:"texts,omitempty"`
	// History holds the feature ids seen in versions before this one
	// (sorted, at most maxHistory).
	History []string `json:"history,omitempty"`
}

// textEntry is the normalized text of artifact Index: Text when valid
// UTF-8 (compresses well), else B64.
type textEntry struct {
	Index int    `json:"i"`
	Text  string `json:"t,omitempty"`
	B64   []byte `json:"b,omitempty"`
}

// maxHistory bounds featuresRecord.History; ids past it (sorted order)
// are forgotten, so their return reads as "added".
const maxHistory = 20000

// encodeFeatures splits p into the record and the main normalized text.
// p is not modified.
func encodeFeatures(p *content.Processed, history []string) ([]byte, []byte, error) {
	q := *p
	q.Artifacts = slices.Clone(p.Artifacts)
	var mainText []byte
	var texts []textEntry
	for i := range q.Artifacts {
		a := &q.Artifacts[i]
		n := a.Normalized
		n.Derived = nil
		if i == 0 {
			mainText = n.Text
		} else if len(n.Text) > 0 {
			if utf8.Valid(n.Text) {
				texts = append(texts, textEntry{Index: i, Text: string(n.Text)})
			} else {
				texts = append(texts, textEntry{Index: i, B64: n.Text})
			}
		}
		n.Text = nil
		a.Normalized = n
		fs := a.Features
		fs.Findings = slices.Clone(fs.Findings)
		for j := range fs.Findings {
			fs.Findings[j].Secret = nil
		}
		a.Features = fs
	}
	b, err := core.CanonicalJSON(featuresRecord{V: 1, Processed: &q, Texts: texts, History: history})
	if err != nil {
		return nil, nil, internal("encode features", err)
	}
	if mainText == nil {
		mainText = []byte{}
	}
	return b, mainText, nil
}

// decodeFeatures rebuilds the processed capture.
func decodeFeatures(b, mainText []byte) (*content.Processed, []string, error) {
	var r featuresRecord
	if err := json.Unmarshal(b, &r); err != nil || r.Processed == nil || len(r.Processed.Artifacts) == 0 {
		return nil, nil, integrity("features record: %v", err)
	}
	p := r.Processed
	p.Artifacts[0].Normalized.Text = mainText
	for _, t := range r.Texts {
		if t.Index <= 0 || t.Index >= len(p.Artifacts) {
			return nil, nil, integrity("features record: text index %d", t.Index)
		}
		if t.B64 != nil {
			p.Artifacts[t.Index].Normalized.Text = t.B64
		} else {
			p.Artifacts[t.Index].Normalized.Text = []byte(t.Text)
		}
	}
	return p, r.History, nil
}
