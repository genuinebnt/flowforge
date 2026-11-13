package contentstore

import (
	"bytes"
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

// MemRepo is the in-memory Repo: the fake of the port, used by tests and
// the in-memory dev stack. Transactions are serialized by one mutex and
// rolled back with an undo log, so InTx has the isolation of Postgres'
// serializable level. It also holds the outbox (Claim).
type MemRepo struct {
	mu      sync.Mutex
	tenants map[core.UUID]*memTenant
	outbox  []memOutboxRow
	outIDs  map[core.UUID]bool
	claimMu sync.Mutex
}

type memOutboxRow struct {
	id        int64
	e         OutboxEvent
	at        time.Time
	published bool
}

type memTenant struct {
	programs map[core.UUID]int64 // program -> last diff seq
	snaps    map[core.UUID]Snapshot
	open     map[string]core.UUID     // key -> open version
	byKey    map[string][]core.UUID   // key -> snapshots (insertion order)
	diffs    map[core.UUID]Diff       // id -> diff
	into     map[core.UUID]core.UUID  // to snapshot -> diff id
	findings map[core.UUID][]Finding  // snapshot -> findings
	vol      map[string]VolatilityRow // program|template|profile|path -> row
	usage    Usage
	limits   *Limits
	index    map[core.UUID]IndexState
	keys     []DataKey
}

// NewMemRepo returns an empty repository.
func NewMemRepo() *MemRepo {
	return &MemRepo{tenants: map[core.UUID]*memTenant{}, outIDs: map[core.UUID]bool{}}
}

func keyString(k Key) string { return k.Program.String() + "\x00" + k.Profile + "\x00" + k.URL }

func (r *MemRepo) tenant(t core.UUID) *memTenant {
	mt := r.tenants[t]
	if mt == nil {
		mt = &memTenant{programs: map[core.UUID]int64{}, snaps: map[core.UUID]Snapshot{}, open: map[string]core.UUID{},
			byKey: map[string][]core.UUID{}, diffs: map[core.UUID]Diff{}, into: map[core.UUID]core.UUID{},
			findings: map[core.UUID][]Finding{}, vol: map[string]VolatilityRow{}, index: map[core.UUID]IndexState{}}
		r.tenants[t] = mt
	}
	return mt
}

// InTx implements Repo.
func (r *MemRepo) InTx(ctx context.Context, tenant core.UUID, fn func(Tx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	tx := &memTx{r: r, t: r.tenant(tenant), tenant: tenant}
	if err := fn(tx); err != nil {
		for i := len(tx.undo) - 1; i >= 0; i-- {
			tx.undo[i]()
		}
		return err
	}
	return nil
}

// Tenants implements Repo.
func (r *MemRepo) Tenants(context.Context) ([]core.UUID, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []core.UUID
	for t, mt := range r.tenants {
		if len(mt.programs) > 0 {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b core.UUID) int { return bytes.Compare(a[:], b[:]) })
	return out, nil
}

// Ping implements Repo.
func (r *MemRepo) Ping(context.Context) error { return nil }

// ClaimOutbox hands up to limit unpublished outbox events to publish in
// append order and marks them published only when publish succeeds (at
// least once, as bus.OutboxSource).
func (r *MemRepo) ClaimOutbox(ctx context.Context, limit int, publish func(context.Context, []OutboxEvent) error) (int, error) {
	r.claimMu.Lock()
	defer r.claimMu.Unlock()
	r.mu.Lock()
	var idx []int
	var batch []OutboxEvent
	for i, row := range r.outbox {
		if !row.published {
			idx = append(idx, i)
			batch = append(batch, row.e)
			if len(batch) == limit {
				break
			}
		}
	}
	r.mu.Unlock()
	if len(batch) == 0 {
		return 0, nil
	}
	if err := publish(ctx, batch); err != nil {
		return 0, err
	}
	r.mu.Lock()
	for _, i := range idx {
		r.outbox[i].published = true
	}
	r.mu.Unlock()
	return len(batch), nil
}

// OutboxEvents returns every outbox event appended so far (tests).
func (r *MemRepo) OutboxEvents() []OutboxEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]OutboxEvent, len(r.outbox))
	for i, row := range r.outbox {
		out[i] = row.e
	}
	return out
}

// Dump returns copies of every row the repository holds for tenant, in no
// particular order (map iteration), for tests that inspect stored data
// (CSTORE-INV-07: no plaintext secret in any row).
func (r *MemRepo) Dump(tenant core.UUID) (snaps []Snapshot, diffs []Diff, findings []Finding, vol []VolatilityRow, keys []DataKey) {
	r.mu.Lock()
	defer r.mu.Unlock()
	mt := r.tenants[tenant]
	if mt == nil {
		return
	}
	for _, s := range mt.snaps {
		snaps = append(snaps, cloneSnapshot(s))
	}
	for _, d := range mt.diffs {
		diffs = append(diffs, cloneDiff(d))
	}
	for _, fs := range mt.findings {
		findings = append(findings, slices.Clone(fs)...)
	}
	for _, v := range mt.vol {
		vol = append(vol, v)
	}
	keys = slices.Clone(mt.keys)
	return
}

type memTx struct {
	r      *MemRepo
	t      *memTenant
	tenant core.UUID
	undo   []func()
}

func (tx *memTx) onUndo(f func()) { tx.undo = append(tx.undo, f) }

func cloneSnapshot(s Snapshot) Snapshot {
	s.BodySeal = bytes.Clone(s.BodySeal)
	s.HeadersSeal = bytes.Clone(s.HeadersSeal)
	return s
}

func cloneDiff(d Diff) Diff {
	d.Levels = slices.Clone(d.Levels)
	d.Stats.Summary = maps.Clone(d.Stats.Summary)
	return d
}

func (tx *memTx) Programs(context.Context) ([]core.UUID, error) {
	out := slices.Collect(maps.Keys(tx.t.programs))
	slices.SortFunc(out, func(a, b core.UUID) int { return bytes.Compare(a[:], b[:]) })
	return out, nil
}

func (tx *memTx) EnsureProgram(_ context.Context, p core.UUID) error {
	if _, ok := tx.t.programs[p]; !ok {
		tx.t.programs[p] = 0
		tx.onUndo(func() { delete(tx.t.programs, p) })
	}
	return nil
}

func (tx *memTx) OpenVersion(_ context.Context, k Key) (Snapshot, bool, error) {
	id, ok := tx.t.open[keyString(k)]
	if !ok {
		return Snapshot{}, false, nil
	}
	return cloneSnapshot(tx.t.snaps[id]), true, nil
}

func (tx *memTx) SnapshotBySignature(_ context.Context, k Key, sig string, at time.Time) (Snapshot, bool, error) {
	for _, id := range tx.t.byKey[keyString(k)] {
		s := tx.t.snaps[id]
		if s.Signature == sig && s.FetchedAt.Equal(at) {
			return cloneSnapshot(s), true, nil
		}
	}
	return Snapshot{}, false, nil
}

func (tx *memTx) GetSnapshot(_ context.Context, id core.UUID) (Snapshot, bool, error) {
	s, ok := tx.t.snaps[id]
	return cloneSnapshot(s), ok, nil
}

func (tx *memTx) SnapshotAsOf(_ context.Context, k Key, t time.Time) (Snapshot, bool, error) {
	for _, id := range tx.t.byKey[keyString(k)] {
		s := tx.t.snaps[id]
		if !s.ValidFrom.After(t) && (s.Open() || s.ValidTo.After(t)) {
			return cloneSnapshot(s), true, nil
		}
	}
	return Snapshot{}, false, nil
}

// snapshotOrder is (ValidFrom, ID) descending.
func snapshotOrder(a, b Snapshot) int {
	return cmp.Or(b.ValidFrom.Compare(a.ValidFrom), bytes.Compare(b.ID[:], a.ID[:]))
}

func (tx *memTx) ListSnapshots(_ context.Context, f SnapshotFilter) ([]Snapshot, error) {
	var out []Snapshot
	for _, s := range tx.t.snaps {
		if s.Program != f.Program || f.URL != "" && s.URL != f.URL || f.Profile != "" && s.Profile != f.Profile || f.OpenOnly && !s.Open() {
			continue
		}
		if f.Before != nil && snapshotOrder(s, Snapshot{ValidFrom: f.Before.ValidFrom, ID: f.Before.ID}) <= 0 {
			continue
		}
		out = append(out, cloneSnapshot(s))
	}
	slices.SortFunc(out, snapshotOrder)
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

func (tx *memTx) KeySnapshots(_ context.Context, k Key) ([]Snapshot, error) {
	var out []Snapshot
	for _, id := range tx.t.byKey[keyString(k)] {
		out = append(out, cloneSnapshot(tx.t.snaps[id]))
	}
	slices.SortFunc(out, snapshotOrder)
	return out, nil
}

func (tx *memTx) Keys(_ context.Context, program core.UUID) ([]Key, error) {
	var out []Key
	for _, ids := range tx.t.byKey {
		if len(ids) == 0 {
			continue
		}
		s := tx.t.snaps[ids[0]]
		if s.Program == program {
			out = append(out, s.Key())
		}
	}
	slices.SortFunc(out, func(a, b Key) int { return cmp.Or(cmp.Compare(a.URL, b.URL), cmp.Compare(a.Profile, b.Profile)) })
	return out, nil
}

func (tx *memTx) HostOpenVersions(_ context.Context, program core.UUID, host, profile, url string, limit int) ([]Snapshot, error) {
	var out []Snapshot
	for _, id := range tx.t.open {
		s := tx.t.snaps[id]
		if s.Program == program && s.Host == host && s.Profile == profile && s.URL != url {
			out = append(out, cloneSnapshot(s))
		}
	}
	slices.SortFunc(out, snapshotOrder)
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (tx *memTx) OpenVersionsByURL(_ context.Context, program core.UUID, profile string, urls []string) ([]Snapshot, error) {
	var out []Snapshot
	for _, u := range urls {
		if id, ok := tx.t.open[keyString(Key{Program: program, URL: u, Profile: profile})]; ok {
			out = append(out, cloneSnapshot(tx.t.snaps[id]))
		}
	}
	slices.SortFunc(out, snapshotOrder)
	return out, nil
}

func (tx *memTx) InsertSnapshot(_ context.Context, s Snapshot) error {
	if _, ok := tx.t.snaps[s.ID]; ok {
		return conflict("snapshot %s exists", s.PublicID())
	}
	ks := keyString(s.Key())
	if s.Open() {
		if _, ok := tx.t.open[ks]; ok {
			return conflict("key %s already has an open version", s.URL)
		}
		tx.t.open[ks] = s.ID
		tx.onUndo(func() { delete(tx.t.open, ks) })
	}
	tx.t.snaps[s.ID] = cloneSnapshot(s)
	prev := tx.t.byKey[ks]
	tx.t.byKey[ks] = append(slices.Clone(prev), s.ID)
	tx.onUndo(func() { delete(tx.t.snaps, s.ID); tx.t.byKey[ks] = prev })
	return nil
}

// update replaces snapshot id through f, recording the undo.
func (tx *memTx) update(id core.UUID, f func(*Snapshot)) error {
	old, ok := tx.t.snaps[id]
	if !ok {
		return notFound("snapshot %s", core.PublicID(PrefixSnapshot, id))
	}
	n := cloneSnapshot(old)
	f(&n)
	tx.t.snaps[id] = n
	tx.onUndo(func() { tx.t.snaps[id] = old })
	return nil
}

func (tx *memTx) CloseVersion(_ context.Context, id core.UUID, validTo time.Time) error {
	s, ok := tx.t.snaps[id]
	if !ok || !s.Open() {
		return conflict("snapshot %s is not an open version", core.PublicID(PrefixSnapshot, id))
	}
	ks := keyString(s.Key())
	delete(tx.t.open, ks)
	tx.onUndo(func() { tx.t.open[ks] = id })
	return tx.update(id, func(s *Snapshot) { s.ValidTo = validTo })
}

func (tx *memTx) Touch(_ context.Context, id core.UUID, lastSeen time.Time) error {
	return tx.update(id, func(s *Snapshot) {
		if lastSeen.After(s.LastSeen) {
			s.LastSeen = lastSeen
		}
	})
}

func (tx *memTx) SetPinned(_ context.Context, id core.UUID, pinned bool, reason string) error {
	return tx.update(id, func(s *Snapshot) {
		s.Pinned = pinned
		s.PinReason = ""
		if pinned {
			s.PinReason = reason
		}
	})
}

func (tx *memTx) DeltaDependents(_ context.Context, ids []core.UUID) ([]Snapshot, error) {
	set := map[core.UUID]bool{}
	for _, id := range ids {
		set[id] = true
	}
	var out []Snapshot
	for _, s := range tx.t.snaps {
		if !s.DeltaBase.IsZero() && set[s.DeltaBase] {
			out = append(out, cloneSnapshot(s))
		}
	}
	slices.SortFunc(out, snapshotOrder)
	return out, nil
}

func (tx *memTx) RewriteBlobs(_ context.Context, id core.UUID, b BlobRefs) error {
	return tx.update(id, func(s *Snapshot) {
		s.HeadersRef, s.BodyManifest, s.NormalizedManifest, s.FeaturesRef = b.HeadersRef, b.BodyManifest, b.NormalizedManifest, b.FeaturesRef
		s.Codec, s.DeltaBase, s.DeltaDepth = b.Codec, b.DeltaBase, b.DeltaDepth
	})
}

func (tx *memTx) StaleSnapshots(_ context.Context, program core.UUID, stamp string, after *Cursor, limit int) ([]Snapshot, error) {
	var out []Snapshot
	for _, s := range tx.t.snaps {
		if s.Program != program || s.DeriveStamp == stamp {
			continue
		}
		if after != nil && snapshotOrder(s, Snapshot{ValidFrom: after.ValidFrom, ID: after.ID}) >= 0 {
			continue
		}
		out = append(out, cloneSnapshot(s))
	}
	slices.SortFunc(out, func(a, b Snapshot) int { return snapshotOrder(b, a) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// LockSnapshot implements Tx; transactions are serialized already.
func (tx *memTx) LockSnapshot(ctx context.Context, id core.UUID) (Snapshot, bool, error) {
	return tx.GetSnapshot(ctx, id)
}

func (tx *memTx) UpdateDerived(_ context.Context, d Snapshot) error {
	return tx.update(d.ID, func(s *Snapshot) {
		s.HeadersRef, s.BodyManifest, s.NormalizedManifest, s.FeaturesRef = d.HeadersRef, d.BodyManifest, d.NormalizedManifest, d.FeaturesRef
		s.Codec, s.DeltaBase, s.DeltaDepth = d.Codec, d.DeltaBase, d.DeltaDepth
		s.Kind, s.NormalizerVersion, s.Coverage, s.SimHash, s.DeriveStamp = d.Kind, d.NormalizerVersion, d.Coverage, d.SimHash, d.DeriveStamp
	})
}

func (tx *memTx) DeleteSnapshots(ctx context.Context, ids []core.UUID) error {
	ds, _ := tx.DiffsTouching(ctx, ids)
	for _, d := range ds {
		d := d
		delete(tx.t.diffs, d.ID)
		if tx.t.into[d.To] == d.ID {
			delete(tx.t.into, d.To)
			tx.onUndo(func() { tx.t.into[d.To] = d.ID })
		}
		tx.onUndo(func() { tx.t.diffs[d.ID] = d })
	}
	gone := map[core.UUID]bool{}
	for _, id := range ids {
		gone[id] = true
	}
	for _, id := range ids {
		s, ok := tx.t.snaps[id]
		if !ok {
			continue
		}
		ks := keyString(s.Key())
		prevKey := tx.t.byKey[ks]
		tx.t.byKey[ks] = slices.DeleteFunc(slices.Clone(prevKey), func(x core.UUID) bool { return x == id })
		if len(tx.t.byKey[ks]) == 0 {
			delete(tx.t.byKey, ks)
		}
		if tx.t.open[ks] == id {
			delete(tx.t.open, ks)
			tx.onUndo(func() { tx.t.open[ks] = id })
		}
		fs := tx.t.findings[id]
		delete(tx.t.findings, id)
		delete(tx.t.snaps, id)
		tx.onUndo(func() { tx.t.snaps[id] = s; tx.t.byKey[ks] = prevKey; tx.t.findings[id] = fs })
	}
	// Children keep living with their links cleared (ON DELETE SET NULL).
	for id, s := range tx.t.snaps {
		if gone[s.ParentID] || gone[s.LineageParentID] {
			if err := tx.update(id, func(s *Snapshot) {
				if gone[s.ParentID] {
					s.ParentID = core.Nil
				}
				if gone[s.LineageParentID] {
					s.LineageParentID = core.Nil
				}
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (tx *memTx) NextDiffSeq(_ context.Context, program core.UUID) (int64, error) {
	old, ok := tx.t.programs[program]
	tx.t.programs[program] = old + 1
	tx.onUndo(func() {
		if ok {
			tx.t.programs[program] = old
		} else {
			delete(tx.t.programs, program)
		}
	})
	return old + 1, nil
}

func (tx *memTx) InsertDiff(_ context.Context, d Diff) error {
	if _, ok := tx.t.diffs[d.ID]; ok {
		return conflict("diff %s exists", d.PublicID())
	}
	tx.t.diffs[d.ID] = cloneDiff(d)
	prev, had := tx.t.into[d.To]
	tx.t.into[d.To] = d.ID
	tx.onUndo(func() {
		delete(tx.t.diffs, d.ID)
		if had {
			tx.t.into[d.To] = prev
		} else {
			delete(tx.t.into, d.To)
		}
	})
	return nil
}

func (tx *memTx) RewriteDiff(_ context.Context, d Diff) error {
	old, ok := tx.t.diffs[d.ID]
	if !ok {
		return notFound("diff %s", d.PublicID())
	}
	n := cloneDiff(old)
	n.Levels, n.Algorithm, n.NormalizerVersion, n.Stats = slices.Clone(d.Levels), d.Algorithm, d.NormalizerVersion, d.Stats
	n.Stats.Summary = maps.Clone(d.Stats.Summary)
	n.HunksRef, n.SemanticRef, n.StoredSize, n.IndexDocs = d.HunksRef, d.SemanticRef, d.StoredSize, d.IndexDocs
	tx.t.diffs[d.ID] = n
	tx.onUndo(func() { tx.t.diffs[d.ID] = old })
	return nil
}

func (tx *memTx) GetDiff(_ context.Context, id core.UUID) (Diff, bool, error) {
	d, ok := tx.t.diffs[id]
	return cloneDiff(d), ok, nil
}

func (tx *memTx) DiffInto(_ context.Context, to core.UUID) (Diff, bool, error) {
	id, ok := tx.t.into[to]
	if !ok {
		return Diff{}, false, nil
	}
	return cloneDiff(tx.t.diffs[id]), true, nil
}

func (tx *memTx) DiffsAfter(_ context.Context, program core.UUID, after int64, limit int) ([]Diff, error) {
	var out []Diff
	for _, d := range tx.t.diffs {
		if d.Program == program && d.Seq > after {
			out = append(out, cloneDiff(d))
		}
	}
	slices.SortFunc(out, func(a, b Diff) int { return cmp.Compare(a.Seq, b.Seq) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (tx *memTx) DiffsTouching(_ context.Context, ids []core.UUID) ([]Diff, error) {
	set := map[core.UUID]bool{}
	for _, id := range ids {
		set[id] = true
	}
	var out []Diff
	for _, d := range tx.t.diffs {
		if set[d.To] || !d.From.IsZero() && set[d.From] {
			out = append(out, cloneDiff(d))
		}
	}
	slices.SortFunc(out, func(a, b Diff) int {
		return cmp.Or(bytes.Compare(a.Program[:], b.Program[:]), cmp.Compare(a.Seq, b.Seq))
	})
	return out, nil
}

func (tx *memTx) InsertFindings(_ context.Context, fs []Finding) error {
	for _, f := range fs {
		f := f
		f.Ciphertext = bytes.Clone(f.Ciphertext)
		prev := tx.t.findings[f.Snapshot]
		if slices.ContainsFunc(prev, func(g Finding) bool { return g.ID == f.ID }) {
			continue
		}
		tx.t.findings[f.Snapshot] = append(slices.Clone(prev), f)
		tx.onUndo(func() { tx.t.findings[f.Snapshot] = prev })
	}
	return nil
}

func (tx *memTx) Findings(_ context.Context, snapshot core.UUID) ([]Finding, error) {
	out := slices.Clone(tx.t.findings[snapshot])
	slices.SortFunc(out, func(a, b Finding) int { return bytes.Compare(a.ID[:], b.ID[:]) })
	return out, nil
}

func volKey(program core.UUID, tmpl, profile, path string) string {
	return program.String() + "\x00" + tmpl + "\x00" + profile + "\x00" + path
}

func (tx *memTx) Volatility(_ context.Context, program core.UUID, tmpl, profile string) ([]VolatilityRow, error) {
	var out []VolatilityRow
	for _, v := range tx.t.vol {
		if v.Program == program && v.URLTemplate == tmpl && v.Profile == profile {
			out = append(out, v)
		}
	}
	slices.SortFunc(out, func(a, b VolatilityRow) int { return cmp.Compare(a.Path, b.Path) })
	return out, nil
}

func (tx *memTx) PutVolatility(_ context.Context, rows []VolatilityRow) error {
	for _, v := range rows {
		k := volKey(v.Program, v.URLTemplate, v.Profile, v.Path)
		old, had := tx.t.vol[k]
		tx.t.vol[k] = v
		tx.onUndo(func() {
			if had {
				tx.t.vol[k] = old
			} else {
				delete(tx.t.vol, k)
			}
		})
	}
	return nil
}

func (tx *memTx) Usage(context.Context) (Usage, error) { return tx.t.usage, nil }

func (tx *memTx) PutUsage(_ context.Context, u Usage) error {
	old := tx.t.usage
	tx.t.usage = u
	tx.onUndo(func() { tx.t.usage = old })
	return nil
}

func (tx *memTx) Limits(context.Context) (Limits, bool, error) {
	if tx.t.limits == nil {
		return Limits{}, false, nil
	}
	return *tx.t.limits, true, nil
}

func (tx *memTx) PutLimits(_ context.Context, l Limits) error {
	old := tx.t.limits
	tx.t.limits = &l
	tx.onUndo(func() { tx.t.limits = old })
	return nil
}

func (tx *memTx) IndexState(_ context.Context, program core.UUID) (IndexState, error) {
	s, ok := tx.t.index[program]
	if !ok {
		s.Program = program
	}
	return s, nil
}

func (tx *memTx) PutIndexState(_ context.Context, s IndexState) error {
	old, had := tx.t.index[s.Program]
	tx.t.index[s.Program] = s
	tx.onUndo(func() {
		if had {
			tx.t.index[s.Program] = old
		} else {
			delete(tx.t.index, s.Program)
		}
	})
	return nil
}

func (tx *memTx) DataKeys(context.Context) ([]DataKey, error) {
	out := slices.Clone(tx.t.keys)
	for i := range out {
		out[i].Wrapped = bytes.Clone(out[i].Wrapped)
	}
	return out, nil
}

func (tx *memTx) InsertDataKey(_ context.Context, k DataKey) (bool, error) {
	for _, x := range tx.t.keys {
		if x.Purpose == k.Purpose && x.Version == k.Version {
			return false, nil
		}
	}
	prev := tx.t.keys
	k.Wrapped = bytes.Clone(k.Wrapped)
	tx.t.keys = append(slices.Clone(prev), k)
	tx.onUndo(func() { tx.t.keys = prev })
	return true, nil
}

func (tx *memTx) AppendOutbox(_ context.Context, e OutboxEvent) error {
	r := tx.r
	if r.outIDs[e.EventID] {
		return nil
	}
	r.outIDs[e.EventID] = true
	r.outbox = append(r.outbox, memOutboxRow{id: int64(len(r.outbox) + 1), e: e})
	n := len(r.outbox) - 1
	tx.onUndo(func() { r.outbox = r.outbox[:n]; delete(r.outIDs, e.EventID) })
	return nil
}

func (tx *memTx) Manifests(context.Context) (live, pinned []string, err error) {
	for _, s := range tx.t.snaps {
		refs := []string{s.HeadersRef, s.BodyManifest, s.NormalizedManifest, s.FeaturesRef}
		live = append(live, refs...)
		if s.Pinned {
			pinned = append(pinned, refs...)
		}
	}
	for _, d := range tx.t.diffs {
		live = append(live, d.HunksRef, d.SemanticRef)
	}
	return sortedRefs(live), sortedRefs(pinned), nil
}

// sortedRefs sorts, de-duplicates and drops empty manifest refs.
func sortedRefs(rs []string) []string {
	rs = slices.DeleteFunc(rs, func(s string) bool { return s == "" })
	slices.Sort(rs)
	return slices.Compact(rs)
}
