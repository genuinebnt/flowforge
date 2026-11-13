// Package content is the content service (V2 design §6-§8, §11 items 13,
// 14 and 18): flowforge.v1.ContentService over a contentstore.Store
// (Postgres or memory repository, per-tenant chunk stores, per-(tenant,
// program) text indexes). Captures arrive through IngestCapture (the
// collect tool's sink); every accepted capture publishes one
// content.changes event through the content outbox, which the graph turns
// into observations and content_changed events and the notifier into
// alerts. Background loops relay the outbox, apply retention and GC, and
// (in batched mode) index pending diffs.
package content

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/contentstore"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// Options configure a Server.
type Options struct {
	Store  *contentstore.Store
	Logger *slog.Logger
}

// Server implements flowforge.v1.ContentService.
type Server struct {
	pb.UnimplementedContentServiceServer
	o Options
}

// NewServer returns a content service.
func NewServer(o Options) *Server {
	if o.Logger == nil {
		o.Logger = platform.DiscardLogger()
	}
	return &Server{o: o}
}

// Store returns the underlying store.
func (s *Server) Store() *contentstore.Store { return s.o.Store }

func identity(ctx context.Context, need auth.Permission) (platform.Identity, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return id, err
	}
	if need != "" && !auth.Can(id.Role, need) {
		return id, core.Errorf(core.KindPermissionDenied, core.ReasonForbiddenRole, "role %s lacks %s", id.Role, need)
	}
	return id, nil
}

// IngestCapture implements ContentService (runs:submit: analysts and the
// services acting for a run).
func (s *Server) IngestCapture(ctx context.Context, req *pb.IngestCaptureRequest) (*pb.IngestCaptureResponse, error) {
	id, err := identity(ctx, auth.PermRunsSubmit)
	if err != nil {
		return nil, err
	}
	prog, err := convert.ParseID(core.PrefixProgram, "program_id", req.GetProgramId())
	if err != nil {
		return nil, err
	}
	if _, err := convert.OptionalID(core.PrefixRun, "run_id", req.GetRunId()); err != nil {
		return nil, err
	}
	if _, err := convert.OptionalID(core.PrefixEntity, "entity_id", req.GetEntityId()); err != nil {
		return nil, err
	}
	c := req.GetCapture()
	if c == nil {
		return nil, convert.Invalid("capture", "capture is required")
	}
	res, err := s.o.Store.Ingest(ctx, id.Tenant, contentstore.IngestRequest{Program: prog, Capture: captureDomain(c),
		EntityID: req.GetEntityId(), RunID: req.GetRunId(), EffectPath: req.GetEffectPath(), PageURL: req.GetPageUrl()})
	if err != nil {
		return nil, err
	}
	out := &pb.IngestCaptureResponse{Snapshot: snapshotProto(res.Snapshot), Duplicate: res.Duplicate, Unchanged: res.Unchanged}
	if res.Diff != nil {
		out.DiffId = res.Diff.PublicID()
	}
	if res.ChangeSet != nil {
		out.Change = summaryProto(*res.ChangeSet)
	}
	if res.Lineage != nil {
		out.LineageParentUrl = res.Lineage.URL
	}
	if !res.EventID.IsZero() {
		out.EventId = res.EventID.String()
	}
	s.o.Logger.InfoContext(ctx, "content.ingested", "snapshot_id", res.Snapshot.PublicID(), "url", res.Snapshot.URL,
		"duplicate", res.Duplicate, "unchanged", res.Unchanged)
	return out, nil
}

// GetSnapshot implements ContentService. raw (secrets restored) needs
// programs:admin (admin and owner).
func (s *Server) GetSnapshot(ctx context.Context, req *pb.GetSnapshotRequest) (*pb.ContentSnapshot, error) {
	need := auth.PermRead
	if req.GetRaw() {
		need = auth.PermProgramsAdmin
	}
	id, err := identity(ctx, need)
	if err != nil {
		return nil, err
	}
	sid, err := convert.ParseID(contentstore.PrefixSnapshot, "snapshot_id", req.GetSnapshotId())
	if err != nil {
		return nil, err
	}
	c, err := s.o.Store.SnapshotContent(ctx, id.Tenant, sid, contentstore.ContentOptions{Body: req.GetIncludeBody(),
		Normalized: req.GetIncludeNormalized(), Features: req.GetIncludeFeatures(), Raw: req.GetRaw()})
	if err != nil {
		return nil, err
	}
	if req.GetRaw() {
		s.o.Logger.InfoContext(ctx, "content.raw_read", "snapshot_id", c.Snapshot.PublicID(), "actor", id.Actor)
	}
	return contentProto(c, req.GetRaw())
}

// ListSnapshots implements ContentService, newest first.
func (s *Server) ListSnapshots(ctx context.Context, req *pb.ListSnapshotsRequest) (*pb.ListSnapshotsResponse, error) {
	id, err := identity(ctx, auth.PermRead)
	if err != nil {
		return nil, err
	}
	prog, err := convert.ParseID(core.PrefixProgram, "program_id", req.GetProgramId())
	if err != nil {
		return nil, err
	}
	size := int(req.GetPage().GetPageSize())
	if size <= 0 || size > 1000 {
		size = 100
	}
	f := contentstore.SnapshotFilter{Program: prog, URL: req.GetUrl(), Profile: req.GetProfile(), OpenOnly: req.GetOpenOnly(), Limit: size + 1}
	if tok := req.GetPage().GetPageToken(); tok != "" {
		c, err := parseCursor(tok)
		if err != nil {
			return nil, err
		}
		f.Before = &c
	}
	ss, err := s.o.Store.ListSnapshots(ctx, id.Tenant, f)
	if err != nil {
		return nil, err
	}
	out := &pb.ListSnapshotsResponse{Page: &pb.PageResponse{}}
	if len(ss) > size {
		ss = ss[:size]
		last := ss[len(ss)-1]
		out.Page.NextPageToken = cursorToken(last)
	}
	for _, sn := range ss {
		out.Snapshots = append(out.Snapshots, snapshotProto(sn))
	}
	return out, nil
}

// cursorToken encodes a list position: "<valid_from unix ms>.<snapshot uuid>".
func cursorToken(s contentstore.Snapshot) string {
	return strings.Join([]string{itoa64(s.ValidFrom.UnixMilli()), s.ID.String()}, ".")
}

func parseCursor(tok string) (contentstore.Cursor, error) {
	ms, id, ok := strings.Cut(tok, ".")
	n, err := atoi64(ms)
	u, err2 := core.ParseUUID(id)
	if !ok || err != nil || err2 != nil {
		return contentstore.Cursor{}, convert.Invalid("page_token", "malformed page token")
	}
	return contentstore.Cursor{ValidFrom: time.UnixMilli(n).UTC(), ID: u}, nil
}

// DiffSnapshots implements ContentService: unified and/or side-by-side
// views of two snapshots, or of a snapshot against its stored predecessor
// (or the empty document for a baseline) when from is empty.
func (s *Server) DiffSnapshots(ctx context.Context, req *pb.DiffSnapshotsRequest) (*pb.SnapshotDiff, error) {
	id, err := identity(ctx, auth.PermRead)
	if err != nil {
		return nil, err
	}
	to, err := convert.ParseID(contentstore.PrefixSnapshot, "to_snapshot_id", req.GetToSnapshotId())
	if err != nil {
		return nil, err
	}
	from, err := convert.OptionalID(contentstore.PrefixSnapshot, "from_snapshot_id", req.GetFromSnapshotId())
	if err != nil {
		return nil, err
	}
	if from.IsZero() {
		snap, err := s.o.Store.GetSnapshot(ctx, id.Tenant, to)
		if err != nil {
			return nil, err
		}
		from = snap.LineageParentID
		if from.IsZero() {
			from = snap.ParentID
		}
	}
	var v contentstore.DiffView
	if from.IsZero() {
		v, err = s.o.Store.BaselineDiff(ctx, id.Tenant, to)
	} else {
		v, err = s.o.Store.Diff(ctx, id.Tenant, from, to)
	}
	if err != nil {
		return nil, err
	}
	return diffProto(v, req.GetFormat())
}

// SearchChanges implements ContentService (V2 §7 query language).
func (s *Server) SearchChanges(ctx context.Context, req *pb.SearchChangesRequest) (*pb.SearchChangesResponse, error) {
	id, err := identity(ctx, auth.PermRead)
	if err != nil {
		return nil, err
	}
	prog, err := convert.ParseID(core.PrefixProgram, "program_id", req.GetProgramId())
	if err != nil {
		return nil, err
	}
	limit := int(req.GetLimit())
	if limit <= 0 || limit > 1000 {
		limit = 20
	}
	res, err := s.o.Store.Search(ctx, id.Tenant, prog, req.GetQuery(), contentstore.SearchOptions{K: limit, IncludeVendor: req.GetIncludeVendor()})
	if err != nil {
		return nil, err
	}
	out := &pb.SearchChangesResponse{Total: int32(res.Total)}
	for _, h := range res.Hits {
		out.Hits = append(out.Hits, hitProto(h))
	}
	return out, nil
}

// FirstAppeared implements ContentService (V2 §12.14).
func (s *Server) FirstAppeared(ctx context.Context, req *pb.FirstAppearedRequest) (*pb.FirstAppearedResponse, error) {
	id, err := identity(ctx, auth.PermRead)
	if err != nil {
		return nil, err
	}
	prog, err := convert.ParseID(core.PrefixProgram, "program_id", req.GetProgramId())
	if err != nil {
		return nil, err
	}
	h, found, err := s.o.Store.FirstAppeared(ctx, id.Tenant, prog, req.GetText(), contentstore.SearchOptions{IncludeVendor: req.GetIncludeVendor()})
	if err != nil {
		return nil, err
	}
	out := &pb.FirstAppearedResponse{Found: found}
	if found {
		out.Hit = hitProto(h)
	}
	return out, nil
}

// PinSnapshot implements ContentService (alerts:write: reviewing a change).
func (s *Server) PinSnapshot(ctx context.Context, req *pb.PinSnapshotRequest) (*pb.ContentSnapshot, error) {
	return s.pin(ctx, req.GetSnapshotId(), true, req.GetReason())
}

// UnpinSnapshot implements ContentService.
func (s *Server) UnpinSnapshot(ctx context.Context, req *pb.UnpinSnapshotRequest) (*pb.ContentSnapshot, error) {
	return s.pin(ctx, req.GetSnapshotId(), false, "")
}

func (s *Server) pin(ctx context.Context, raw string, pin bool, reason string) (*pb.ContentSnapshot, error) {
	id, err := identity(ctx, auth.PermAlertsWrite)
	if err != nil {
		return nil, err
	}
	sid, err := convert.ParseID(contentstore.PrefixSnapshot, "snapshot_id", raw)
	if err != nil {
		return nil, err
	}
	var sn contentstore.Snapshot
	if pin {
		sn, err = s.o.Store.Pin(ctx, id.Tenant, sid, reason)
	} else {
		sn, err = s.o.Store.Unpin(ctx, id.Tenant, sid)
	}
	if err != nil {
		return nil, err
	}
	return snapshotProto(sn), nil
}

// GetQuotaStatus implements ContentService.
func (s *Server) GetQuotaStatus(ctx context.Context, _ *pb.GetQuotaStatusRequest) (*pb.QuotaStatus, error) {
	id, err := identity(ctx, auth.PermRead)
	if err != nil {
		return nil, err
	}
	q, err := s.o.Store.Quota(ctx, id.Tenant)
	if err != nil {
		return nil, err
	}
	return &pb.QuotaStatus{CapturesPerDay: q.Limits.CapturesPerDay, StoredBytesLimit: q.Limits.StoredBytes,
		IndexBytesLimit: q.Limits.IndexBytes, Day: q.Usage.Day, CapturesToday: q.Usage.Captures,
		StoredBytes: q.Usage.StoredBytes, IndexBytes: q.Usage.IndexBytes}, nil
}

// Sweep runs one background pass over every tenant: retention with chunk
// GC, then (batched mode) indexing of pending diffs. Errors of one tenant
// are logged and do not stop the others.
func (s *Server) Sweep(ctx context.Context, tenants func(context.Context) ([]core.UUID, error), index bool) {
	ts, err := tenants(ctx)
	if err != nil {
		s.o.Logger.WarnContext(ctx, "content.sweep_failed", "error", err.Error())
		return
	}
	for _, t := range ts {
		if ctx.Err() != nil {
			return
		}
		if rep, err := s.o.Store.ApplyRetention(ctx, t); err != nil {
			s.o.Logger.WarnContext(ctx, "content.retention_failed", "tenant_id", t.String(), "error", err.Error())
		} else if rep.Snapshots > 0 {
			s.o.Logger.InfoContext(ctx, "content.retention", "tenant_id", t.String(), "snapshots", rep.Snapshots, "diffs", rep.Diffs)
		}
		if index {
			s.IndexTenant(ctx, t)
		}
	}
}

// IndexTenant indexes every program of a tenant up to its latest diff.
func (s *Server) IndexTenant(ctx context.Context, t core.UUID) {
	progs, err := s.o.Store.Programs(ctx, t)
	if err != nil {
		s.o.Logger.WarnContext(ctx, "content.index_failed", "tenant_id", t.String(), "error", err.Error())
		return
	}
	for _, p := range progs {
		if _, err := s.o.Store.IndexPending(ctx, t, p); err != nil {
			s.o.Logger.WarnContext(ctx, "content.index_failed", "tenant_id", t.String(), "program_id", p.String(), "error", err.Error())
		}
	}
}
