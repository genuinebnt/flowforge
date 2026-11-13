package contentstore

import (
	"crypto/sha256"
	"encoding/binary"
	"strconv"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// Public identifier prefixes of the records this package owns (spec 1.2
// style: "<prefix>_<26 Crockford base32>").
const (
	PrefixSnapshot core.Prefix = "snp"
	PrefixDiff     core.Prefix = "cdf"
)

// Key names one versioned artifact: a (URL, capture profile) of a
// program (V2 §5: diffs compare only snapshots with the same key).
type Key struct {
	Program core.UUID
	URL     string
	Profile string
}

// Snapshot is one stored capture (V2 §6.2 content.snapshots, with the
// §12.12 validity interval and pinning). Validity is [ValidFrom, ValidTo):
// the open version of a key has ValidTo zero; an unchanged re-fetch only
// moves LastSeen. A degraded capture (V2 §12.5) is stored closed at once
// (ValidTo == ValidFrom), so it is never the open version and never
// diffed against.
type Snapshot struct {
	ID          core.UUID
	Tenant      core.UUID
	Program     core.UUID
	EntityID    string
	URL         string
	URLTemplate string
	Host        string
	Profile     string
	RunID       string
	EffectPath  string
	FetchedAt   time.Time
	LastSeen    time.Time
	ValidFrom   time.Time
	ValidTo     time.Time // zero: the open version
	Status      int
	Mime        string
	Kind        string
	// Signature is SHA-256 (hex) over the body hash, status, media type
	// and the normalized security-header block: an unchanged re-fetch has
	// the same signature (V2 §12.12).
	Signature  string
	BodySHA256 string // SHA-256 of the raw body, before redaction
	RawSize    int64  // raw body + raw header bytes
	StoredSize int64  // chunk-record bytes this ingest added (dedupe-aware)
	// Chunk-store manifest ids (hex): the capture metadata record, the
	// redacted body, the main artifact's normalized text and the features
	// record.
	HeadersRef         string
	BodyManifest       string
	NormalizedManifest string
	FeaturesRef        string
	NormalizerVersion  string
	ParentID           core.UUID // previous version of the same key (Nil: none)
	LineageParentID    core.UUID // lineage predecessor of a renamed script (Nil: none)
	Outcome            string    // integrity outcome: complete, partial, failed, degraded
	Coverage           string    // oam coverage of the reading
	Degraded           bool
	SimHash            uint64
	Pinned             bool
	PinReason          string
	// Codec is the blob codec ("delta" or "chunk"); with "delta" the
	// blobs are FFZB frames against DeltaBase's blobs (Nil: a keyframe),
	// DeltaDepth versions from the last keyframe.
	Codec      string
	DeltaBase  core.UUID
	DeltaDepth int
	// BodySeal and HeadersSeal are AES-256-GCM ciphertexts of the spans
	// that redaction replaced (V2 §6.4, §12.11), under data key KeyID.
	BodySeal    []byte
	HeadersSeal []byte
	KeyID       string
	CreatedAt   time.Time
}

// Key returns the snapshot's key.
func (s Snapshot) Key() Key { return Key{Program: s.Program, URL: s.URL, Profile: s.Profile} }

// Open reports whether the snapshot is its key's open version.
func (s Snapshot) Open() bool { return s.ValidTo.IsZero() }

// PublicID returns "snp_...".
func (s Snapshot) PublicID() string { return core.PublicID(PrefixSnapshot, s.ID) }

// BlobRefs are a snapshot's blob references and delta base.
type BlobRefs struct {
	HeadersRef, BodyManifest, NormalizedManifest, FeaturesRef string
	Codec                                                     string
	DeltaBase                                                 core.UUID
	DeltaDepth                                                int
}

// DiffStats are the summary columns of a stored diff.
type DiffStats struct {
	Outcome   string         `json:"outcome"`
	Class     string         `json:"class,omitempty"`
	Newness   string         `json:"newness,omitempty"`
	Cause     string         `json:"cause"`
	Baseline  bool           `json:"baseline,omitempty"`
	Score     float64        `json:"score"`
	Inserted  int            `json:"inserted"`
	Deleted   int            `json:"deleted"`
	Artifacts int            `json:"artifacts"`
	Changes   int            `json:"changes"`
	Summary   map[string]int `json:"summary,omitempty"`
	Lineage   bool           `json:"lineage,omitempty"`
}

// Diff is one stored diff (V2 §6.2 content.diffs). From is Nil for the
// baseline diff of a key's first version (every line added), which is
// what makes "first appeared" answerable from the diff history alone
// (V2 §12.14). Seq orders a program's diffs; the text index watermark
// and document ids are derived from it.
type Diff struct {
	ID                core.UUID
	Tenant            core.UUID
	Program           core.UUID
	Seq               int64
	From              core.UUID
	To                core.UUID
	Levels            []string
	Algorithm         string
	NormalizerVersion string
	Stats             DiffStats
	HunksRef          string
	SemanticRef       string
	StoredSize        int64
	// IndexDocs is the number of text-index documents the diff produced
	// (ids Seq<<20 | 0..IndexDocs-1).
	IndexDocs int
	At        time.Time // the new snapshot's fetch time
	URL       string
	Profile   string
	CreatedAt time.Time
}

// PublicID returns "cdf_...".
func (d Diff) PublicID() string { return core.PublicID(PrefixDiff, d.ID) }

// Finding is one encrypted secret (V2 §6.2 content.findings, §6.4): only
// RuleID, Preview and KeyedHash are in the clear.
type Finding struct {
	ID         core.UUID
	Tenant     core.UUID
	Program    core.UUID
	Snapshot   core.UUID
	RuleID     string
	Preview    string
	KeyedHash  string
	Ciphertext []byte
	KeyID      string
	CreatedAt  time.Time
}

// VolatilityRow is the learned-volatility state of one feature path (V2
// §6.2 content.volatility): counters while unfrozen, the version that
// froze it once a rule.
type VolatilityRow struct {
	Program       core.UUID
	URLTemplate   string
	Profile       string
	Path          string
	Changes       int
	Stable        int
	Streak        int
	FrozenVersion int // 0: not frozen
}

// Limits are a tenant's quotas (V2 §11 item 18). Zero means unlimited.
type Limits struct {
	CapturesPerDay int64 `json:"captures_per_day"`
	StoredBytes    int64 `json:"stored_bytes"`
	IndexBytes     int64 `json:"index_bytes"`
}

// Usage is a tenant's quota usage. Captures counts the accepted captures
// of Day (UTC, YYYY-MM-DD); StoredBytes are chunk-record bytes and
// IndexBytes text-index segment bytes.
type Usage struct {
	Day         string `json:"day"`
	Captures    int64  `json:"captures"`
	StoredBytes int64  `json:"stored_bytes"`
	IndexBytes  int64  `json:"index_bytes"`
}

// IndexState is the search watermark of one (tenant, program) index:
// every diff with Seq <= Watermark has been indexed.
type IndexState struct {
	Program   core.UUID
	Watermark int64
	Docs      int64
	Bytes     int64
}

// DataKey is a wrapped per-tenant data key (envelope encryption, V2
// §6.4): Wrapped is the key encrypted by the key-encryption key KEKID.
type DataKey struct {
	Tenant    core.UUID
	Purpose   string
	Version   int
	KEKID     string
	Wrapped   []byte
	CreatedAt time.Time
}

// ID is the data key's id as recorded on ciphertexts: "<purpose>/<version>".
func (k DataKey) ID() string { return k.Purpose + "/" + strconv.Itoa(k.Version) }

// OutboxEvent is one row of the content outbox (spec 23.7): Payload is
// the outbox JSON every service writes (services/internal/convert
// OutboxJSON).
type OutboxEvent struct {
	EventID     core.UUID
	Topic       string
	OrderingKey string
	Payload     []byte
}

// derivedUUID hashes length-prefixed parts under a domain into a version 8
// UUID (RFC 9562), so records derived from the same inputs get the same
// id on every replica and replay.
//
// PORT: the encoding is uvarint(len) || bytes for the domain and each
// part, SHA-256, first 16 bytes, version nibble 8, variant 10.
func derivedUUID(domain string, parts ...string) core.UUID {
	h := sha256.New()
	var n [binary.MaxVarintLen64]byte
	for _, p := range append([]string{domain}, parts...) {
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(p)))])
		h.Write([]byte(p))
	}
	var u core.UUID
	copy(u[:], h.Sum(nil))
	u[6] = (u[6] & 0x0f) | 0x80
	u[8] = (u[8] & 0x3f) | 0x80
	return u
}
