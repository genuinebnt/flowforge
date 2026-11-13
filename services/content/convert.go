package content

import (
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/contentstore"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/diff"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/convert"
)

// Proto <-> domain conversions of the content service. They live here,
// not in services/internal/convert, because only this service and the
// gateway (through the proto) use them.

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

func atoi64(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }

func headersDomain(hs []*pb.HttpHeader) []content.Header {
	var out []content.Header
	for _, h := range hs {
		out = append(out, content.Header{Name: h.GetName(), Value: h.GetValue()})
	}
	return out
}

func headersProto(hs []content.Header) []*pb.HttpHeader {
	var out []*pb.HttpHeader
	for _, h := range hs {
		out = append(out, &pb.HttpHeader{Name: h.Name, Value: h.Value})
	}
	return out
}

func hopsProto(hs []content.Hop) []*pb.RedirectHop {
	var out []*pb.RedirectHop
	for _, h := range hs {
		out = append(out, &pb.RedirectHop{Url: h.URL, Status: int32(h.Status), Headers: headersProto(h.Headers)})
	}
	return out
}

// captureDomain converts a capture; an absent profile is the default one.
func captureDomain(c *pb.Capture) content.Capture {
	prof := content.DefaultProfile()
	if p := c.GetProfile(); p != nil {
		prof = content.Profile{Name: p.GetName(), Method: p.GetMethod(), Headers: headersDomain(p.GetHeaders()), UserAgent: p.GetUserAgent(),
			CookieJarRef: p.GetCookieJarRef(), FollowRedirects: p.GetFollowRedirects(), Accept: p.GetAccept()}
	}
	out := content.Capture{URL: c.GetUrl(), Profile: prof, Status: int(c.GetStatus()), Headers: headersDomain(c.GetHeaders()),
		Body: c.GetBody(), FetchedAt: convert.Time(c.GetFetchedAt()), Truncated: c.GetTruncated(), Outcome: content.Outcome(c.GetOutcome())}
	for _, h := range c.GetRedirectChain() {
		out.RedirectChain = append(out.RedirectChain, content.Hop{URL: h.GetUrl(), Status: int(h.GetStatus()), Headers: headersDomain(h.GetHeaders())})
	}
	return out
}

func snapshotProto(s contentstore.Snapshot) *pb.ContentSnapshot {
	return &pb.ContentSnapshot{Id: s.PublicID(), ProgramId: core.PublicID(core.PrefixProgram, s.Program), EntityId: s.EntityID, Url: s.URL,
		UrlTemplate: s.URLTemplate, Profile: s.Profile, RunId: s.RunID, EffectPath: s.EffectPath, FetchedAt: convert.TS(s.FetchedAt),
		LastSeen: convert.TS(s.LastSeen), ValidFrom: convert.TS(s.ValidFrom), ValidTo: convert.TS(s.ValidTo), Status: int32(s.Status),
		Mime: s.Mime, Kind: s.Kind, BodySha256: s.BodySHA256, RawSize: s.RawSize, StoredSize: s.StoredSize,
		NormalizerVersion: s.NormalizerVersion, ParentId: convert.ID(contentstore.PrefixSnapshot, s.ParentID),
		LineageParentId: convert.ID(contentstore.PrefixSnapshot, s.LineageParentID), Outcome: s.Outcome, Coverage: s.Coverage,
		Degraded: s.Degraded, Pinned: s.Pinned, PinReason: s.PinReason, CreatedAt: convert.TS(s.CreatedAt)}
}

func contentProto(c contentstore.SnapshotContent, raw bool) (*pb.ContentSnapshot, error) {
	out := snapshotProto(c.Snapshot)
	out.Body, out.BodyRaw, out.Normalized = c.Body, raw && c.Body != nil, string(c.Normalized)
	if c.Capture != nil {
		out.Headers, out.RedirectChain = headersProto(c.Capture.Headers), hopsProto(c.Capture.RedirectChain)
	}
	if c.Processed != nil {
		b, err := core.CanonicalJSON(c.Processed.Features())
		if err != nil {
			return nil, err
		}
		out.FeaturesJson = string(b)
	}
	for _, f := range c.Findings {
		out.Findings = append(out.Findings, &pb.SecretFindingInfo{RuleId: f.RuleID, Preview: f.Preview, KeyedHash: f.KeyedHash})
	}
	return out, nil
}

func changesProto(cs []content.FeatureChange) []*pb.FeatureChangeRef {
	var out []*pb.FeatureChangeRef
	for _, c := range cs {
		out = append(out, &pb.FeatureChangeRef{Op: c.Op, Kind: string(c.Feature.Kind), Key: c.Feature.Key, Newness: c.Newness, Value: c.Feature.Value})
	}
	return out
}

func summaryProto(cs content.ChangeSet) *pb.ChangeSummary {
	out := &pb.ChangeSummary{Outcome: cs.Outcome, Levels: cs.Levels, Class: cs.Class, Newness: cs.Newness, Cause: cs.Cause,
		Baseline: cs.Baseline, Score: cs.Score.Value, Changes: changesProto(cs.Changes), Alertable: contentstore.Alertable(cs)}
	for _, r := range cs.Score.Reasons {
		out.ScoreFeatures = append(out.ScoreFeatures, &pb.ContentScoreFeature{Name: r.Name, Weight: r.Weight})
	}
	if len(cs.Summary) > 0 {
		out.Summary = map[string]int32{}
		for k, v := range cs.Summary {
			out.Summary[k] = int32(v)
		}
	}
	return out
}

// unified renders hunks as unified-diff text.
func unified(hs []content.Hunk) string {
	var b strings.Builder
	for _, h := range hs {
		b.WriteString(h.Header + "\n")
		for _, l := range h.Lines {
			b.WriteString(l + "\n")
		}
	}
	return b.String()
}

func diffProto(v contentstore.DiffView, f pb.DiffFormat) (*pb.SnapshotDiff, error) {
	out := &pb.SnapshotDiff{ToSnapshotId: v.To.PublicID(), Change: summaryProto(v.ChangeSet)}
	if !v.From.ID.IsZero() {
		out.FromSnapshotId = v.From.PublicID()
	}
	if v.Stored != nil {
		out.DiffId = v.Stored.PublicID()
	}
	wantUnified := f != pb.DiffFormat_DIFF_FORMAT_SIDE_BY_SIDE
	wantSide := f != pb.DiffFormat_DIFF_FORMAT_UNIFIED
	for _, a := range v.ChangeSet.Artifacts {
		av := &pb.ArtifactDiffView{DocId: a.DocID, Kind: string(a.Kind), Outcome: a.Outcome, Levels: a.Levels, Algorithm: a.Algorithm,
			BoundLevel: a.BoundLevel, Fallback: a.Fallback, Inserted: int32(a.Inserted), Deleted: int32(a.Deleted),
			FeatureChanges: changesProto(a.FeatureChanges)}
		if wantUnified {
			av.Unified = unified(a.Hunks)
		}
		if wantSide {
			b, err := diff.MarshalSideBySide(a.SideBySide)
			if err != nil {
				return nil, err
			}
			av.SideBySideJson = string(b)
		}
		out.Artifacts = append(out.Artifacts, av)
	}
	return out, nil
}

func hitProto(h contentstore.ChangeHit) *pb.ChangeHit {
	return &pb.ChangeHit{Score: h.Score, At: convert.TS(h.At), DiffId: h.DiffID, SnapshotId: h.SnapshotID, FromSnapshotId: h.FromID,
		Url: h.URL, Doc: h.Doc, Header: h.Header, Kind: h.Kind, Side: h.Side, Feature: h.Feature, Mime: h.Mime, UrlTemplate: h.URLTemplate,
		Class: h.Class, Newness: h.Newness, Cause: h.Cause, Vendor: h.Vendor, Text: h.Text, DecodedFrom: h.DecodedFrom}
}
