package gateway

import (
	"encoding/json"
	"net/http"
	"strings"

	"google.golang.org/grpc"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
)

// Content routes (V2 design §7, §11 items 13, 14 and 18): snapshots,
// their diffs (unified and side-by-side JSON) and change search, served
// by the content service. Without a content backend they answer 503.

const prefixSnapshot core.Prefix = "snp"

// contentMaxMessage is the receive limit for content-service replies that
// can carry a capture body or a diff of one: up to content.MaxResponseBytes
// (8 MiB, V2 §12.10) plus headers and framing, above gRPC's 4 MiB default.
// The content service already accepts messages this large.
const contentMaxMessage = content.MaxResponseBytes + 4<<20

func (s *Server) content() (pb.ContentServiceClient, error) {
	if s.cfg.Backends.Content == nil {
		return nil, newError(http.StatusServiceUnavailable, core.ReasonUnavailable, "the content service is not configured")
	}
	return s.cfg.Backends.Content, nil
}

func headersJSON(hs []*pb.HttpHeader) []map[string]any {
	out := []map[string]any{}
	for _, h := range hs {
		out = append(out, map[string]any{"name": h.GetName(), "value": h.GetValue()})
	}
	return out
}

func contentSnapshotJSON(sn *pb.ContentSnapshot) map[string]any {
	m := map[string]any{"id": sn.GetId(), "program_id": sn.GetProgramId(), "entity_id": nonEmpty(sn.GetEntityId()), "url": sn.GetUrl(),
		"url_template": sn.GetUrlTemplate(), "profile": sn.GetProfile(), "run_id": nonEmpty(sn.GetRunId()),
		"fetched_at": ts(sn.GetFetchedAt()), "last_seen": ts(sn.GetLastSeen()), "valid_from": ts(sn.GetValidFrom()),
		"valid_to": ts(sn.GetValidTo()), "status": sn.GetStatus(), "mime": sn.GetMime(), "kind": sn.GetKind(),
		"body_sha256": sn.GetBodySha256(), "raw_size": sn.GetRawSize(), "stored_size": sn.GetStoredSize(),
		"normalizer_version": sn.GetNormalizerVersion(), "parent_id": nonEmpty(sn.GetParentId()),
		"lineage_parent_id": nonEmpty(sn.GetLineageParentId()), "outcome": sn.GetOutcome(), "coverage": sn.GetCoverage(),
		"degraded": sn.GetDegraded(), "pinned": sn.GetPinned(), "pin_reason": nonEmpty(sn.GetPinReason()), "created_at": ts(sn.GetCreatedAt())}
	if sn.GetBody() != nil {
		m["body"], m["body_raw"] = string(sn.GetBody()), sn.GetBodyRaw()
	}
	if sn.GetNormalized() != "" {
		m["normalized"] = sn.GetNormalized()
	}
	if len(sn.GetHeaders()) > 0 {
		m["headers"] = headersJSON(sn.GetHeaders())
	}
	if f := sn.GetFeaturesJson(); f != "" {
		m["features"] = json.RawMessage(f)
	}
	if fs := sn.GetFindings(); len(fs) > 0 {
		var out []map[string]any
		for _, f := range fs {
			out = append(out, map[string]any{"rule_id": f.GetRuleId(), "preview": f.GetPreview(), "keyed_hash": f.GetKeyedHash()})
		}
		m["findings"] = out
	}
	return m
}

func changeRefsJSON(cs []*pb.FeatureChangeRef) []map[string]any {
	out := []map[string]any{}
	for _, c := range cs {
		out = append(out, map[string]any{"op": c.GetOp(), "kind": c.GetKind(), "key": c.GetKey(), "newness": nonEmpty(c.GetNewness()),
			"value": nonEmpty(c.GetValue())})
	}
	return out
}

func changeSummaryJSON(c *pb.ChangeSummary) map[string]any {
	if c == nil {
		return nil
	}
	feats := []map[string]any{}
	for _, f := range c.GetScoreFeatures() {
		feats = append(feats, map[string]any{"name": f.GetName(), "weight": f.GetWeight()})
	}
	return map[string]any{"outcome": c.GetOutcome(), "levels": strs(c.GetLevels()), "class": nonEmpty(c.GetClass()),
		"newness": nonEmpty(c.GetNewness()), "cause": c.GetCause(), "baseline": c.GetBaseline(), "score": c.GetScore(),
		"score_features": feats, "changes": changeRefsJSON(c.GetChanges()), "summary": c.GetSummary()}
}

func hitJSON(h *pb.ChangeHit) map[string]any {
	return map[string]any{"score": h.GetScore(), "at": ts(h.GetAt()), "diff_id": nonEmpty(h.GetDiffId()), "snapshot_id": h.GetSnapshotId(),
		"from_snapshot_id": nonEmpty(h.GetFromSnapshotId()), "url": h.GetUrl(), "doc": nonEmpty(h.GetDoc()), "header": nonEmpty(h.GetHeader()),
		"kind": h.GetKind(), "side": nonEmpty(h.GetSide()), "feature": nonEmpty(h.GetFeature()), "mime": h.GetMime(),
		"url_template": nonEmpty(h.GetUrlTemplate()), "class": nonEmpty(h.GetClass()), "newness": nonEmpty(h.GetNewness()),
		"cause": nonEmpty(h.GetCause()), "vendor": h.GetVendor(), "text": h.GetText(), "decoded_from": nonEmpty(h.GetDecodedFrom())}
}

// GET /v1/snapshots/{id}?include=body,normalized,features&raw=true
func (s *Server) getSnapshot(c *call) error {
	cl, err := s.content()
	if err != nil {
		return err
	}
	id, err := c.pathID("id", prefixSnapshot)
	if err != nil {
		return err
	}
	raw, err := c.queryBool("raw")
	if err != nil {
		return err
	}
	req := &pb.GetSnapshotRequest{SnapshotId: id, Raw: raw, IncludeBody: raw}
	for _, inc := range c.queryList("include") {
		switch inc {
		case "body":
			req.IncludeBody = true
		case "normalized":
			req.IncludeNormalized = true
		case "features":
			req.IncludeFeatures = true
		default:
			return errValidation("include: unknown value %q (body, normalized, features)", inc)
		}
	}
	sn, err := cl.GetSnapshot(c.ctx, req, grpc.MaxCallRecvMsgSize(contentMaxMessage))
	if err != nil {
		return err
	}
	return c.ok(contentSnapshotJSON(sn))
}

// GET /v1/snapshots?program_id=&url=&profile=&open_only=
func (s *Server) listSnapshots(c *call) error {
	cl, err := s.content()
	if err != nil {
		return err
	}
	pid, err := c.programParam(true)
	if err != nil {
		return err
	}
	size, tok, err := c.page()
	if err != nil {
		return err
	}
	open, err := c.queryBool("open_only")
	if err != nil {
		return err
	}
	q := c.r.URL.Query()
	res, err := cl.ListSnapshots(c.ctx, &pb.ListSnapshotsRequest{ProgramId: pid, Url: q.Get("url"), Profile: q.Get("profile"), OpenOnly: open,
		Page: &pb.PageRequest{PageSize: size, PageToken: tok}})
	if err != nil {
		return err
	}
	var out []map[string]any
	for _, sn := range res.GetSnapshots() {
		out = append(out, contentSnapshotJSON(sn))
	}
	return c.ok(listOf(out, res.GetPage().GetNextPageToken()))
}

// GET /v1/snapshots/{a}/diff/{b}?format=unified|side_by_side|both ({a}
// may be "previous": the stored predecessor of {b}).
func (s *Server) diffSnapshots(c *call) error {
	cl, err := s.content()
	if err != nil {
		return err
	}
	to, err := c.pathID("b", prefixSnapshot)
	if err != nil {
		return err
	}
	from := ""
	if a := c.r.PathValue("a"); a != "previous" {
		if from, err = c.pathID("a", prefixSnapshot); err != nil {
			return err
		}
	}
	format := pb.DiffFormat_DIFF_FORMAT_BOTH
	switch f := c.r.URL.Query().Get("format"); f {
	case "", "both":
	case "unified":
		format = pb.DiffFormat_DIFF_FORMAT_UNIFIED
	case "side_by_side", "side-by-side":
		format = pb.DiffFormat_DIFF_FORMAT_SIDE_BY_SIDE
	default:
		return errValidation("format: unified, side_by_side or both")
	}
	d, err := cl.DiffSnapshots(c.ctx, &pb.DiffSnapshotsRequest{FromSnapshotId: from, ToSnapshotId: to, Format: format}, grpc.MaxCallRecvMsgSize(contentMaxMessage))
	if err != nil {
		return err
	}
	arts := []map[string]any{}
	for _, a := range d.GetArtifacts() {
		m := map[string]any{"doc_id": a.GetDocId(), "kind": a.GetKind(), "outcome": a.GetOutcome(), "levels": strs(a.GetLevels()),
			"algorithm": nonEmpty(a.GetAlgorithm()), "bound_level": nonEmpty(a.GetBoundLevel()), "fallback": nonEmpty(a.GetFallback()),
			"inserted": a.GetInserted(), "deleted": a.GetDeleted(), "feature_changes": changeRefsJSON(a.GetFeatureChanges())}
		if format != pb.DiffFormat_DIFF_FORMAT_SIDE_BY_SIDE {
			m["unified"] = a.GetUnified()
		}
		if format != pb.DiffFormat_DIFF_FORMAT_UNIFIED && a.GetSideBySideJson() != "" {
			m["side_by_side"] = json.RawMessage(a.GetSideBySideJson())
		}
		arts = append(arts, m)
	}
	return c.ok(map[string]any{"from_snapshot_id": nonEmpty(d.GetFromSnapshotId()), "to_snapshot_id": d.GetToSnapshotId(),
		"diff_id": nonEmpty(d.GetDiffId()), "format": strings.ToLower(strings.TrimPrefix(format.String(), "DIFF_FORMAT_")),
		"change": changeSummaryJSON(d.GetChange()), "artifacts": arts})
}

// GET /v1/changes/search?program_id=&q=&limit=&include_vendor=
func (s *Server) searchChanges(c *call) error {
	cl, err := s.content()
	if err != nil {
		return err
	}
	pid, err := c.programParam(true)
	if err != nil {
		return err
	}
	q := c.r.URL.Query().Get("q")
	if q == "" {
		return errValidation("q: required")
	}
	limit, err := c.queryInt("limit", 20, 1, 1000)
	if err != nil {
		return err
	}
	vendor, err := c.queryBool("include_vendor")
	if err != nil {
		return err
	}
	res, err := cl.SearchChanges(c.ctx, &pb.SearchChangesRequest{ProgramId: pid, Query: q, Limit: int32(limit), IncludeVendor: vendor})
	if err != nil {
		return err
	}
	hits := []map[string]any{}
	for _, h := range res.GetHits() {
		hits = append(hits, hitJSON(h))
	}
	return c.ok(map[string]any{"hits": hits, "total": res.GetTotal()})
}

// GET /v1/changes/first-appeared?program_id=&text=
func (s *Server) firstAppeared(c *call) error {
	cl, err := s.content()
	if err != nil {
		return err
	}
	pid, err := c.programParam(true)
	if err != nil {
		return err
	}
	vendor, err := c.queryBool("include_vendor")
	if err != nil {
		return err
	}
	res, err := cl.FirstAppeared(c.ctx, &pb.FirstAppearedRequest{ProgramId: pid, Text: c.r.URL.Query().Get("text"), IncludeVendor: vendor})
	if err != nil {
		return err
	}
	if !res.GetFound() {
		return errNotFound()
	}
	return c.ok(hitJSON(res.GetHit()))
}

// POST /v1/snapshots/{id}/:pin {"reason": ...} and /:unpin
func (s *Server) pinSnapshot(c *call) error   { return s.setPin(c, true) }
func (s *Server) unpinSnapshot(c *call) error { return s.setPin(c, false) }

func (s *Server) setPin(c *call, pin bool) error {
	cl, err := s.content()
	if err != nil {
		return err
	}
	id, err := c.pathID("id", prefixSnapshot)
	if err != nil {
		return err
	}
	var in struct {
		Reason string `json:"reason"`
	}
	if err := c.decodeOptional(&in); err != nil {
		return err
	}
	var sn *pb.ContentSnapshot
	if pin {
		sn, err = cl.PinSnapshot(c.ctx, &pb.PinSnapshotRequest{SnapshotId: id, Reason: in.Reason})
	} else {
		sn, err = cl.UnpinSnapshot(c.ctx, &pb.UnpinSnapshotRequest{SnapshotId: id})
	}
	if err != nil {
		return err
	}
	return c.ok(contentSnapshotJSON(sn))
}

// GET /v1/content/quota
func (s *Server) contentQuota(c *call) error {
	cl, err := s.content()
	if err != nil {
		return err
	}
	q, err := cl.GetQuotaStatus(c.ctx, &pb.GetQuotaStatusRequest{})
	if err != nil {
		return err
	}
	return c.ok(map[string]any{"limits": map[string]any{"captures_per_day": q.GetCapturesPerDay(), "stored_bytes": q.GetStoredBytesLimit(),
		"index_bytes": q.GetIndexBytesLimit()}, "usage": map[string]any{"day": q.GetDay(), "captures": q.GetCapturesToday(),
		"stored_bytes": q.GetStoredBytes(), "index_bytes": q.GetIndexBytes()}})
}
