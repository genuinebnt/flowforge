package contentstore

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
)

// RetentionPolicy is the per-(URL, profile) retention of V2 §6.2: keep
// the newest KeepLast snapshots and every snapshot fetched within
// KeepFor, whichever keeps more (their union). Pinned snapshots and each
// key's newest snapshot are always kept (V2 §12.12). Zero fields take
// the defaults (50, 180 days).
type RetentionPolicy struct {
	KeepLast int
	KeepFor  time.Duration
	// DisableOnIngest stops applying the policy to a key right after each
	// new version of it; the background sweep (Retain) still runs.
	DisableOnIngest bool
}

func (p RetentionPolicy) withDefaults() RetentionPolicy {
	if p.KeepLast <= 0 {
		p.KeepLast = 50
	}
	if p.KeepFor <= 0 {
		p.KeepFor = 180 * 24 * time.Hour
	}
	return p
}

// Config configures a Store.
type Config struct {
	Repo   Repo
	Chunks *ChunkStores
	Keys   *Keyring
	// Index is the search integration (nil: no search; ingest still
	// stores diffs, so an index can be built later).
	Index *Indexes
	Clock core.Clock
	// DefaultLimits apply to tenants without an override (zero fields:
	// unlimited).
	DefaultLimits Limits
	Retention     RetentionPolicy
	// Diff configures every diff (policy, bounds, context lines).
	Diff content.DiffOptions
	// Lineage is the lineage matcher; LineageOptions its thresholds.
	Lineage        content.LineageAlgo
	LineageOptions content.LineageOptions
	Splitter       content.SplitOptions
	Limits         content.Limits
	Volatility     content.VolatilityPolicy
	// HostBodies bounds the catch-all sample per capture (default 50).
	HostBodies int
	// LineageCandidates bounds the scripts considered as predecessors
	// (default 64).
	LineageCandidates int
	// MaxRetries bounds the retries of an ingest that lost a race on its
	// key (default 5).
	MaxRetries int
	// Codec is the blob codec of new snapshots (default CodecDelta).
	Codec BlobCodec
	// KeyframeInterval bounds delta chains: DeltaDepth stays below it, and
	// a snapshot whose depth would reach it is stored as a keyframe
	// (default 16).
	KeyframeInterval int
	// CacheBytes bounds the decoded-blob cache (default 64 MiB).
	CacheBytes int
	// Pipeline derives captures (default: content.Process, stamped with
	// DerivationStamp). Snapshots derived under another stamp are
	// re-derived by Rederive, and by an ingest before it uses one as its
	// predecessor.
	Pipeline Pipeline
}

// Store is the content store: capture ingest, the diff pipeline, findings
// encryption, volatility learning, retention, quotas and search over a
// Repo, per-tenant chunk stores and per-(tenant, program) text indexes
// (V2 §6, §7). Safe for concurrent use.
type Store struct {
	cfg   Config
	cache *blobCache
}

// New returns a Store. Repo, Chunks, Keys and Clock are required.
func New(cfg Config) (*Store, error) {
	if cfg.Repo == nil || cfg.Chunks == nil || cfg.Keys == nil || cfg.Clock == nil {
		return nil, invalid("contentstore: Repo, Chunks, Keys and Clock are required")
	}
	cfg.Retention = cfg.Retention.withDefaults()
	if cfg.HostBodies <= 0 {
		cfg.HostBodies = 50
	}
	if cfg.LineageCandidates <= 0 {
		cfg.LineageCandidates = 64
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 5
	}
	if cfg.LineageOptions.Algo != cfg.Lineage {
		cfg.LineageOptions.Algo = cfg.Lineage
	}
	if cfg.KeyframeInterval <= 0 {
		cfg.KeyframeInterval = 16
	}
	if cfg.CacheBytes <= 0 {
		cfg.CacheBytes = 64 << 20
	}
	cfg.Pipeline = cfg.Pipeline.withDefaults()
	return &Store{cfg: cfg, cache: newBlobCache(cfg.CacheBytes)}, nil
}

// Config returns the store's configuration (with defaults applied).
func (s *Store) Config() Config { return s.cfg }

// loadProcessed reads a snapshot's processed capture back from its
// features record and normalized text.
func (s *Store) loadProcessed(ctx context.Context, tc *tenantChunks, snap Snapshot) (*content.Processed, []string, error) {
	p, err := s.snapshotPlain(ctx, tc, snap)
	if err != nil {
		return nil, nil, err
	}
	return decodeFeatures(p.Features, p.Normalized)
}

// sealPayload is the plaintext of a redaction seal.
type sealPayload struct {
	Spans []sealSpan `json:"spans"`
}

func sealAAD(tenant, snapshot core.UUID, what string) string {
	return "ff-content-seal|" + tenant.String() + "|" + snapshot.String() + "|" + what
}

func findingAAD(tenant, snapshot, finding core.UUID) string {
	return "ff-content-finding|" + tenant.String() + "|" + snapshot.String() + "|" + finding.String()
}

// openSeal decrypts a seal (nil ciphertext: no spans).
func (s *Store) openSeal(ctx context.Context, snap Snapshot, what string, ct []byte) ([]sealSpan, error) {
	if len(ct) == 0 {
		return nil, nil
	}
	pt, err := s.cfg.Keys.decrypt(ctx, snap.Tenant, snap.KeyID, sealAAD(snap.Tenant, snap.ID, what), ct)
	if err != nil {
		return nil, err
	}
	var p sealPayload
	if err := json.Unmarshal(pt, &p); err != nil {
		return nil, integrity("seal: %v", err)
	}
	return p.Spans, nil
}

// findingPayload is the plaintext of a finding's ciphertext.
type findingPayload struct {
	Secret []byte `json:"secret"`
	Doc    string `json:"doc,omitempty"`
	Start  int    `json:"start"`
	End    int    `json:"end"`
	Rule   string `json:"rule"`
}

// restoreFindings puts the decrypted secrets back on the findings of a
// processed capture loaded from storage (Observe needs them for the keyed
// hash), matching on (rule, doc, start, end).
func (s *Store) restoreFindings(ctx context.Context, snap Snapshot, p *content.Processed) error {
	var fs []Finding
	if err := s.cfg.Repo.InTx(ctx, snap.Tenant, func(tx Tx) error {
		var err error
		fs, err = tx.Findings(ctx, snap.ID)
		return err
	}); err != nil {
		return internal("findings", err)
	}
	type spot struct {
		rule, doc  string
		start, end int
	}
	secrets := map[spot][]byte{}
	for _, f := range fs {
		pt, err := s.cfg.Keys.decrypt(ctx, snap.Tenant, f.KeyID, findingAAD(snap.Tenant, snap.ID, f.ID), f.Ciphertext)
		if err != nil {
			return err
		}
		var fp findingPayload
		if err := json.Unmarshal(pt, &fp); err != nil {
			return integrity("finding: %v", err)
		}
		secrets[spot{fp.Rule, fp.Doc, fp.Start, fp.End}] = fp.Secret
	}
	for i := range p.Artifacts {
		for j := range p.Artifacts[i].Features.Findings {
			f := &p.Artifacts[i].Features.Findings[j]
			if sec, ok := secrets[spot{f.RuleID, f.Doc, f.Start, f.End}]; ok {
				f.Secret = sec
			}
		}
	}
	return nil
}

// rebase moves a processed capture to another URL, so a lineage
// predecessor diffs against its successor under the successor's document
// ids (V2 §5: "the diff is computed against the predecessor").
func rebase(p *content.Processed, url string) *content.Processed {
	q := *p
	old := p.URL
	q.URL = url
	q.Artifacts = make([]content.Artifact, len(p.Artifacts))
	copy(q.Artifacts, p.Artifacts)
	for i := range q.Artifacts {
		if rest, ok := strings.CutPrefix(q.Artifacts[i].DocID, old); ok {
			q.Artifacts[i].DocID = url + rest
		}
	}
	return &q
}

// hostOf returns the lower-cased authority of a URL without userinfo (the
// port is kept; "" when there is no "://").
func hostOf(raw string) string {
	_, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return ""
	}
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		rest = rest[:i]
	}
	if i := strings.LastIndexByte(rest, '@'); i >= 0 {
		rest = rest[i+1:]
	}
	return strings.ToLower(rest)
}

// today is the UTC date of t.
func today(t time.Time) string { return t.UTC().Format("2006-01-02") }

// untilTomorrow is the time from t to the next UTC midnight.
func untilTomorrow(t time.Time) time.Duration {
	u := t.UTC()
	next := time.Date(u.Year(), u.Month(), u.Day()+1, 0, 0, 0, 0, time.UTC)
	return next.Sub(u)
}

// loadDiff reads a stored diff's records and expands them with the two
// snapshots' processed captures (to is the diff's new snapshot).
func (s *Store) loadDiff(ctx context.Context, tc *tenantChunks, d Diff, to Snapshot) (hunksRecord, semanticRecord, error) {
	hb, err := tc.get(ctx, d.HunksRef)
	if err != nil {
		return hunksRecord{}, semanticRecord{}, err
	}
	if hb, err = decodeDiffBlob(hb); err != nil {
		return hunksRecord{}, semanticRecord{}, err
	}
	sb, err := tc.get(ctx, d.SemanticRef)
	if err != nil {
		return hunksRecord{}, semanticRecord{}, err
	}
	if sb, err = decodeDiffBlob(sb); err != nil {
		return hunksRecord{}, semanticRecord{}, err
	}
	cur, _, err := s.loadProcessed(ctx, tc, to)
	if err != nil {
		return hunksRecord{}, semanticRecord{}, err
	}
	var prev *content.Processed
	if !d.From.IsZero() {
		from, err := s.GetSnapshot(ctx, d.Tenant, d.From)
		if err != nil {
			return hunksRecord{}, semanticRecord{}, err
		}
		if prev, _, err = s.loadProcessed(ctx, tc, from); err != nil {
			return hunksRecord{}, semanticRecord{}, err
		}
		if prev.URL != cur.URL {
			prev = rebase(prev, cur.URL)
		}
	}
	return expandRecords(hb, sb, prev, cur, s.cfg.Diff)
}
