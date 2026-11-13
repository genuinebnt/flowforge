package contentstore

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/textindex"
)

// Re-derive jobs (V2 §12.2). When the pipeline's stamp changes (a
// normalizer, extractor or rule table was upgraded), every stored snapshot
// is re-derived from its stored raw body: its normalized text, features
// record, kind, coverage, simhash and normalizer_version are rewritten in
// place, the diffs touching it are recomputed between snapshots of the
// new derivation, and the index documents of those diffs are replaced.
// Re-derivation is not a change: it creates no snapshot, diff, finding
// event or alert, moves no validity interval, counts no capture, and
// keeps every id and diff sequence number (CSTORE-INV-15).
//
// Order. A snapshot is re-derived only after its predecessor (lineage
// parent, else parent), so the diff into it is recomputed between two
// re-derived snapshots and its feature-id history is the predecessor's
// new one; the job walks a program's stale snapshots in (valid_from, id)
// order and derives a stale predecessor first when the order alone does
// not. The diffs out of a re-derived snapshot whose successor is still
// stale keep their old content: their hunks, stored by reference to the
// old normalized text, are rewritten verbatim, and the successor's own
// re-derivation recomputes them. At every commit point each diff is
// therefore either entirely of the old derivation or entirely of the new.
//
// Storage. The raw blobs (capture record, redacted body) are unchanged.
// The derived blobs are encoded as at ingest (a delta frame against the
// delta base's current blobs, or plain), and every snapshot whose delta
// base is the re-derived one is re-encoded against its new blobs in the
// same transaction, so no frame ever references a dictionary that
// changed. The old blobs become garbage for the next chunk GC.
//
// Progress is the stamps themselves: a crash leaves each snapshot old or
// new, and the next call carries on (resumable); a snapshot already at
// the stamp is skipped, and re-deriving a snapshot whose derivation did
// not change writes nothing but its stamp (idempotent, CSTORE-INV-14).
// Volatility is not re-learned: a snapshot is re-derived with the learned
// set it was derived with (its normalizer_version names it), and its
// integrity report (degraded or not) is kept, since the host's
// catch-all sample it was judged against is not stored.

// RederiveOptions bound one Rederive call.
type RederiveOptions struct {
	// Program restricts the job to one program (Nil: every program of
	// the tenant).
	Program core.UUID
	// BatchSize bounds the snapshots one call re-derives, predecessors
	// derived first included (default 64).
	BatchSize int
}

// RederiveReport is what one Rederive call did.
type RederiveReport struct {
	// Snapshots were brought to the store's stamp.
	Snapshots int
	// Rewritten of them had derived records that changed (the others had
	// only their stamp set).
	Rewritten int
	// Diffs were recomputed (both sides re-derived) and HunksVerbatim
	// re-encoded with their lines inline (successor still stale).
	Diffs, HunksVerbatim int
	// Remaining reports stale snapshots left: call again.
	Remaining bool
}

func (r *RederiveReport) add(o RederiveReport) {
	r.Snapshots += o.Snapshots
	r.Rewritten += o.Rewritten
	r.Diffs += o.Diffs
	r.HunksVerbatim += o.HunksVerbatim
}

// Rederive re-derives up to BatchSize of the tenant's snapshots whose
// DeriveStamp is not the store's pipeline stamp (V2 §12.2), oldest first
// per program, and reports whether stale snapshots remain. It is safe to
// run concurrently with ingests and with itself.
func (s *Store) Rederive(ctx context.Context, tenant core.UUID, opts RederiveOptions) (RederiveReport, error) {
	if tenant.IsZero() {
		return RederiveReport{}, invalid("tenant is required")
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = 64
	}
	progs := []core.UUID{opts.Program}
	if opts.Program.IsZero() {
		var err error
		if progs, err = s.Programs(ctx, tenant); err != nil {
			return RederiveReport{}, err
		}
	}
	var rep RederiveReport
	for _, prog := range progs {
		var after *Cursor
		for rep.Snapshots < opts.BatchSize {
			batch, err := s.staleSnapshots(ctx, tenant, prog, after, min(opts.BatchSize-rep.Snapshots, 64))
			if err != nil {
				return rep, err
			}
			if len(batch) == 0 {
				break
			}
			for _, sn := range batch {
				if rep.Snapshots >= opts.BatchSize {
					break
				}
				r, err := s.rederiveChain(ctx, tenant, sn.ID)
				if err != nil {
					return rep, err
				}
				rep.add(r)
				after = &Cursor{ValidFrom: sn.ValidFrom, ID: sn.ID}
			}
		}
	}
	for _, prog := range progs {
		left, err := s.staleSnapshots(ctx, tenant, prog, nil, 1)
		if err != nil {
			return rep, err
		}
		if len(left) > 0 {
			rep.Remaining = true
			break
		}
	}
	return rep, nil
}

func (s *Store) staleSnapshots(ctx context.Context, tenant, prog core.UUID, after *Cursor, limit int) ([]Snapshot, error) {
	var out []Snapshot
	err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		var err error
		out, err = tx.StaleSnapshots(ctx, prog, s.cfg.Pipeline.Stamp, after, limit)
		return err
	})
	if err != nil {
		return nil, internal("stale snapshots", err)
	}
	return out, nil
}

// maxRederiveSteps bounds one chain of predecessors re-derived first (a
// guard against a corrupt parent cycle; real chains are bounded by
// retention).
const maxRederiveSteps = 1 << 16

// rederiveChain re-derives one snapshot, deriving its stale predecessors
// first, with conflict retries per step.
func (s *Store) rederiveChain(ctx context.Context, tenant, id core.UUID) (RederiveReport, error) {
	var total RederiveReport
	stack := []core.UUID{id}
	for steps := 0; len(stack) > 0; steps++ {
		if steps > maxRederiveSteps {
			return total, integrity("re-derive of %s: predecessor chain longer than %d", core.PublicID(PrefixSnapshot, id), maxRederiveSteps)
		}
		top := stack[len(stack)-1]
		var need core.UUID
		var r RederiveReport
		var err error
		for attempt := 0; ; attempt++ {
			need, r, err = s.rederiveOnce(ctx, tenant, top)
			if !errors.Is(err, ErrConflict) || attempt+1 >= s.cfg.MaxRetries {
				break
			}
		}
		if err != nil {
			return total, err
		}
		if !need.IsZero() {
			if slices.Contains(stack, need) {
				return total, integrity("re-derive of %s: predecessor cycle", core.PublicID(PrefixSnapshot, need))
			}
			stack = append(stack, need)
			continue
		}
		total.add(r)
		stack = stack[:len(stack)-1]
	}
	return total, nil
}

// rederiveState is what the read transaction of one re-derive saw.
type rederiveState struct {
	snap    Snapshot
	found   bool
	pred    Snapshot // lineage parent, else parent
	hasPred bool
	in      *Diff                  // the diff into snap (baseline: From Nil)
	out     []Diff                 // the diffs out of snap
	succ    map[core.UUID]Snapshot // their new sides
	deps    []Snapshot             // snapshots whose delta base is snap
	base    Snapshot               // snap's delta base
	hasBase bool
	vol     []VolatilityRow
}

// rewrittenDiff is a diff whose records the re-derive rewrites; docs are
// its new index documents when it is recomputed (nil: content unchanged).
type rewrittenDiff struct {
	old, new Diff
	docs     []textindex.Document
}

// rederiveOnce re-derives snapshot id if it is stale and its predecessor
// is not. It returns the predecessor's id instead when that is stale.
func (s *Store) rederiveOnce(ctx context.Context, tenant, id core.UUID) (core.UUID, RederiveReport, error) {
	stamp := s.cfg.Pipeline.Stamp
	var st rederiveState
	err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		var err error
		if st.snap, st.found, err = tx.GetSnapshot(ctx, id); err != nil || !st.found || st.snap.DeriveStamp == stamp {
			return err
		}
		predID := st.snap.LineageParentID
		if predID.IsZero() {
			predID = st.snap.ParentID
		}
		if !predID.IsZero() {
			if st.pred, st.hasPred, err = tx.GetSnapshot(ctx, predID); err != nil {
				return err
			}
		}
		if !st.snap.DeltaBase.IsZero() {
			if st.base, st.hasBase, err = tx.GetSnapshot(ctx, st.snap.DeltaBase); err != nil {
				return err
			}
		}
		ds, err := tx.DiffsTouching(ctx, []core.UUID{id})
		if err != nil {
			return err
		}
		st.succ = map[core.UUID]Snapshot{}
		for _, d := range ds {
			switch {
			case d.To == id:
				st.in = &d
			case d.From == id:
				c, ok, err := tx.GetSnapshot(ctx, d.To)
				if err != nil {
					return err
				}
				if ok {
					st.out = append(st.out, d)
					st.succ[d.To] = c
				}
			}
		}
		if st.deps, err = tx.DeltaDependents(ctx, []core.UUID{id}); err != nil {
			return err
		}
		st.vol, err = tx.Volatility(ctx, st.snap.Program, st.snap.URLTemplate, st.snap.Profile)
		return err
	})
	if err != nil {
		return core.Nil, RederiveReport{}, internal("re-derive read", err)
	}
	if !st.found || st.snap.DeriveStamp == stamp {
		return core.Nil, RederiveReport{}, nil // gone (retention), or done meanwhile
	}
	if st.hasPred && st.pred.DeriveStamp != stamp {
		return st.pred.ID, RederiveReport{}, nil
	}
	tc, err := s.cfg.Chunks.open(ctx, tenant)
	if err != nil {
		return core.Nil, RederiveReport{}, err
	}
	// The gate is held from the first put to the commit (ChunkStores).
	tc.gate.RLock()
	defer tc.gate.RUnlock()
	snap := st.snap
	old, err := s.snapshotPlain(ctx, tc, snap)
	if err != nil {
		return core.Nil, RederiveReport{}, err
	}
	oldP, oldHist, err := decodeFeatures(old.Features, old.Normalized)
	if err != nil {
		return core.Nil, RederiveReport{}, err
	}
	c, pageURL, err := s.rawCapture(ctx, snap, old)
	if err != nil {
		return core.Nil, RederiveReport{}, err
	}
	hasher, err := s.cfg.Keys.hasher(ctx, tenant)
	if err != nil {
		return core.Nil, RederiveReport{}, err
	}

	// The predecessor, already re-derived: the old side of the diff into
	// snap (rebased for a lineage pair) and, for a version of the same key,
	// the previous capture of the integrity checks (as at ingest).
	var prev *content.Processed
	var prevHist []string
	if st.hasPred {
		if prev, prevHist, err = s.loadProcessed(ctx, tc, st.pred); err != nil {
			return core.Nil, RederiveReport{}, err
		}
		if prev.URL != snap.URL {
			prev = rebase(prev, snap.URL)
		}
	}
	popts := content.ProcessOptions{Learned: learnedSetAt(st.vol, learnedVersion(snap.NormalizerVersion)), Hasher: hasher,
		Limits: s.cfg.Limits, Splitter: s.cfg.Splitter, Lineage: s.cfg.Lineage, PageURL: pageURL}
	if prev != nil && snap.LineageParentID.IsZero() {
		popts.Previous = &content.PreviousCapture{Status: prev.Status, FeatureIDs: prev.FeatureIDs(), ErrorPage: prev.ErrorPage}
	}
	cur, err := s.cfg.Pipeline.Process(c, popts)
	if err != nil {
		return core.Nil, RederiveReport{}, invalid("%v", err)
	}
	// The integrity verdict stands (re-derivation never moves a validity
	// interval); a failed or degraded capture has no reading authority, as
	// content's coverage rule says.
	cur.Integrity = oldP.Integrity
	if o := cur.Integrity.Outcome; o == content.OutcomeFailed || o == content.OutcomeDegraded {
		cur.Coverage, cur.CoveredRules = oam.CoverageFailed, nil
	}
	main := cur.Main()
	history := oldHist
	if prev != nil {
		history = mergeHistory(prevHist, prev.FeatureIDs())
	}
	fb, nb, err := encodeFeatures(cur, history)
	if err != nil {
		return core.Nil, RederiveReport{}, err
	}
	upd := snap
	upd.Kind, upd.NormalizerVersion, upd.Coverage, upd.SimHash, upd.DeriveStamp = string(main.Kind), main.Normalized.Version,
		string(cur.Coverage), cur.SimHash, stamp
	changed := !bytes.Equal(nb, old.Normalized) || !bytes.Equal(fb, old.Features) || upd.Kind != snap.Kind ||
		upd.NormalizerVersion != snap.NormalizerVersion || upd.Coverage != snap.Coverage || upd.SimHash != snap.SimHash
	rep := RederiveReport{Snapshots: 1}
	var added int64
	depRefs := map[core.UUID]BlobRefs{}
	var diffs []rewrittenDiff
	if changed {
		rep.Rewritten = 1
		plain := plainBlobs{Record: old.Record, Body: old.Body, Normalized: nb, Features: fb}
		codec := CodecDelta
		if snap.Codec == CodecChunk.String() || snap.Codec == "" {
			codec = CodecChunk
		}
		var basePlain *plainBlobs
		if codec == CodecDelta && st.hasBase {
			bp, err := s.snapshotPlain(ctx, tc, st.base)
			if err != nil {
				return core.Nil, rep, err
			}
			basePlain = &bp
		}
		enc, err := encodeSnapshotBlobs(codec, basePlain, plain)
		if err != nil {
			return core.Nil, rep, err
		}
		if added, err = putSnapshotBlobs(ctx, tc, &upd, enc); err != nil {
			return core.Nil, rep, err
		}
		// Dependents: same plain blobs, re-encoded against the new ones.
		for _, d := range st.deps {
			dp, err := s.snapshotPlain(ctx, tc, d)
			if err != nil {
				return core.Nil, rep, err
			}
			denc, err := encodeSnapshotBlobs(CodecDelta, &plain, dp)
			if err != nil {
				return core.Nil, rep, err
			}
			n := d
			a, err := putSnapshotBlobs(ctx, tc, &n, denc)
			if err != nil {
				return core.Nil, rep, err
			}
			added += a
			depRefs[d.ID] = BlobRefs{HeadersRef: n.HeadersRef, BodyManifest: n.BodyManifest, NormalizedManifest: n.NormalizedManifest,
				FeaturesRef: n.FeaturesRef, Codec: d.Codec, DeltaBase: d.DeltaBase, DeltaDepth: d.DeltaDepth}
		}
		if diffs, err = s.rederiveDiffs(ctx, tc, st, upd, prev, prevHist, cur, history, &added); err != nil {
			return core.Nil, rep, err
		}
		for _, d := range diffs {
			if d.docs != nil {
				rep.Diffs++
			} else {
				rep.HunksVerbatim++
			}
		}
	}

	err = s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		row, ok, err := tx.LockSnapshot(ctx, id)
		if err != nil {
			return err
		}
		if !ok || !sameBlobs(row, snap) {
			return conflict("snapshot %s changed during its re-derive", snap.PublicID())
		}
		if changed {
			// Successors first: most are also delta dependents, rewritten
			// below.
			for _, sc := range st.succ {
				lc, ok, err := tx.LockSnapshot(ctx, sc.ID)
				if err != nil {
					return err
				}
				if !ok || !sameBlobs(lc, sc) {
					return conflict("successor %s changed during a re-derive", sc.PublicID())
				}
			}
			deps, err := tx.DeltaDependents(ctx, []core.UUID{id})
			if err != nil {
				return err
			}
			if len(deps) != len(st.deps) {
				return conflict("delta dependents of %s changed", snap.PublicID())
			}
			for _, d := range deps {
				i := slices.IndexFunc(st.deps, func(x Snapshot) bool { return x.ID == d.ID })
				if i < 0 {
					return conflict("delta dependents of %s changed", snap.PublicID())
				}
				ld, ok, err := tx.LockSnapshot(ctx, d.ID)
				if err != nil {
					return err
				}
				if !ok || !sameBlobs(ld, st.deps[i]) {
					return conflict("delta dependent %s changed", d.PublicID())
				}
				if err := tx.RewriteBlobs(ctx, d.ID, depRefs[d.ID]); err != nil {
					return err
				}
			}
			for _, d := range diffs {
				cd, ok, err := tx.GetDiff(ctx, d.old.ID)
				if err != nil {
					return err
				}
				if !ok || cd.HunksRef != d.old.HunksRef || cd.SemanticRef != d.old.SemanticRef {
					return conflict("diff %s changed during a re-derive", d.old.PublicID())
				}
				if err := tx.RewriteDiff(ctx, d.new); err != nil {
					return err
				}
			}
			u, err := tx.Usage(ctx)
			if err != nil {
				return err
			}
			u.StoredBytes += added
			if err := tx.PutUsage(ctx, u); err != nil {
				return err
			}
		}
		return tx.UpdateDerived(ctx, upd)
	})
	if err != nil {
		return core.Nil, rep, internal("re-derive write", err)
	}
	if !changed {
		return core.Nil, rep, nil
	}
	s.cache.drop(tenant, id)
	return core.Nil, rep, s.reindex(ctx, tc, tenant, upd, nb, diffs)
}

// rederiveDiffs rewrites the diffs touching a re-derived snapshot: the
// diff into it is recomputed (its predecessor is re-derived), as is a
// diff out of it whose successor is; a diff out of it into a stale
// successor keeps its content with its hunks inlined.
func (s *Store) rederiveDiffs(ctx context.Context, tc *tenantChunks, st rederiveState, upd Snapshot, prev *content.Processed,
	prevHist []string, cur *content.Processed, history []string, added *int64) ([]rewrittenDiff, error) {
	put := func(b []byte) (string, int64, error) {
		eb, err := encodeDiffBlob(s.cfg.Codec, b)
		if err != nil {
			return "", 0, err
		}
		ref, n, err := tc.put(ctx, eb)
		*added += n
		return ref, n, err
	}
	recompute := func(d Diff, to Snapshot, from, into *content.Processed, hist []string, lineage bool) (rewrittenDiff, error) {
		baseline := d.From.IsZero()
		opts := s.cfg.Diff
		opts.History = hist
		cs, err := content.Compare(from, into, opts)
		if err != nil {
			return rewrittenDiff{}, invalid("%v", err)
		}
		if baseline {
			if err := baselineArtifacts(&cs, into, opts); err != nil {
				return rewrittenDiff{}, err
			}
		}
		hb, sb, err := diffRecords(&cs, baseline, from, into)
		if err != nil {
			return rewrittenDiff{}, err
		}
		n := d
		n.Levels, n.Algorithm, n.NormalizerVersion = slices.Clone(cs.Levels), diffAlgorithm(s.cfg.Diff.Policy, &cs), into.Main().Normalized.Version
		n.Stats = diffStats(&cs, lineage)
		var a, b int64
		if n.HunksRef, a, err = put(hb); err != nil {
			return rewrittenDiff{}, err
		}
		if n.SemanticRef, b, err = put(sb); err != nil {
			return rewrittenDiff{}, err
		}
		n.StoredSize = a + b
		hr, sr := fullRecords(&cs, baseline)
		docs := diffDocuments(n, to, hr, sr)
		n.IndexDocs = len(docs)
		if docs == nil {
			docs = []textindex.Document{}
		}
		return rewrittenDiff{old: d, new: n, docs: docs}, nil
	}
	var out []rewrittenDiff
	if st.in != nil {
		var from *content.Processed
		var hist []string
		if !st.in.From.IsZero() {
			from, hist = prev, prevHist
		}
		r, err := recompute(*st.in, upd, from, cur, hist, !upd.LineageParentID.IsZero())
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	for _, d := range st.out {
		succ := st.succ[d.To]
		if succ.DeriveStamp == s.cfg.Pipeline.Stamp {
			into, _, err := s.loadProcessed(ctx, tc, succ)
			if err != nil {
				return nil, err
			}
			from := cur
			if succ.URL != upd.URL {
				from = rebase(cur, succ.URL)
			}
			r, err := recompute(d, succ, from, into, history, !succ.LineageParentID.IsZero())
			if err != nil {
				return nil, err
			}
			out = append(out, r)
			continue
		}
		hr, _, err := s.loadDiff(ctx, tc, d, succ)
		if err != nil {
			return nil, err
		}
		hb, err := verbatimHunks(hr)
		if err != nil {
			return nil, err
		}
		n := d
		if n.HunksRef, _, err = put(hb); err != nil {
			return nil, err
		}
		out = append(out, rewrittenDiff{old: d, new: n})
	}
	return out, nil
}

// verbatimHunks encodes a full hunks record with every hunk's lines
// inline, so it no longer depends on the two snapshots' normalized texts.
func verbatimHunks(hr hunksRecord) ([]byte, error) {
	stored := hunksRecord{V: 1, Baseline: hr.Baseline, Artifacts: []artifactHunks{}}
	for _, a := range hr.Artifacts {
		sa := artifactHunks{DocID: a.DocID, Kind: a.Kind, Vendor: a.Vendor}
		for _, h := range a.Hunks {
			sa.Compact = append(sa.Compact, compactHunk{Header: h.Header, Lines: h.Lines})
		}
		stored.Artifacts = append(stored.Artifacts, sa)
	}
	b, err := core.CanonicalJSON(stored)
	if err != nil {
		return nil, internal("encode hunks", err)
	}
	return b, nil
}

// reindex replaces the index documents of recomputed diffs that the
// index already holds (diffs above the watermark are indexed later from
// their rewritten rows), and the key's latest-snapshot document.
func (s *Store) reindex(ctx context.Context, tc *tenantChunks, tenant core.UUID, snap Snapshot, normalized []byte, diffs []rewrittenDiff) error {
	if s.cfg.Index == nil {
		return nil
	}
	pi, err := s.cfg.Index.get(tenant, snap.Program)
	if err != nil {
		return err
	}
	pi.mu.Lock()
	defer pi.mu.Unlock()
	var st IndexState
	if err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		var err error
		st, err = tx.IndexState(ctx, snap.Program)
		return err
	}); err != nil {
		return internal("index state", err)
	}
	n := 0
	for _, d := range diffs {
		if d.docs == nil || d.old.Seq > st.Watermark {
			continue
		}
		for i := len(d.docs); i < d.old.IndexDocs; i++ {
			if err := pi.ix.Delete(docID(d.old.Seq, i)); err != nil {
				return internal("index delete", err)
			}
		}
		for _, doc := range d.docs {
			if err := pi.ix.Update(doc); err != nil {
				return internal("index update", err)
			}
		}
		n++
	}
	latest := slices.ContainsFunc(diffs, func(d rewrittenDiff) bool { return d.old.To == snap.ID && d.old.Seq <= st.Watermark })
	if s.cfg.Index.IndexLatest && snap.Open() && latest {
		if err := pi.ix.Update(latestDocument(snap, normalized)); err != nil {
			return internal("index update", err)
		}
		n++
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

// rawCapture rebuilds a snapshot's capture from its stored raw blobs: the
// capture record with its header values restored from the headers seal,
// and the body restored from the body seal (checked against the stored
// hash). It also returns the page URL the capture was ingested with.
func (s *Store) rawCapture(ctx context.Context, snap Snapshot, plain plainBlobs) (*content.Capture, string, error) {
	var rec captureRecord
	if err := json.Unmarshal(plain.Record, &rec); err != nil {
		return nil, "", integrity("capture record: %v", err)
	}
	hspans, err := s.openSeal(ctx, snap, "headers", snap.HeadersSeal)
	if err != nil {
		return nil, "", err
	}
	c, err := rec.capture(hspans)
	if err != nil {
		return nil, "", err
	}
	bspans, err := s.openSeal(ctx, snap, "body", snap.BodySeal)
	if err != nil {
		return nil, "", err
	}
	body, err := restore(plain.Body, bspans)
	if err != nil {
		return nil, "", err
	}
	if subtle.ConstantTimeCompare([]byte(sha256Hex(body)), []byte(snap.BodySHA256)) != 1 {
		return nil, "", integrity("restored body of %s does not match its hash", snap.PublicID())
	}
	c.Body = body
	return c, rec.PageURL, nil
}

// learnedVersion is the learned volatility set version k named by a
// normalizer_version "<name>.learned<k>@<n>" (content.Normalized.Version),
// 0 when none was applied.
func learnedVersion(v string) int {
	_, rest, ok := strings.Cut(v, ".learned")
	if !ok {
		return 0
	}
	digits, _, _ := strings.Cut(rest, "@")
	k, err := strconv.Atoi(digits)
	if err != nil || k < 0 {
		return 0
	}
	return k
}

// learnedSetAt is the learned set as it was at version k: the rows frozen
// at versions 1..k (paths never unfreeze, so a later set only adds rows).
func learnedSetAt(rows []VolatilityRow, k int) content.VolatilitySet {
	var keep []VolatilityRow
	for _, r := range rows {
		if r.FrozenVersion > 0 && r.FrozenVersion <= k {
			keep = append(keep, r)
		}
	}
	return learnedSet(keep)
}
