package contentstore

import (
	"context"
	"slices"

	"github.com/genuinebnt/flowforge/chunkstore"
	"github.com/genuinebnt/flowforge/core"
)

// RetentionReport is what a retention pass deleted.
type RetentionReport struct {
	Snapshots int
	Diffs     int
}

// keep decides which snapshots of one key survive (V2 §6.2, §12.12):
// sorted newest first, a snapshot is kept when it is among the newest
// KeepLast, was fetched within KeepFor of now, is pinned, or is the
// key's newest snapshot or open version. The rest are returned for
// deletion. Pure, so CSTORE-INV-03 checks it directly.
func keep(snaps []Snapshot, p RetentionPolicy, now core.Clock) (deleted []Snapshot) {
	p = p.withDefaults()
	sorted := slices.Clone(snaps)
	slices.SortFunc(sorted, snapshotOrder)
	cutoff := now.Now().Add(-p.KeepFor)
	for i, s := range sorted {
		if i == 0 || i < p.KeepLast || !s.FetchedAt.Before(cutoff) || s.Pinned || s.Open() {
			continue
		}
		deleted = append(deleted, s)
	}
	return deleted
}

// retainKey applies the retention policy to one key. Snapshots outside
// the deletion set whose delta base is deleted are first rewritten as
// keyframes (their plain content is unchanged), so no surviving blob
// depends on a deleted one. The rewrite runs outside the transaction (it
// writes chunks), so a dependent that appears meanwhile restarts the pass;
// after MaxRetries restarts the key is left as it is for the next pass.
// Index documents of the deleted diffs are tombstoned after the commit.
func (s *Store) retainKey(ctx context.Context, tenant core.UUID, k Key) (RetentionReport, error) {
	for attempt := 0; ; attempt++ {
		rep, gone, again, err := s.retainKeyOnce(ctx, tenant, k)
		if err != nil {
			return RetentionReport{}, err
		}
		if again && attempt < s.cfg.MaxRetries {
			continue
		}
		return rep, s.tombstone(ctx, tenant, k.Program, gone)
	}
}

func (s *Store) retainKeyOnce(ctx context.Context, tenant core.UUID, k Key) (RetentionReport, []Diff, bool, error) {
	var ids []core.UUID
	var deps []Snapshot
	err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		snaps, err := tx.KeySnapshots(ctx, k)
		if err != nil {
			return err
		}
		for _, d := range keep(snaps, s.cfg.Retention, s.cfg.Clock) {
			ids = append(ids, d.ID)
		}
		if len(ids) == 0 {
			return nil
		}
		deps, err = tx.DeltaDependents(ctx, ids)
		return err
	})
	if err != nil {
		return RetentionReport{}, nil, false, internal("retention", err)
	}
	if len(ids) == 0 {
		return RetentionReport{}, nil, false, nil
	}
	tc, err := s.cfg.Chunks.open(ctx, tenant)
	if err != nil {
		return RetentionReport{}, nil, false, err
	}
	tc.gate.RLock()
	defer tc.gate.RUnlock()
	rewrites := map[core.UUID]BlobRefs{}
	for _, d := range deps {
		if slices.Contains(ids, d.ID) {
			continue
		}
		plain, err := s.snapshotPlain(ctx, tc, d)
		if err != nil {
			return RetentionReport{}, nil, false, err
		}
		enc, err := encodeSnapshotBlobs(CodecDelta, nil, plain)
		if err != nil {
			return RetentionReport{}, nil, false, err
		}
		n := d
		if _, err := putSnapshotBlobs(ctx, tc, &n, enc); err != nil {
			return RetentionReport{}, nil, false, err
		}
		rewrites[d.ID] = BlobRefs{HeadersRef: n.HeadersRef, BodyManifest: n.BodyManifest, NormalizedManifest: n.NormalizedManifest,
			FeaturesRef: n.FeaturesRef, Codec: CodecDelta.String()}
	}
	var rep RetentionReport
	var gone []Diff
	again := false
	err = s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		cur, err := tx.DeltaDependents(ctx, ids)
		if err != nil {
			return err
		}
		for _, d := range cur {
			if slices.Contains(ids, d.ID) {
				continue
			}
			b, ok := rewrites[d.ID]
			if !ok {
				again = true // a new dependent appeared; start over
				return nil
			}
			if err := tx.RewriteBlobs(ctx, d.ID, b); err != nil {
				return err
			}
		}
		if gone, err = tx.DiffsTouching(ctx, ids); err != nil {
			return err
		}
		rep = RetentionReport{Snapshots: len(ids), Diffs: len(gone)}
		return tx.DeleteSnapshots(ctx, ids)
	})
	if err != nil {
		return RetentionReport{}, nil, false, internal("retention", err)
	}
	return rep, gone, again, nil
}

// ApplyRetention applies the retention policy to every key of the
// tenant, then garbage-collects its chunk store (deleted snapshots' and
// diffs' chunks are reclaimed only through chunkstore GC, within the
// tenant's own namespace: no cross-tenant dedupe, V2 §12.12).
func (s *Store) ApplyRetention(ctx context.Context, tenant core.UUID) (RetentionReport, error) {
	var keys []Key
	err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		ps, err := tx.Programs(ctx)
		if err != nil {
			return err
		}
		for _, p := range ps {
			ks, err := tx.Keys(ctx, p)
			if err != nil {
				return err
			}
			keys = append(keys, ks...)
		}
		return nil
	})
	if err != nil {
		return RetentionReport{}, internal("retention keys", err)
	}
	var total RetentionReport
	for _, k := range keys {
		r, err := s.retainKey(ctx, tenant, k)
		if err != nil {
			return total, err
		}
		total.Snapshots += r.Snapshots
		total.Diffs += r.Diffs
	}
	if _, err := s.GC(ctx, tenant); err != nil {
		return total, err
	}
	return total, nil
}

// GC collects the tenant's unreferenced chunks: the live set is every
// manifest the tenant's rows reference, pinned snapshots' manifests are
// pinned, and packs below half live are rewritten with recompression at
// level 19 (kept only when it saves 1/8, V2 §12.12). The tenant's stored
// bytes usage is then set to the store's physical record bytes.
func (s *Store) GC(ctx context.Context, tenant core.UUID) (chunkstore.GCReport, error) {
	tc, err := s.cfg.Chunks.open(ctx, tenant)
	if err != nil {
		return chunkstore.GCReport{}, err
	}
	tc.gate.Lock()
	defer tc.gate.Unlock()
	var live, pinned []string
	if err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		var err error
		live, pinned, err = tx.Manifests(ctx)
		return err
	}); err != nil {
		return chunkstore.GCReport{}, internal("gc roots", err)
	}
	parse := func(refs []string) ([]chunkstore.ManifestID, error) {
		out := make([]chunkstore.ManifestID, 0, len(refs))
		for _, r := range refs {
			id, err := chunkstore.ParseManifestID(r)
			if err != nil {
				return nil, integrity("manifest id %q: %v", r, err)
			}
			out = append(out, id)
		}
		return out, nil
	}
	l, err := parse(live)
	if err != nil {
		return chunkstore.GCReport{}, err
	}
	p, err := parse(pinned)
	if err != nil {
		return chunkstore.GCReport{}, err
	}
	rep, err := tc.s.GC(ctx, l, p, chunkstore.GCOptions{LiveThresholdPercent: 50, RecompressLevel: 19})
	if err != nil {
		return rep, internal("chunk gc", err)
	}
	bytes := int64(tc.s.Metrics().CompressedBytes)
	err = s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		u, err := tx.Usage(ctx)
		if err != nil {
			return err
		}
		u.StoredBytes = bytes
		return tx.PutUsage(ctx, u)
	})
	if err != nil {
		return rep, internal("usage", err)
	}
	return rep, nil
}

// Pin pins a snapshot: retention never deletes it and chunk GC keeps its
// blobs (V2 §12.12: versions cited by a reviewed or alerted change).
func (s *Store) Pin(ctx context.Context, tenant, id core.UUID, reason string) (Snapshot, error) {
	return s.setPinned(ctx, tenant, id, true, reason)
}

// Unpin removes a pin.
func (s *Store) Unpin(ctx context.Context, tenant, id core.UUID) (Snapshot, error) {
	return s.setPinned(ctx, tenant, id, false, "")
}

func (s *Store) setPinned(ctx context.Context, tenant, id core.UUID, pinned bool, reason string) (Snapshot, error) {
	if len(reason) > 256 {
		return Snapshot{}, invalid("pin reason longer than 256 bytes")
	}
	var out Snapshot
	err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		_, ok, err := tx.GetSnapshot(ctx, id)
		if err != nil {
			return err
		}
		if !ok {
			return notFound("snapshot %s", core.PublicID(PrefixSnapshot, id))
		}
		if err := tx.SetPinned(ctx, id, pinned, reason); err != nil {
			return err
		}
		out, _, err = tx.GetSnapshot(ctx, id)
		return err
	})
	if err != nil {
		return Snapshot{}, internal("pin", err)
	}
	return out, nil
}

// QuotaStatus is a tenant's limits and usage.
type QuotaStatus struct {
	Limits Limits
	Usage  Usage
}

// Quota returns the tenant's quota status; Usage.Captures is today's.
func (s *Store) Quota(ctx context.Context, tenant core.UUID) (QuotaStatus, error) {
	var q QuotaStatus
	err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		var err error
		if q.Limits, err = s.limitsOf(ctx, tx); err != nil {
			return err
		}
		q.Usage, err = tx.Usage(ctx)
		return err
	})
	if err != nil {
		return q, internal("quota", err)
	}
	if d := today(s.cfg.Clock.Now()); q.Usage.Day != d {
		q.Usage.Day, q.Usage.Captures = d, 0
	}
	return q, nil
}

// SetLimits overrides the tenant's quotas (negative values are invalid;
// zero is unlimited).
func (s *Store) SetLimits(ctx context.Context, tenant core.UUID, l Limits) error {
	if l.CapturesPerDay < 0 || l.StoredBytes < 0 || l.IndexBytes < 0 {
		return invalid("quota limits must not be negative")
	}
	if err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error { return tx.PutLimits(ctx, l) }); err != nil {
		return internal("limits", err)
	}
	return nil
}
