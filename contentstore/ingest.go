package contentstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
)

// IngestRequest is one capture to store.
type IngestRequest struct {
	Program core.UUID
	Capture content.Capture
	// EntityID, RunID and EffectPath are recorded on the snapshot and
	// carried by its event (the graph entity of the URL, the run and
	// effect that fetched it).
	EntityID   string
	RunID      string
	EffectPath string
	// PageURL is the page that loaded this script or style: its previous
	// scripts are the lineage candidates (V2 §5), and it is the base URL
	// for resolving relative references.
	PageURL string
}

// IngestResult reports what an ingest did.
type IngestResult struct {
	Snapshot Snapshot
	// Diff is the stored diff into Snapshot (nil: duplicate, unchanged
	// re-fetch, degraded capture).
	Diff *Diff
	// ChangeSet is the classified comparison (nil when nothing was
	// compared).
	ChangeSet *content.ChangeSet
	// Duplicate: the exact capture was already stored; nothing changed.
	Duplicate bool
	// Unchanged: an unchanged re-fetch moved last_seen of the open
	// version (V2 §12.12); no snapshot was added.
	Unchanged bool
	// Lineage is the predecessor match of a renamed script.
	Lineage *content.LineageMatch
	// Observation is the oam observation published for the graph.
	Observation oam.Observation
	EventID     core.UUID
}

// signature is the §12.12 unchanged-re-fetch signature: SHA-256 over the
// raw body hash, status, media type and the normalized security-header
// block of the final response and every redirect hop (headers@1 drops
// the volatile ones: Date, Age, request ids, nonces, cookie values).
func signature(c *content.Capture) string {
	h := sha256.New()
	body := sha256.Sum256(c.Body)
	h.Write([]byte("ff-content-sig/1\n"))
	h.Write([]byte(hex.EncodeToString(body[:]) + "\n" + strconv.Itoa(c.Status) + "\n" + c.ContentType() + "\n"))
	h.Write(content.Normalize(content.KindHeaders, content.HeaderBlock(c.Headers), content.NormalizeOptions{}).Text)
	for _, hop := range c.RedirectChain {
		h.Write([]byte("\nhop " + strconv.Itoa(hop.Status) + " " + hop.URL + "\n"))
		h.Write(content.Normalize(content.KindHeaders, content.HeaderBlock(hop.Headers), content.NormalizeOptions{}).Text)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// snapshotID derives the snapshot id from what identifies a capture, so
// concurrent ingests of one capture collide on the primary key.
func snapshotID(tenant core.UUID, k Key, sig string, at time.Time) core.UUID {
	return derivedUUID("ff-content-snapshot", tenant.String(), k.Program.String(), k.URL, k.Profile, sig, core.FormatTime(at))
}

// Ingest stores one capture (V2 §6.2, §12.12): it is idempotent (the same
// capture twice is one snapshot, CSTORE-INV-01); an unchanged re-fetch
// only extends last_seen; otherwise the capture is processed (content
// pipeline with the key's learned volatility, the tenant's keyed hasher,
// the previous capture and the host's other bodies), linked to its
// predecessor (parent, or a lineage parent for a renamed script), diffed
// and classified, and stored: redacted body, capture record, normalized
// text and features as chunk-store blobs; hunks and semantic records as
// canonical-JSON blobs; secrets encrypted; volatility counters; quota
// usage; one outbox event. Captures over a quota fail with
// QUOTA_EXCEEDED and store no rows.
func (s *Store) Ingest(ctx context.Context, tenant core.UUID, req IngestRequest) (IngestResult, error) {
	if tenant.IsZero() || req.Program.IsZero() {
		return IngestResult{}, invalid("tenant and program are required")
	}
	c := req.Capture
	if err := c.Validate(); err != nil {
		return IngestResult{}, invalid("%v", err)
	}
	if c.FetchedAt.IsZero() {
		return IngestResult{}, invalid("capture fetched_at is required")
	}
	prof, _ := c.Profile.Normalize()
	c.Profile = prof
	c.FetchedAt = c.FetchedAt.UTC().Truncate(time.Millisecond)
	if c.Outcome == "" {
		c.Outcome = content.OutcomeComplete
	}
	var res IngestResult
	var err error
	for attempt := 0; attempt < s.cfg.MaxRetries; attempt++ {
		res, err = s.ingestOnce(ctx, tenant, req, &c)
		if !errors.Is(err, ErrConflict) {
			break
		}
	}
	if err != nil {
		return IngestResult{}, err
	}
	if !res.Duplicate && !res.Unchanged {
		if !s.cfg.Retention.DisableOnIngest {
			if _, err := s.retainKey(ctx, tenant, res.Snapshot.Key()); err != nil {
				return res, err
			}
		}
		if s.cfg.Index != nil && s.cfg.Index.Mode == IndexInline {
			if _, err := s.IndexPending(ctx, tenant, req.Program); err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

// ingestState is what the first, read-only transaction of an ingest saw.
type ingestState struct {
	open      Snapshot
	hasOpen   bool
	parentOf  Snapshot // the open version's parent (lineage candidates)
	vol       []VolatilityRow
	hosts     []Snapshot
	page      Snapshot
	hasPage   bool
	pageOld   Snapshot
	hasOld    bool
	limits    Limits
	usage     Usage
	duplicate *Snapshot
}

func (s *Store) limitsOf(ctx context.Context, tx Tx) (Limits, error) {
	l, ok, err := tx.Limits(ctx)
	if err != nil {
		return Limits{}, err
	}
	if !ok {
		l = s.cfg.DefaultLimits
	}
	return l, nil
}

// checkQuota applies the three quotas to an ingest that adds stored bytes
// (V2 §11 item 18). Exact: an ingest is admitted iff today's captures are
// below the limit, stored+added is within the limit and the index is
// below its limit.
func checkQuota(l Limits, u Usage, added int64, now time.Time) error {
	if l.CapturesPerDay > 0 && u.Day == today(now) && u.Captures >= l.CapturesPerDay {
		return quotaExceeded(QuotaCapturesPerDay, u.Captures, l.CapturesPerDay, untilTomorrow(now))
	}
	if l.StoredBytes > 0 && u.StoredBytes+added > l.StoredBytes {
		return quotaExceeded(QuotaStoredBytes, u.StoredBytes+added, l.StoredBytes, 0)
	}
	if l.IndexBytes > 0 && u.IndexBytes >= l.IndexBytes {
		return quotaExceeded(QuotaIndexBytes, u.IndexBytes, l.IndexBytes, 0)
	}
	return nil
}

// countCapture adds one capture and added bytes to the usage row.
func countCapture(u Usage, added int64, now time.Time) Usage {
	if d := today(now); u.Day != d {
		u.Day, u.Captures = d, 0
	}
	u.Captures++
	u.StoredBytes += added
	return u
}

func (s *Store) ingestOnce(ctx context.Context, tenant core.UUID, req IngestRequest, c *content.Capture) (IngestResult, error) {
	k := Key{Program: req.Program, URL: c.URL, Profile: c.Profile.Name}
	sig := signature(c)
	tmpl := urlTemplate(c.URL)
	host := hostOf(c.URL)
	now := s.cfg.Clock.Now()
	var st ingestState
	err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		var err error
		if d, ok, err := tx.SnapshotBySignature(ctx, k, sig, c.FetchedAt); err != nil {
			return err
		} else if ok {
			st.duplicate = &d
			return nil
		}
		if st.open, st.hasOpen, err = tx.OpenVersion(ctx, k); err != nil {
			return err
		}
		if st.hasOpen && !st.open.ParentID.IsZero() {
			st.parentOf, _, err = tx.GetSnapshot(ctx, st.open.ParentID)
			if err != nil {
				return err
			}
		}
		if st.vol, err = tx.Volatility(ctx, k.Program, tmpl, k.Profile); err != nil {
			return err
		}
		if st.hosts, err = tx.HostOpenVersions(ctx, k.Program, host, k.Profile, k.URL, s.cfg.HostBodies); err != nil {
			return err
		}
		if req.PageURL != "" && !st.hasOpen {
			if st.page, st.hasPage, err = tx.OpenVersion(ctx, Key{Program: k.Program, URL: req.PageURL, Profile: k.Profile}); err != nil {
				return err
			}
			if st.hasPage && !st.page.ParentID.IsZero() {
				if st.pageOld, st.hasOld, err = tx.GetSnapshot(ctx, st.page.ParentID); err != nil {
					return err
				}
			}
		}
		if st.limits, err = s.limitsOf(ctx, tx); err != nil {
			return err
		}
		st.usage, err = tx.Usage(ctx)
		return err
	})
	if err != nil {
		return IngestResult{}, internal("ingest read", err)
	}
	if st.duplicate != nil {
		return IngestResult{Snapshot: *st.duplicate, Duplicate: true}, nil
	}
	if st.hasOpen && c.FetchedAt.Before(st.open.ValidFrom) {
		return IngestResult{}, &core.Error{Kind: core.KindFailedPrecondition, Reason: ReasonOutOfOrder,
			Message: "capture fetched at " + core.FormatTime(c.FetchedAt) + " is older than the open version (" + core.FormatTime(st.open.ValidFrom) + ")"}
	}
	if st.hasOpen && st.open.Signature == sig {
		if !c.FetchedAt.After(st.open.LastSeen) {
			return IngestResult{Snapshot: st.open, Duplicate: true}, nil
		}
		return s.refetch(ctx, tenant, req, c, st)
	}
	// Early refusal before any chunk is written; the exact check (with
	// the bytes this ingest adds) runs again in the write transaction.
	if err := checkQuota(st.limits, st.usage, 0, now); err != nil {
		return IngestResult{}, err
	}
	return s.store(ctx, tenant, req, c, k, sig, tmpl, st)
}

// refetch handles an unchanged re-fetch: last_seen moves, the capture is
// counted, and the open version's observation is re-published with the
// new fetch time (graph liveness).
func (s *Store) refetch(ctx context.Context, tenant core.UUID, req IngestRequest, c *content.Capture, st ingestState) (IngestResult, error) {
	tc, err := s.cfg.Chunks.open(ctx, tenant)
	if err != nil {
		return IngestResult{}, err
	}
	p, _, err := s.loadProcessed(ctx, tc, st.open)
	if err != nil {
		return IngestResult{}, err
	}
	if err := s.restoreFindings(ctx, st.open, p); err != nil {
		return IngestResult{}, err
	}
	p.FetchedAt = c.FetchedAt
	h, err := s.cfg.Keys.hasher(ctx, tenant)
	if err != nil {
		return IngestResult{}, err
	}
	obs, err := content.Observe(p, content.ObserveOptions{Hasher: h, SnapshotID: st.open.PublicID()})
	if err != nil {
		return IngestResult{}, internal("observe", err)
	}
	evID := derivedUUID("ff-content-event", tenant.String(), st.open.ID.String(), "refetch", core.FormatTime(c.FetchedAt))
	ev, err := s.event(evID, tenant, req, st.open, nil, nil, obs, c.FetchedAt)
	if err != nil {
		return IngestResult{}, err
	}
	now := s.cfg.Clock.Now()
	err = s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		cur, ok, err := tx.OpenVersion(ctx, Key{Program: req.Program, URL: c.URL, Profile: c.Profile.Name})
		if err != nil {
			return err
		}
		if !ok || cur.ID != st.open.ID {
			return conflict("open version moved")
		}
		l, err := s.limitsOf(ctx, tx)
		if err != nil {
			return err
		}
		u, err := tx.Usage(ctx)
		if err != nil {
			return err
		}
		if err := checkQuota(l, u, 0, now); err != nil {
			return err
		}
		if err := tx.PutUsage(ctx, countCapture(u, 0, now)); err != nil {
			return err
		}
		if err := tx.Touch(ctx, st.open.ID, c.FetchedAt); err != nil {
			return err
		}
		return tx.AppendOutbox(ctx, ev)
	})
	if err != nil {
		return IngestResult{}, internal("ingest re-fetch", err)
	}
	snap := st.open
	snap.LastSeen = c.FetchedAt
	return IngestResult{Snapshot: snap, Unchanged: true, Observation: obs, EventID: evID}, nil
}

// event builds the outbox event of a stored capture.
func (s *Store) event(id, tenant core.UUID, req IngestRequest, snap Snapshot, d *Diff, cs *content.ChangeSet, obs oam.Observation, at time.Time) (OutboxEvent, error) {
	ob, err := content.ObservationJSON(obs)
	if err != nil {
		return OutboxEvent{}, internal("observation", err)
	}
	p := EventPayload{SnapshotID: snap.PublicID(), URL: snap.URL, Profile: snap.Profile, EntityID: req.EntityID,
		RunID: req.RunID, EffectPath: req.EffectPath, Observation: string(ob)}
	from, diffID := "", ""
	if d != nil {
		diffID = d.PublicID()
		if !d.From.IsZero() {
			from = core.PublicID(PrefixSnapshot, d.From)
		}
	}
	p.SnapshotFrom, p.DiffID = from, diffID
	if p, err = eventPayload(p, cs, from, snap.PublicID(), diffID); err != nil {
		return OutboxEvent{}, err
	}
	typ := EventCaptured
	if cs != nil && cs.IsChange() {
		typ = EventChanged
	}
	return newEvent(id, tenant, snap.Program, typ, at, p)
}

// scriptKind reports artifacts that take part in file lineage (V2 §5).
func scriptKind(k content.Kind) bool {
	return k == content.KindJS || k == content.KindCSS || k == content.KindTSText
}

// lineageCandidates collects the predecessor candidates of a new script:
// the scripts and styles the page loaded in its open and previous
// versions (V2 §5 "the page's previously loaded scripts"), then the other
// open versions of the host of the same kind.
func (s *Store) lineageCandidates(ctx context.Context, tenant core.UUID, tc *tenantChunks, k Key, kind content.Kind, st ingestState) ([]Snapshot, error) {
	var urls []string
	if st.hasPage {
		pages := []Snapshot{st.page}
		if st.hasOld {
			pages = append(pages, st.pageOld)
		}
		for _, pg := range pages {
			p, _, err := s.loadProcessed(ctx, tc, pg)
			if err != nil {
				return nil, err
			}
			for _, f := range p.Features() {
				if (f.Kind == content.FeatScript || f.Kind == content.FeatStyle) && f.Ref != "" && f.Ref != k.URL {
					urls = append(urls, f.Ref)
				}
			}
		}
		slices.Sort(urls)
		urls = slices.Compact(urls)
	}
	var out []Snapshot
	err := s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		if len(urls) == 0 {
			return nil
		}
		var err error
		out, err = tx.OpenVersionsByURL(ctx, k.Program, k.Profile, urls)
		return err
	})
	if err != nil {
		return nil, internal("lineage candidates", err)
	}
	// The host's other open versions follow the page's scripts: a chunk
	// the page loads only through another script (a lazy chunk) has no
	// page reference. The matcher's threshold keeps unrelated ones out.
	// A host version that shares its lineage key with one of the page's
	// previous scripts is left out: the page names the predecessor, and
	// older renames of the same bundle stay open versions (nothing
	// re-fetches a URL the site stopped serving), so with them the key
	// would no longer decide and SimHash could pick a stale generation.
	pageKeys := map[string]bool{}
	for _, x := range out {
		pageKeys[content.LineageKey(x.URL)] = true
	}
	for _, h := range st.hosts {
		if !slices.ContainsFunc(out, func(x Snapshot) bool { return x.ID == h.ID }) && !pageKeys[content.LineageKey(h.URL)] {
			out = append(out, h)
		}
	}
	out = slices.DeleteFunc(out, func(x Snapshot) bool { return content.Kind(x.Kind) != kind })
	if len(out) > s.cfg.LineageCandidates {
		out = out[:s.cfg.LineageCandidates]
	}
	return out, nil
}

// store is the path of a new version.
func (s *Store) store(ctx context.Context, tenant core.UUID, req IngestRequest, c *content.Capture, k Key, sig, tmpl string, st ingestState) (IngestResult, error) {
	tc, err := s.cfg.Chunks.open(ctx, tenant)
	if err != nil {
		return IngestResult{}, err
	}
	hasher, err := s.cfg.Keys.hasher(ctx, tenant)
	if err != nil {
		return IngestResult{}, err
	}
	var prev *content.Processed
	var history []string
	if st.hasOpen {
		if prev, history, err = s.loadProcessed(ctx, tc, st.open); err != nil {
			return IngestResult{}, err
		}
	}
	popts := content.ProcessOptions{Learned: learnedSet(st.vol), Hasher: hasher, Limits: s.cfg.Limits,
		Splitter: s.cfg.Splitter, Lineage: s.cfg.Lineage, PageURL: req.PageURL}
	if prev != nil {
		popts.Previous = &content.PreviousCapture{Status: prev.Status, FeatureIDs: prev.FeatureIDs(), ErrorPage: prev.ErrorPage}
	}
	for _, h := range st.hosts {
		if h.SimHash != 0 && !scriptKind(content.Kind(h.Kind)) {
			popts.HostBodies = append(popts.HostBodies, content.HostBody{URL: h.URL, SimHash: h.SimHash})
		}
	}
	cur, err := content.Process(c, popts)
	if err != nil {
		return IngestResult{}, invalid("%v", err)
	}
	main := cur.Main()
	degraded := cur.Integrity.Degraded()

	// Lineage: a script URL without a previous capture of its own.
	var lineage *content.LineageMatch
	var linParent Snapshot
	if prev == nil && !degraded && scriptKind(main.Kind) {
		cands, err := s.lineageCandidates(ctx, tenant, tc, k, main.Kind, st)
		if err != nil {
			return IngestResult{}, err
		}
		var lc []content.LineageCandidate
		byURL := map[string]Snapshot{}
		procs := map[string]*content.Processed{}
		hists := map[string][]string{}
		for _, cs := range cands {
			p, h, err := s.loadProcessed(ctx, tc, cs)
			if err != nil {
				return IngestResult{}, err
			}
			lc = append(lc, content.LineageCandidate{URL: cs.URL, Fingerprint: p.Main().Fingerprint})
			byURL[cs.URL], procs[cs.URL], hists[cs.URL] = cs, p, h
		}
		if m, ok := content.FindPredecessor(c.URL, main.Fingerprint, lc, s.cfg.LineageOptions); ok {
			lineage, linParent = &m, byURL[m.URL]
			prev, history = rebase(procs[m.URL], c.URL), hists[m.URL]
		}
	}

	var cs *content.ChangeSet
	if !degraded || prev != nil {
		opts := s.cfg.Diff
		opts.History = history
		set, err := content.Compare(prev, cur, opts)
		if err != nil {
			return IngestResult{}, invalid("%v", err)
		}
		if prev == nil && !degraded {
			if err := baselineArtifacts(&set, cur, opts); err != nil {
				return IngestResult{}, err
			}
		}
		cs = &set
	}

	id := snapshotID(tenant, k, sig, c.FetchedAt)
	snap := Snapshot{ID: id, Tenant: tenant, Program: k.Program, EntityID: req.EntityID, URL: k.URL, URLTemplate: tmpl,
		Host: hostOf(k.URL), Profile: k.Profile, RunID: req.RunID, EffectPath: req.EffectPath, FetchedAt: c.FetchedAt,
		LastSeen: c.FetchedAt, ValidFrom: c.FetchedAt, Status: c.Status, Mime: c.ContentType(), Kind: string(main.Kind),
		Signature: sig, BodySHA256: sha256Hex(c.Body), RawSize: int64(len(c.Body) + len(content.HeaderBlock(c.Headers))),
		NormalizerVersion: main.Normalized.Version, Outcome: string(cur.Integrity.Outcome), Coverage: string(cur.Coverage),
		Degraded: degraded, SimHash: cur.SimHash, CreatedAt: s.cfg.Clock.Now()}
	if degraded {
		snap.ValidTo = snap.ValidFrom
	}
	if st.hasOpen {
		snap.ParentID = st.open.ID
	}
	if lineage != nil {
		snap.LineageParentID = linParent.ID
	}

	// Blobs. The gate is held until the rows commit (ChunkStores).
	tc.gate.RLock()
	defer tc.gate.RUnlock()
	body, bodySpans := redact("body", c.Body)
	rec, hdrSpans := newCaptureRecord(c)
	recBytes, err := core.CanonicalJSON(rec)
	if err != nil {
		return IngestResult{}, internal("capture record", err)
	}
	if prev != nil {
		history = mergeHistory(history, prev.FeatureIDs())
	}
	fb, nb, err := encodeFeatures(cur, history)
	if err != nil {
		return IngestResult{}, err
	}
	plain := plainBlobs{Record: recBytes, Body: body, Normalized: nb, Features: fb}
	snap.Codec = s.cfg.Codec.String()
	var basePlain *plainBlobs
	if s.cfg.Codec == CodecDelta {
		// The delta base: the lineage predecessor, else the open version
		// (the parent), unless that would pass the keyframe interval.
		var base *Snapshot
		if lineage != nil {
			base = &linParent
		} else if st.hasOpen {
			base = &st.open
		}
		if base != nil && base.DeltaDepth+1 < s.cfg.KeyframeInterval {
			bp, err := s.snapshotPlain(ctx, tc, *base)
			if err != nil {
				return IngestResult{}, err
			}
			basePlain, snap.DeltaBase, snap.DeltaDepth = &bp, base.ID, base.DeltaDepth+1
		}
	}
	enc, err := encodeSnapshotBlobs(s.cfg.Codec, basePlain, plain)
	if err != nil {
		return IngestResult{}, err
	}
	added, err := putSnapshotBlobs(ctx, tc, &snap, enc)
	if err != nil {
		return IngestResult{}, err
	}
	snapAdded := added
	put := func(b []byte) (string, error) {
		eb, err := encodeDiffBlob(s.cfg.Codec, b)
		if err != nil {
			return "", err
		}
		ref, n, err := tc.put(ctx, eb)
		added += n
		return ref, err
	}

	// Seals and findings (V2 §6.4): the redacted spans and the secrets
	// only ever leave memory encrypted.
	if len(bodySpans) > 0 {
		pt, _ := json.Marshal(sealPayload{Spans: bodySpans})
		if snap.BodySeal, snap.KeyID, err = s.cfg.Keys.encrypt(ctx, tenant, sealAAD(tenant, id, "body"), pt); err != nil {
			return IngestResult{}, err
		}
	}
	if len(hdrSpans) > 0 {
		pt, _ := json.Marshal(sealPayload{Spans: hdrSpans})
		if snap.HeadersSeal, snap.KeyID, err = s.cfg.Keys.encrypt(ctx, tenant, sealAAD(tenant, id, "headers"), pt); err != nil {
			return IngestResult{}, err
		}
	}
	// One row per occurrence (artifact, span), so a stored processed
	// capture gets every secret back (restoreFindings) and the observation
	// replays exactly.
	var findings []Finding
	for _, f := range allFindings(cur) {
		fid := derivedUUID("ff-content-finding", tenant.String(), id.String(), f.RuleID, f.Doc, strconv.Itoa(f.Start), strconv.Itoa(f.End))
		pt, _ := json.Marshal(findingPayload{Secret: f.Secret, Doc: f.Doc, Start: f.Start, End: f.End, Rule: f.RuleID})
		ct, kid, err := s.cfg.Keys.encrypt(ctx, tenant, findingAAD(tenant, id, fid), pt)
		if err != nil {
			return IngestResult{}, err
		}
		findings = append(findings, Finding{ID: fid, Tenant: tenant, Program: k.Program, Snapshot: id, RuleID: f.RuleID,
			Preview: f.Preview, KeyedHash: hasher.KeyedHash(f.Secret), Ciphertext: ct, KeyID: kid, CreatedAt: snap.CreatedAt})
	}

	// The diff.
	var d *Diff
	var hunksB, semB []byte
	if cs != nil && !degraded {
		from := core.Nil
		if lineage != nil {
			from = linParent.ID
		} else if st.hasOpen {
			from = st.open.ID
		}
		hunksB, semB, err = diffRecords(cs, prev == nil, prev, cur)
		if err != nil {
			return IngestResult{}, err
		}
		d = &Diff{ID: derivedUUID("ff-content-diff", tenant.String(), from.String(), id.String(), main.Normalized.Version, s.cfg.Diff.Policy.String()),
			Tenant: tenant, Program: k.Program, From: from, To: id, Levels: slices.Clone(cs.Levels),
			Algorithm: diffAlgorithm(s.cfg.Diff.Policy, cs), NormalizerVersion: main.Normalized.Version, Stats: diffStats(cs, lineage != nil),
			At: c.FetchedAt, URL: k.URL, Profile: k.Profile, CreatedAt: snap.CreatedAt}
		if d.HunksRef, err = put(hunksB); err != nil {
			return IngestResult{}, err
		}
		if d.SemanticRef, err = put(semB); err != nil {
			return IngestResult{}, err
		}
		d.StoredSize = added - snapAdded
		hr, sr := fullRecords(cs, prev == nil)
		d.IndexDocs = len(diffDocuments(*d, snap, hr, sr))
	}
	snap.StoredSize = snapAdded

	fellBack := false
	if cs != nil {
		for _, a := range cs.Artifacts {
			fellBack = fellBack || a.HashOnly()
		}
	}
	oopts := content.ObserveOptions{Hasher: hasher, SnapshotID: snap.PublicID(), DiffFellBack: fellBack, Predecessor: lineage}
	if cs != nil {
		oopts.Cause, oopts.Baseline = cs.Cause, cs.Baseline
	}
	obs, err := content.Observe(cur, oopts)
	if err != nil {
		return IngestResult{}, internal("observe", err)
	}
	evID := derivedUUID("ff-content-event", tenant.String(), id.String(), "stored")
	ev, err := s.event(evID, tenant, req, snap, d, cs, obs, c.FetchedAt)
	if err != nil {
		return IngestResult{}, err
	}
	outcome := ""
	if cs != nil {
		outcome = cs.Outcome
	}
	vol := updateVolatility(st.vol, k.Program, tmpl, k.Profile, prevIfSameKey(prev, lineage), cur, outcome, s.cfg.Volatility)

	now := s.cfg.Clock.Now()
	err = s.cfg.Repo.InTx(ctx, tenant, func(tx Tx) error {
		open, ok, err := tx.OpenVersion(ctx, k)
		if err != nil {
			return err
		}
		if ok != st.hasOpen || ok && open.ID != st.open.ID {
			return conflict("open version of %s moved", k.URL)
		}
		if _, dup, err := tx.GetSnapshot(ctx, id); err != nil {
			return err
		} else if dup {
			return conflict("snapshot stored concurrently")
		}
		l, err := s.limitsOf(ctx, tx)
		if err != nil {
			return err
		}
		u, err := tx.Usage(ctx)
		if err != nil {
			return err
		}
		if err := checkQuota(l, u, added, now); err != nil {
			return err
		}
		if err := tx.PutUsage(ctx, countCapture(u, added, now)); err != nil {
			return err
		}
		if err := tx.EnsureProgram(ctx, k.Program); err != nil {
			return err
		}
		if st.hasOpen && !degraded {
			if err := tx.CloseVersion(ctx, st.open.ID, c.FetchedAt); err != nil {
				return err
			}
		}
		if err := tx.InsertSnapshot(ctx, snap); err != nil {
			return err
		}
		if d != nil {
			if d.Seq, err = tx.NextDiffSeq(ctx, k.Program); err != nil {
				return err
			}
			if err := tx.InsertDiff(ctx, *d); err != nil {
				return err
			}
		}
		if len(findings) > 0 {
			if err := tx.InsertFindings(ctx, findings); err != nil {
				return err
			}
		}
		if len(vol) > 0 {
			if err := tx.PutVolatility(ctx, vol); err != nil {
				return err
			}
		}
		return tx.AppendOutbox(ctx, ev)
	})
	if err != nil {
		var ce *core.Error
		if errors.As(err, &ce) {
			return IngestResult{}, err
		}
		return IngestResult{}, internal("ingest write", err)
	}
	return IngestResult{Snapshot: snap, Diff: d, ChangeSet: cs, Lineage: lineage, Observation: obs, EventID: evID}, nil
}

// allFindings lists every finding of every artifact, in artifact order.
func allFindings(p *content.Processed) []content.SecretFinding {
	var out []content.SecretFinding
	for i := range p.Artifacts {
		out = append(out, p.Artifacts[i].Features.Findings...)
	}
	return out
}

// prevIfSameKey returns prev unless it is a lineage predecessor
// (volatility is learned per URL template, not across renames).
func prevIfSameKey(prev *content.Processed, lineage *content.LineageMatch) *content.Processed {
	if lineage != nil {
		return nil
	}
	return prev
}

// mergeHistory adds ids to a sorted history, bounded by maxHistory.
func mergeHistory(h, ids []string) []string {
	out := append(slices.Clone(h), ids...)
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > maxHistory {
		out = out[:maxHistory]
	}
	return out
}

// baselineArtifacts fills a baseline change set with every artifact
// diffed against the empty document, so the first version's lines are
// "added" hunks (search and "first appeared" read the diff history
// only), and its feature changes as pre-existing additions.
func baselineArtifacts(cs *content.ChangeSet, cur *content.Processed, opts content.DiffOptions) error {
	var changes []content.FeatureChange
	for i := range cur.Artifacts {
		a := &cur.Artifacts[i]
		e := emptyArtifact(a)
		d, err := content.DiffArtifacts(e, a, opts)
		if err != nil {
			return invalid("%v", err)
		}
		for j := range d.FeatureChanges {
			d.FeatureChanges[j].Newness = content.NewnessPreExisting
		}
		changes = append(changes, d.FeatureChanges...)
		cs.Artifacts = append(cs.Artifacts, d)
	}
	slices.SortStableFunc(changes, func(a, b content.FeatureChange) int { return strings.Compare(a.Feature.ID(), b.Feature.ID()) })
	cs.Changes = slices.CompactFunc(changes, func(a, b content.FeatureChange) bool { return a.Feature.ID() == b.Feature.ID() })
	return nil
}

// emptyArtifact is the empty version of a (no bytes, no features).
func emptyArtifact(a *content.Artifact) *content.Artifact {
	return &content.Artifact{DocID: a.DocID, Kind: a.Kind, BodySHA256: sha256Hex(nil), NormalizedSHA256: sha256Hex(nil),
		Normalized: content.Normalized{Kind: a.Kind}}
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// diffAlgorithm names the diff as "<policy>/<main artifact algorithm>".
func diffAlgorithm(p content.DiffPolicy, cs *content.ChangeSet) string {
	alg := "none"
	for _, a := range cs.Artifacts {
		if a.Algorithm != "" {
			alg = a.Algorithm
			break
		}
	}
	return p.String() + "/" + alg
}

func diffStats(cs *content.ChangeSet, lineage bool) DiffStats {
	st := DiffStats{Outcome: cs.Outcome, Class: cs.Class, Newness: cs.Newness, Cause: cs.Cause, Baseline: cs.Baseline,
		Score: cs.Score.Value, Artifacts: len(cs.Artifacts), Changes: len(cs.Changes), Summary: cs.Summary, Lineage: lineage}
	for _, a := range cs.Artifacts {
		st.Inserted += a.Inserted
		st.Deleted += a.Deleted
	}
	return st
}

// urlTemplate is the learned-volatility key of a URL (oam endpoint URL
// template rules; the URL itself when it does not normalize).
func urlTemplate(u string) string {
	if t, err := oam.NormalizeURLTemplate(u); err == nil {
		return t
	}
	return u
}
