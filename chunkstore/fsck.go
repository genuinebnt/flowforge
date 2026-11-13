package chunkstore

import (
	"cmp"
	"context"
	"errors"
	"slices"
)

// Severity grades an fsck problem.
type Severity uint8

// Severities.
const (
	// SeverityError: data is missing or damaged.
	SeverityError Severity = iota
	// SeverityWarning: harmless leftovers (orphans, duplicate copies)
	// that the next GC removes.
	SeverityWarning
)

// String returns "error" or "warning".
func (s Severity) String() string {
	if s == SeverityWarning {
		return "warning"
	}
	return "error"
}

// Problem is one fsck finding.
type Problem struct {
	Code     Code
	Severity Severity
	Object   string // backend object name
	Detail   string // chunk id or explanation
}

// FsckReport lists what fsck checked and found, problems sorted by
// (object, code, detail).
type FsckReport struct {
	Packs     int
	Chunks    int
	Manifests int
	Problems  []Problem
}

// OK reports whether no problem has error severity.
func (r *FsckReport) OK() bool {
	for _, p := range r.Problems {
		if p.Severity == SeverityError {
			return false
		}
	}
	return true
}

// Has reports whether some problem carries code.
func (r *FsckReport) Has(code Code) bool {
	return slices.ContainsFunc(r.Problems, func(p Problem) bool { return p.Code == code })
}

// add records err against object, keeping its code (CodeBackend for a
// foreign error).
func (r *FsckReport) add(sev Severity, object string, err error) {
	var e *Error
	if errors.As(err, &e) {
		detail := e.Detail
		if e.Object != "" && e.Object != object {
			detail = e.Object + ": " + detail
		}
		r.Problems = append(r.Problems, Problem{Code: e.Code, Severity: sev, Object: object, Detail: detail})
		return
	}
	r.Problems = append(r.Problems, Problem{Code: CodeBackend, Severity: sev, Object: object, Detail: err.Error()})
}

// Fsck verifies the namespace ns of b without trusting any in-memory
// state: the ref decodes; every pack it names has an index and a pack
// object; each decodes; the index matches the pack (pack id, size, footer
// CRC, one entry per record with equal lengths); the pack's SHA-256 equals
// its name; every chunk verifies (record CRC, decoding, SHA-256); objects
// under the namespace that the ref does not name are reported as orphans
// (warning); and each manifest in manifests is present, decodes, and
// references only present chunks whose lengths sum to its total.
//
// The error result is reserved for operational failures (context, an
// unreachable backend); damage is reported in the FsckReport.
func Fsck(ctx context.Context, b Backend, ns string, manifests []ManifestID) (FsckReport, error) {
	nsv, err := newNamespace(ns)
	if err != nil {
		return FsckReport{}, err
	}
	c, err := newCodec()
	if err != nil {
		return FsckReport{}, err
	}
	defer c.close()
	return fsck(ctx, b, nsv, c, manifests)
}

// Fsck runs Fsck on the store's namespace after sealing the open pack,
// holding the write lock.
func (s *Store) Fsck(ctx context.Context, manifests []ManifestID) (FsckReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return FsckReport{}, ErrClosed
	}
	if err := s.sealLocked(ctx); err != nil {
		return FsckReport{}, err
	}
	return fsck(ctx, s.b, s.ns, s.codec, manifests)
}

// fsckLoc is where fsck found a chunk first: the pack object name and the
// index entry.
type fsckLoc struct {
	pack string
	e    IndexEntry
}

// getOptional reads name, reporting a missing object as ok = false rather
// than an error.
func getOptional(ctx context.Context, b Backend, name string) ([]byte, bool, error) {
	data, err := b.Get(ctx, name)
	if errors.Is(err, ErrObjectNotFound) {
		return nil, false, nil
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, false, backendErr(name, err)
	}
	return data, true, nil
}

// fsck is Fsck over a validated namespace with the given codec.
func fsck(ctx context.Context, b Backend, ns namespace, c *codec, manifests []ManifestID) (FsckReport, error) {
	var rep FsckReport
	defer func() {
		slices.SortFunc(rep.Problems, func(x, y Problem) int {
			return cmp.Or(cmp.Compare(x.Object, y.Object), cmp.Compare(x.Code, y.Code), cmp.Compare(x.Detail, y.Detail))
		})
	}()
	ref := &Ref{}
	refData, ok, err := getOptional(ctx, b, ns.ref())
	if err != nil {
		return rep, err
	}
	if ok {
		if ref, err = DecodeRef(refData); err != nil {
			rep.add(SeverityError, ns.ref(), err)
			return rep, nil
		}
	}
	expected := map[string]struct{}{ns.ref(): {}}
	locs := map[ChunkID]fsckLoc{}
	packData := map[string][]byte{}
	for _, pid := range ref.Packs {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		rep.Packs++
		pn, in := ns.pack(pid), ns.index(pid)
		expected[pn], expected[in] = struct{}{}, struct{}{}
		idata, iok, err := getOptional(ctx, b, in)
		if err != nil {
			return rep, err
		}
		pdata, pok, err := getOptional(ctx, b, pn)
		if err != nil {
			return rep, err
		}
		var ix *Index
		if !iok {
			rep.add(SeverityError, in, errf(CodeIndexMissing, in, "index of a live pack is missing"))
		} else if ix, err = DecodeIndex(idata); err != nil {
			rep.add(SeverityError, in, err)
		}
		var pk *Pack
		if !pok {
			rep.add(SeverityError, pn, errf(CodePackMissing, pn, "live pack is missing"))
		} else if pk, err = DecodePack(pdata); err != nil {
			rep.add(SeverityError, pn, err)
		} else if PackID(SumChunk(pdata)) != pid {
			rep.add(SeverityError, pn, errf(CodePackChecksum, pn, "SHA-256 of the pack differs from its name"))
		}
		if ix == nil {
			continue
		}
		if ix.PackID != pid {
			rep.add(SeverityError, in, errf(CodeIndexPackMismatch, in, "index describes pack %s", ix.PackID))
		}
		if pk != nil {
			checkIndexAgainstPack(&rep, in, ix, pk)
		}
		packData[pn] = pdata
		for _, e := range ix.Entries {
			rep.Chunks++
			if prev, dup := locs[e.ID]; dup {
				rep.add(SeverityWarning, pn, errf(CodeDuplicateChunk, pn, "chunk %s also in %s", e.ID, prev.pack))
			} else {
				locs[e.ID] = fsckLoc{pack: pn, e: e}
			}
			if !pok {
				continue
			}
			if e.Offset+e.RecordSize() > uint64(len(pdata)) {
				rep.add(SeverityError, pn, errf(CodeRecordMalformed, e.ID.String(), "record beyond the end of the pack"))
				continue
			}
			if _, err := c.openRecord(e, pdata[e.Offset:e.Offset+e.RecordSize()]); err != nil {
				rep.add(SeverityError, pn, err)
			}
		}
	}

	names, err := b.List(ctx, ns.root())
	if err != nil {
		return rep, backendErr(ns.root(), err)
	}
	for _, n := range names {
		if _, ok := expected[n]; !ok {
			rep.add(SeverityWarning, n, errf(CodeOrphanObject, n, "not named by the ref"))
		}
	}

	for _, mid := range manifests {
		rep.Manifests++
		obj := mid.String()
		l, ok := locs[ChunkID(mid)]
		if !ok {
			rep.add(SeverityError, obj, errf(CodeManifestNotFound, obj, "not in any live pack"))
			continue
		}
		pdata := packData[l.pack]
		if pdata == nil || l.e.Offset+l.e.RecordSize() > uint64(len(pdata)) {
			continue // already reported
		}
		raw, err := c.openRecord(l.e, pdata[l.e.Offset:l.e.Offset+l.e.RecordSize()])
		if err != nil {
			continue // already reported against the pack
		}
		m, err := DecodeManifest(raw)
		if err != nil {
			rep.add(SeverityError, obj, err)
			continue
		}
		var sum uint64
		missing := false
		lens := make([]uint32, 0, len(m.Chunks))
		for _, cid := range m.Chunks {
			cl, ok := locs[cid]
			if !ok {
				rep.add(SeverityError, obj, errf(CodeChunkMissing, obj, "chunk %s", cid))
				missing = true
				continue
			}
			sum += uint64(cl.e.RawLen)
			lens = append(lens, cl.e.RawLen)
		}
		if !missing && sum != m.TotalLen {
			rep.add(SeverityError, obj, errf(CodeManifestLengthMismatch, obj, "chunks hold %d bytes, manifest says %d", sum, m.TotalLen))
		}
		if !missing && m.Chunking.Algo != 0 {
			if i, ok := chunkLengthsFit(m.Chunking, lens); !ok {
				rep.add(SeverityError, obj, errf(CodeManifestChunking, obj, "chunk %d has %d bytes, outside the bounds of %s %d/%d/%d",
					i, lens[i], m.Chunking.Algo, m.Chunking.MinSize, m.Chunking.AvgSize, m.Chunking.MaxSize))
			}
		}
	}
	return rep, nil
}

// checkIndexAgainstPack reports every disagreement between an index and
// its decoded pack: size, footer CRC, record count, and each entry's
// record (offset and lengths).
func checkIndexAgainstPack(rep *FsckReport, in string, ix *Index, pk *Pack) {
	if ix.PackSize != pk.Size || ix.PackCRC != pk.CRC {
		rep.add(SeverityError, in, errf(CodeIndexPackMismatch, in, "index says size %d crc %08x, pack is %d crc %08x", ix.PackSize, ix.PackCRC, pk.Size, pk.CRC))
	}
	if len(ix.Entries) != len(pk.Records) {
		rep.add(SeverityError, in, errf(CodeIndexPackMismatch, in, "%d entries for %d records", len(ix.Entries), len(pk.Records)))
	}
	byOff := make(map[uint64]PackRecord, len(pk.Records))
	for _, r := range pk.Records {
		byOff[r.Offset] = r
	}
	for _, e := range ix.Entries {
		r, ok := byOff[e.Offset]
		if !ok || r.RawLen != e.RawLen || r.CompLen != e.CompLen {
			rep.add(SeverityError, in, errf(CodeIndexPackMismatch, e.ID.String(), "entry does not match a record"))
		}
	}
}

// chunkLengthsFit checks chunk lengths against a recorded chunker's
// bounds (FORMAT.md §1): every chunk but the last in [min, max] (whole
// blob: exactly max), the last in [1, max]. It returns the first
// offending chunk. The index's raw lengths suffice, so fsck reads no
// chunk payload for it; re-cutting the bytes (CHUNK-INV-24) is the exact
// check.
func chunkLengthsFit(c ChunkerConfig, lens []uint32) (int, bool) {
	lo, hi := c.MinSize, c.MaxSize
	if c.Algo == ChunkWhole {
		lo = hi
	}
	for i, n := range lens {
		if int(n) > hi || (i < len(lens)-1 && int(n) < lo) {
			return i, false
		}
	}
	return 0, true
}
