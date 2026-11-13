package contentstore

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"time"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
)

// GetSnapshot returns one snapshot of the tenant (NOT_FOUND for another
// tenant's id: existence is never revealed).
func (s *Store) GetSnapshot(ctx context.Context, tenant, id core.UUID) (Snapshot, error) {
	var out Snapshot
	err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		sn, ok, err := tx.GetSnapshot(ctx, id)
		if err != nil {
			return err
		}
		if !ok {
			return notFound("snapshot %s", core.PublicID(PrefixSnapshot, id))
		}
		out = sn
		return nil
	})
	if err != nil {
		return Snapshot{}, internal("get snapshot", err)
	}
	return out, nil
}

// AsOf returns the version of a key that was valid at t (NOT_FOUND when
// none: before the first capture, or inside a degraded capture's empty
// interval only).
func (s *Store) AsOf(ctx context.Context, tenant core.UUID, k Key, t time.Time) (Snapshot, error) {
	var out Snapshot
	err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		sn, ok, err := tx.SnapshotAsOf(ctx, k, t.UTC())
		if err != nil {
			return err
		}
		if !ok {
			return notFound("no version of %s valid at %s", k.URL, core.FormatTime(t))
		}
		out = sn
		return nil
	})
	if err != nil {
		return Snapshot{}, internal("as of", err)
	}
	return out, nil
}

// ListSnapshots lists snapshots, newest first (limit 1..1000, default 100).
func (s *Store) ListSnapshots(ctx context.Context, tenant core.UUID, f SnapshotFilter) ([]Snapshot, error) {
	if f.Program.IsZero() {
		return nil, invalid("program is required")
	}
	if f.Limit <= 0 {
		f.Limit = 100
	}
	f.Limit = min(f.Limit, 1000)
	var out []Snapshot
	err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		var err error
		out, err = tx.ListSnapshots(ctx, f)
		return err
	})
	if err != nil {
		return nil, internal("list snapshots", err)
	}
	return out, nil
}

// ContentOptions selects what SnapshotContent reads.
type ContentOptions struct {
	Body       bool
	Normalized bool
	Features   bool
	// Raw restores the redacted credential spans of the body and headers
	// (decrypting the seals). Callers must authorize it: it is the only
	// read that returns secrets.
	Raw bool
}

// SnapshotContent is a snapshot's stored content.
type SnapshotContent struct {
	Snapshot Snapshot
	// Capture is the capture metadata (headers redacted unless Raw).
	Capture *content.Capture
	// Body is the body (redacted unless Raw).
	Body []byte
	// Normalized is the main artifact's normalized text (always redacted).
	Normalized []byte
	// Processed is the processed capture (features, artifacts, grades).
	Processed *content.Processed
	// Findings are the encrypted findings' clear columns.
	Findings []Finding
}

// SnapshotContent reads a snapshot's blobs.
func (s *Store) SnapshotContent(ctx context.Context, tenant, id core.UUID, o ContentOptions) (SnapshotContent, error) {
	snap, err := s.GetSnapshot(ctx, tenant, id)
	if err != nil {
		return SnapshotContent{}, err
	}
	tc, err := s.cfg.Chunks.open(ctx, tenant)
	if err != nil {
		return SnapshotContent{}, err
	}
	out := SnapshotContent{Snapshot: snap}
	plain, err := s.snapshotPlain(ctx, tc, snap)
	if err != nil {
		return out, err
	}
	var rec captureRecord
	if err := json.Unmarshal(plain.Record, &rec); err != nil {
		return out, integrity("capture record: %v", err)
	}
	var hspans []sealSpan
	if o.Raw {
		if hspans, err = s.openSeal(ctx, snap, "headers", snap.HeadersSeal); err != nil {
			return out, err
		}
	}
	if out.Capture, err = rec.capture(hspans); err != nil {
		return out, err
	}
	if o.Body || o.Raw {
		b := plain.Body
		if o.Raw {
			spans, err := s.openSeal(ctx, snap, "body", snap.BodySeal)
			if err != nil {
				return out, err
			}
			if b, err = restore(b, spans); err != nil {
				return out, err
			}
			if subtle.ConstantTimeCompare([]byte(sha256Hex(b)), []byte(snap.BodySHA256)) != 1 {
				return out, integrity("restored body of %s does not match its hash", snap.PublicID())
			}
		}
		out.Body = b
		out.Capture.Body = b
	}
	if o.Normalized || o.Features {
		p, _, err := decodeFeatures(plain.Features, plain.Normalized)
		if err != nil {
			return out, err
		}
		out.Normalized = p.Main().Normalized.Text
		if o.Features {
			out.Processed = p
		}
	}
	if err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		out.Findings, err = tx.Findings(ctx, id)
		return err
	}); err != nil {
		return out, internal("findings", err)
	}
	for i := range out.Findings {
		out.Findings[i].Ciphertext = nil
	}
	return out, nil
}

// DiffView is a comparison of two snapshots for viewers: the change set
// with unified hunks and side-by-side rows per artifact.
type DiffView struct {
	From, To  Snapshot
	ChangeSet content.ChangeSet
	// Stored is the stored diff between the two, when they are
	// consecutive versions (or a lineage pair).
	Stored *Diff
}

// Diff compares two snapshots of the tenant (V2 §11 item 14). Both must
// be of one program and profile; snapshots of different URLs are compared
// as a rename (the older under the newer's URL), which needs the same
// main kind. Degraded snapshots have no diff. The result is
// deterministic, and for a stored pair its hunks equal the stored ones
// (CSTORE-INV-04).
func (s *Store) Diff(ctx context.Context, tenant, a, b core.UUID) (DiffView, error) {
	var v DiffView
	err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		var ok bool
		var err error
		if v.From, ok, err = tx.GetSnapshot(ctx, a); err != nil {
			return err
		} else if !ok {
			return notFound("snapshot %s", core.PublicID(PrefixSnapshot, a))
		}
		if v.To, ok, err = tx.GetSnapshot(ctx, b); err != nil {
			return err
		} else if !ok {
			return notFound("snapshot %s", core.PublicID(PrefixSnapshot, b))
		}
		if d, ok, err := tx.DiffInto(ctx, b); err != nil {
			return err
		} else if ok && d.From == a {
			v.Stored = &d
		}
		return nil
	})
	if err != nil {
		return DiffView{}, internal("diff", err)
	}
	if v.From.Program != v.To.Program || v.From.Profile != v.To.Profile {
		return DiffView{}, invalid("snapshots of different programs or profiles are not comparable")
	}
	if v.From.Degraded || v.To.Degraded {
		return DiffView{}, invalid("a degraded capture has no diff")
	}
	if v.From.Kind != v.To.Kind {
		return DiffView{}, invalid("snapshots of different kinds (%s, %s) are not comparable", v.From.Kind, v.To.Kind)
	}
	tc, err := s.cfg.Chunks.open(ctx, tenant)
	if err != nil {
		return DiffView{}, err
	}
	pa, histA, err := s.loadProcessed(ctx, tc, v.From)
	if err != nil {
		return DiffView{}, err
	}
	pb, _, err := s.loadProcessed(ctx, tc, v.To)
	if err != nil {
		return DiffView{}, err
	}
	if pa.URL != pb.URL {
		pa = rebase(pa, pb.URL)
	}
	opts := s.cfg.Diff
	if v.Stored != nil {
		opts.History = histA
	}
	cs, err := content.Compare(pa, pb, opts)
	if err != nil {
		return DiffView{}, invalid("%v", err)
	}
	v.ChangeSet = cs
	return v, nil
}

// BaselineDiff returns the change set of a snapshot against the empty
// document (every line added): the view of a key's first version.
func (s *Store) BaselineDiff(ctx context.Context, tenant, id core.UUID) (DiffView, error) {
	snap, err := s.GetSnapshot(ctx, tenant, id)
	if err != nil {
		return DiffView{}, err
	}
	tc, err := s.cfg.Chunks.open(ctx, tenant)
	if err != nil {
		return DiffView{}, err
	}
	p, _, err := s.loadProcessed(ctx, tc, snap)
	if err != nil {
		return DiffView{}, err
	}
	cs, err := content.Compare(nil, p, s.cfg.Diff)
	if err != nil {
		return DiffView{}, invalid("%v", err)
	}
	if err := baselineArtifacts(&cs, p, s.cfg.Diff); err != nil {
		return DiffView{}, err
	}
	return DiffView{To: snap, ChangeSet: cs}, nil
}

// StoredDiff reads a stored diff's records: the unified hunks per
// artifact and the semantic change set.
func (s *Store) StoredDiff(ctx context.Context, tenant, id core.UUID) (Diff, []ArtifactHunks, content.ChangeSet, error) {
	var d Diff
	err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		var ok bool
		var err error
		if d, ok, err = tx.GetDiff(ctx, id); err != nil {
			return err
		} else if !ok {
			return notFound("diff %s", core.PublicID(PrefixDiff, id))
		}
		return nil
	})
	if err != nil {
		return Diff{}, nil, content.ChangeSet{}, internal("stored diff", err)
	}
	tc, err := s.cfg.Chunks.open(ctx, tenant)
	if err != nil {
		return Diff{}, nil, content.ChangeSet{}, err
	}
	to, err := s.GetSnapshot(ctx, tenant, d.To)
	if err != nil {
		return Diff{}, nil, content.ChangeSet{}, err
	}
	hr, sr, err := s.loadDiff(ctx, tc, d, to)
	if err != nil {
		return Diff{}, nil, content.ChangeSet{}, err
	}
	out := make([]ArtifactHunks, len(hr.Artifacts))
	for i, a := range hr.Artifacts {
		out[i] = ArtifactHunks{DocID: a.DocID, Kind: a.Kind, Hunks: a.Hunks, Vendor: a.Vendor}
	}
	return d, out, sr.ChangeSet, nil
}

// ArtifactHunks are one artifact's stored unified hunks.
type ArtifactHunks struct {
	DocID  string
	Kind   content.Kind
	Hunks  []content.Hunk
	Vendor bool
}
