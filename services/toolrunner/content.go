package toolrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"google.golang.org/grpc"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/core/netnorm"
	"github.com/genuinebnt/flowforge/engine"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
)

// The V2 content tools (V2_DESIGN §8): collect, crawl and wellknown fetch
// through the egress proxy and store every response with the content
// service (IngestCapture); extract, unpack_sourcemap, diff_latest and
// search_changes only read the content service. Their results are the DSL
// prelude records Snapshot, Features, ChangeSet and ChangeHit (dsl
// contentRecords), field for field.
//
// The graph learns about content from the content service: every accepted
// capture publishes one content.changes event carrying the capture's oam
// observation (URL, File, Endpoint, ContentProperty, HTTPHeaderProperty,
// redirects_to, ... with coverage-gated authority, V2 §3.4). The content
// tools therefore stream no observations of their own, so each fact has
// one writer and authority is applied once; the raw responses are kept by
// the content service in the tenant's chunk store (raw snapshots).

// ContentClient is the part of ContentService the content tools call
// (pb.ContentServiceClient implements it); the methods are the RPCs of
// the same names (spec 37.1).
type ContentClient interface {
	IngestCapture(ctx context.Context, in *pb.IngestCaptureRequest, opts ...grpc.CallOption) (*pb.IngestCaptureResponse, error)
	GetSnapshot(ctx context.Context, in *pb.GetSnapshotRequest, opts ...grpc.CallOption) (*pb.ContentSnapshot, error)
	ListSnapshots(ctx context.Context, in *pb.ListSnapshotsRequest, opts ...grpc.CallOption) (*pb.ListSnapshotsResponse, error)
	DiffSnapshots(ctx context.Context, in *pb.DiffSnapshotsRequest, opts ...grpc.CallOption) (*pb.SnapshotDiff, error)
	SearchChanges(ctx context.Context, in *pb.SearchChangesRequest, opts ...grpc.CallOption) (*pb.SearchChangesResponse, error)
}

// ContentMaxMessage is the gRPC message limit between the tool runner and
// the content service: a capture body may be content.MaxResponseBytes
// (8 MiB, V2 §12.10) plus its headers and framing, above gRPC's 4 MiB
// default.
const ContentMaxMessage = content.MaxResponseBytes + 4<<20

// ContentConfig configures the content tools.
type ContentConfig struct {
	// Content is the content service; nil makes every content tool fail
	// with no_content.
	Content ContentClient
	// Profiles are the named capture profiles (V2 §5) collect accepts, in
	// addition to "default" (content.DefaultProfile). crawl and wellknown
	// use "default".
	Profiles []content.Profile
	// MaxBody bounds the bytes read of one response (default and maximum
	// content.MaxResponseBytes); a longer body is cut and the capture is
	// partial.
	MaxBody int
	// MaxRedirects bounds the redirect hops followed per capture (default
	// 5, as the HTTP client of spec 4.5).
	MaxRedirects int
	// MaxResources bounds the subresources (scripts, styles, their source
	// maps) collect captures with one page (default 32; negative: none).
	MaxResources int
	// MaxBudget caps crawl's fetch budget (default 500).
	MaxBudget int
	// Revisit is crawl's revisit policy (default AdaptiveRevisit with its
	// defaults) and RevisitState where crawl keeps each URL's interval
	// (default a MemRevisitStore per adapter set).
	Revisit      RevisitPolicy
	RevisitState RevisitStore
	// Ports are the schemes and ports wellknown tries for a host (default
	// https:443 and http:80, as httpprobe).
	Ports []ProbePort
	// UserAgent is sent when a profile names none, and is the product
	// token crawl matches robots.txt groups with.
	UserAgent string
}

// DefaultUserAgent is the content tools' User-Agent; its product token
// ("flowforge") selects robots.txt groups (RFC 9309 §2.2.1).
const DefaultUserAgent = "flowforge/1.0 (+content collector)"

// contentTools is the shared state of one set of content adapters.
type contentTools struct {
	cfg      ContentConfig
	profiles map[string]content.Profile
}

// ContentAdapters returns the content tools sharing cfg: collect, crawl,
// wellknown, extract, unpack_sourcemap, diff_latest and search_changes.
// Invalid profiles are an error (they are configuration, not input).
func ContentAdapters(cfg ContentConfig) ([]Adapter, error) {
	if cfg.MaxBody <= 0 || cfg.MaxBody > content.MaxResponseBytes {
		cfg.MaxBody = content.MaxResponseBytes
	}
	if cfg.MaxRedirects <= 0 {
		cfg.MaxRedirects = 5
	}
	if cfg.MaxResources == 0 {
		cfg.MaxResources = 32
	}
	if cfg.MaxBudget <= 0 {
		cfg.MaxBudget = 500
	}
	if cfg.Revisit == nil {
		cfg.Revisit = AdaptiveRevisit{}
	}
	if cfg.RevisitState == nil {
		cfg.RevisitState = NewMemRevisitStore()
	}
	if len(cfg.Ports) == 0 {
		cfg.Ports = []ProbePort{{"https", 443}, {"http", 80}}
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = DefaultUserAgent
	}
	ct := &contentTools{cfg: cfg, profiles: map[string]content.Profile{}}
	def, _ := content.DefaultProfile().Normalize()
	ct.profiles[def.Name] = def
	for _, p := range cfg.Profiles {
		n, err := p.Normalize()
		if err != nil {
			return nil, fmt.Errorf("toolrunner: capture profile %q: %w", p.Name, err)
		}
		if n.CookieJarRef != "" {
			// Cookie jars are a sandbox feature (ROADMAP_V2 A1); a profile
			// that names one cannot be honoured here.
			return nil, fmt.Errorf("toolrunner: capture profile %q: cookie jars are not supported", p.Name)
		}
		ct.profiles[n.Name] = n
	}
	return []Adapter{&Collect{ct}, &Crawl{ct}, &WellKnown{ct}, &Extract{ct}, &UnpackSourcemap{ct}, &DiffLatest{ct}, &SearchChanges{ct}}, nil
}

// errNoContent is returned when the runner has no content service.
var errNoContent = &ToolError{Code: "no_content", Message: "the tool runner has no content service configured"}

// errSkipped reports a target the adapter skipped (already streamed as
// Skipped): denied by scope, refused by the proxy, or unreachable.
var errSkipped = errors.New("toolrunner: target skipped")

// contentCtx is ctx acting as the tool runner for tc's tenant: the content
// service authorizes ingest with runs:submit, which analysts have.
func contentCtx(ctx context.Context, tc *ToolCtx) context.Context {
	return platform.WithIdentity(ctx, platform.Identity{Tenant: tc.Tenant, Actor: platform.ServiceActor("tool-runner"), Role: auth.RoleAnalyst})
}

// contentError maps a content service error to a tool error.
func contentError(op string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	ce := platform.FromStatus(err)
	msg := op + ": " + ce.Error()
	switch core.KindOf(ce) {
	case core.KindInvalidArgument:
		return Errorf("invalid_input", false, msg)
	case core.KindNotFound:
		return Errorf("not_found", false, msg)
	case core.KindPermissionDenied, core.KindUnauthenticated:
		return Errorf("content_denied", false, msg)
	case core.KindResourceExhausted:
		return Errorf("quota_exceeded", true, msg)
	case core.KindFailedPrecondition:
		return Errorf("content_precondition", false, msg)
	}
	return Errorf("content_unavailable", true, msg)
}

// profile returns the named capture profile.
func (c *contentTools) profile(name string) (content.Profile, error) {
	p, ok := c.profiles[name]
	if !ok {
		names := make([]string, 0, len(c.profiles))
		for n := range c.profiles {
			names = append(names, n)
		}
		sort.Strings(names)
		return p, Errorf("invalid_input", false, fmt.Sprintf("unknown capture profile %q (configured: %s)", name, strings.Join(names, ", ")))
	}
	return p, nil
}

// canonicalURL is the netnorm form of an http(s) URL, the form scope
// decisions, the content store and the DSL Url type agree on.
func canonicalURL(raw string) (string, bool) {
	u, err := netnorm.ParseURL(raw)
	if err != nil {
		return "", false
	}
	return u.String(), true
}

// capture is one fetched and stored response.
type capture struct {
	snap      *pb.ContentSnapshot
	resp      *pb.IngestCaptureResponse
	body      []byte
	status    int
	redirects []string // each Location followed, in order
	// reused marks the open version this run already stored: nothing was
	// fetched.
	reused bool
}

// newVersion reports a capture that stored a new version (not a duplicate
// and not an unchanged re-fetch).
func (c *capture) newVersion() bool {
	return !c.reused && !c.resp.GetDuplicate() && !c.resp.GetUnchanged()
}

// changed reports a new version that is a change (structural or level 3),
// not a counted non-change such as masked_only or reordered (V2 §12.5).
func (c *capture) changed() bool {
	if !c.newVersion() {
		return false
	}
	switch c.resp.GetChange().GetOutcome() {
	case content.DiffChanged, content.DiffStructural:
		return true
	}
	return false
}

// fetchOpts are the options of one capture.
type fetchOpts struct {
	// page is the page that loaded a script or style (lineage).
	page string
	// probe stores only a 2xx final response: a missing well-known
	// resource is an answer, not content worth a version (the store would
	// keep a degraded catch-all 404 per run).
	probe bool
}

// allowed checks target in two steps: a passive check (no resolution)
// first, then the active check that resolves and pins the addresses the
// proxy may dial. Scope's active check resolves a name before it matches
// the allow rules (spec 4.3 step 7), so without the passive step every
// out-of-scope link a crawl meets would be looked up in its owner's DNS;
// with it only in-scope names are resolved (as D3's passive checks do).
func allowed(ctx context.Context, tc *ToolCtx, out Emitter, target string) (scope.Decision, bool, error) {
	if _, ok, err := tc.Allowed(ctx, out, target, scope.TargetURL, core.ClassPassive); err != nil || !ok {
		return scope.Decision{}, false, err
	}
	return tc.Allowed(ctx, out, target, scope.TargetURL, core.ClassActive)
}

// fetch captures rawURL with profile prof through the egress proxy and
// stores it with the content service. Every hop is scope-checked first
// (allowed) and fetched under its own pinned decision; a denied target or
// hop is streamed as Skipped and never contacted. It returns errSkipped
// when the target itself was not fetched, and a capture without snap for
// a probe that was not stored.
//
// A (URL, profile) is captured at most once per run: when this run
// already stored the open version (crawl and wellknown fetch what collect
// then asks for), that version is returned without a fetch. Two captures
// of one deploy would otherwise split a change across versions that
// differ only in per-response volatile values.
func (c *contentTools) fetch(ctx context.Context, tc *ToolCtx, out Emitter, rawURL string, prof content.Profile, o fetchOpts) (*capture, error) {
	if c.cfg.Content == nil {
		return nil, errNoContent
	}
	nw := tc.Network()
	if nw == nil {
		return nil, ErrNoEgress
	}
	requested, ok := canonicalURL(rawURL)
	if !ok {
		_ = out.Skipped(rawURL, "invalid_url")
		return nil, errSkipped
	}
	d, ok, err := allowed(ctx, tc, out, requested)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errSkipped
	}
	if got, err := c.capturedInRun(ctx, tc, requested, prof); err != nil || got != nil {
		return got, err
	}
	cur := requested
	var hops []*pb.RedirectHop
	redirects := []string{}
	for {
		resp, err := c.do(ctx, tc, nw, d, out, cur, prof)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			var re *RefusedError
			if errors.As(err, &re) {
				_ = out.Skipped(cur, "egress_refused:"+re.Reason)
				if re.RetryAfter > 0 {
					// Stop here: Execute reports the edge's delay
					// (ToolCtx.edgeOutcome) and the engine waits it out.
					return nil, &ToolError{Code: engine.CodeRateLimited, Message: re.Error(), Retryable: true, RetryAfter: re.RetryAfter}
				}
			} else {
				_ = out.Skipped(cur, "fetch_failed")
			}
			if len(hops) == 0 || o.probe {
				return nil, errSkipped
			}
			// A later hop failed: the capture is the redirect chain so far,
			// stored as failed so the store records the collection problem.
			return c.store(ctx, tc, requested, prof, o.page, &pb.Capture{Status: 0, Outcome: string(content.OutcomeFailed)}, hops, redirects, nil)
		}
		next, isRedirect := redirectTarget(cur, resp)
		if isRedirect && prof.FollowRedirects && len(hops) < c.cfg.MaxRedirects {
			_ = resp.Body.Close()
			nd, ok, err := allowed(ctx, tc, out, next)
			if err != nil {
				return nil, err
			}
			if ok {
				hops = append(hops, &pb.RedirectHop{Url: cur, Status: int32(resp.StatusCode), Headers: headersOf(resp.Header)})
				redirects = append(redirects, next)
				cur, d = next, nd
				continue
			}
			// The hop is out of scope: the redirect response itself is the
			// capture (its Location is a redirects_to fact, never fetched).
		}
		body, truncated, rerr := readBounded(resp.Body, c.cfg.MaxBody)
		_ = resp.Body.Close()
		if o.probe && (resp.StatusCode < 200 || resp.StatusCode > 299) {
			return &capture{status: resp.StatusCode, body: body, redirects: redirects}, nil
		}
		outcome := content.OutcomeComplete
		if truncated || rerr != nil {
			outcome = content.OutcomePartial
		}
		pc := &pb.Capture{Status: int32(resp.StatusCode), Headers: headersOf(resp.Header), Body: body, Truncated: truncated, Outcome: string(outcome)}
		return c.store(ctx, tc, requested, prof, o.page, pc, hops, redirects, body)
	}
}

// capturedInRun returns the open version of (u, prof) when the calling run
// stored it, with its (redacted) body, or nil.
func (c *contentTools) capturedInRun(ctx context.Context, tc *ToolCtx, u string, prof content.Profile) (*capture, error) {
	run := convert.ID(core.PrefixRun, tc.Run)
	if run == "" {
		return nil, nil
	}
	ls, err := c.cfg.Content.ListSnapshots(contentCtx(ctx, tc), &pb.ListSnapshotsRequest{ProgramId: convert.ID(core.PrefixProgram, tc.Program),
		Url: u, Profile: prof.Name, OpenOnly: true, Page: &pb.PageRequest{PageSize: 1}})
	if err != nil {
		return nil, contentError("list snapshots "+u, err)
	}
	if len(ls.GetSnapshots()) == 0 || ls.GetSnapshots()[0].GetRunId() != run {
		return nil, nil
	}
	s, err := c.cfg.Content.GetSnapshot(contentCtx(ctx, tc), &pb.GetSnapshotRequest{SnapshotId: ls.GetSnapshots()[0].GetId(), IncludeBody: true},
		grpc.MaxCallRecvMsgSize(ContentMaxMessage))
	if err != nil {
		return nil, contentError("get snapshot "+u, err)
	}
	return &capture{snap: s, resp: &pb.IngestCaptureResponse{Snapshot: s, Unchanged: true}, body: s.GetBody(), status: int(s.GetStatus()),
		redirects: redirectTargets(s), reused: true}, nil
}

// do sends one request of profile prof to target under decision d. The
// client never follows redirects itself: fetch re-checks every hop.
func (c *contentTools) do(ctx context.Context, tc *ToolCtx, nw Network, d scope.Decision, out Emitter, target string, prof content.Profile) (*http.Response, error) {
	client := nw.HTTPClient(tc, d, func(t, r string) { _ = out.Skipped(t, r) })
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, prof.Method, target, nil)
	if err != nil {
		return nil, err
	}
	for _, h := range prof.Headers {
		req.Header.Add(h.Name, h.Value)
	}
	ua := prof.UserAgent
	if ua == "" {
		ua = c.cfg.UserAgent
	}
	req.Header.Set("User-Agent", ua)
	if prof.Accept != "" {
		req.Header.Set("Accept", prof.Accept)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if r := resp.Header.Get(platform.HeaderRefused); r != "" {
		_ = resp.Body.Close()
		return nil, &RefusedError{Reason: r, Status: resp.Status}
	}
	return resp, nil
}

// store sends one capture to the content service.
func (c *contentTools) store(ctx context.Context, tc *ToolCtx, requested string, prof content.Profile, page string, pc *pb.Capture,
	hops []*pb.RedirectHop, redirects []string, body []byte) (*capture, error) {
	pc.Url, pc.Profile, pc.RedirectChain, pc.FetchedAt = requested, profileProto(prof), hops, convert.TS(tc.Now())
	req := &pb.IngestCaptureRequest{ProgramId: convert.ID(core.PrefixProgram, tc.Program), RunId: convert.ID(core.PrefixRun, tc.Run),
		EffectPath: tc.Path, PageUrl: page, Capture: pc}
	resp, err := c.cfg.Content.IngestCapture(contentCtx(ctx, tc), req, grpc.MaxCallSendMsgSize(ContentMaxMessage))
	if err != nil {
		return nil, contentError("ingest "+requested, err)
	}
	return &capture{snap: resp.GetSnapshot(), resp: resp, body: body, status: int(pc.GetStatus()), redirects: redirects}, nil
}

// redirectTarget returns the absolute, canonical Location of a 3xx
// response (RFC 9110 §15.4: relative references resolve against the
// request URL).
func redirectTarget(cur string, resp *http.Response) (string, bool) {
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", false
	}
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", false
	}
	base, err := url.Parse(cur)
	if err != nil {
		return "", false
	}
	ref, err := url.Parse(loc)
	if err != nil {
		return "", false
	}
	return canonicalURL(base.ResolveReference(ref).String())
}

// readBounded reads at most limit bytes of r and reports whether more
// followed.
func readBounded(r io.Reader, limit int) ([]byte, bool, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if len(b) > limit {
		return b[:limit], true, err
	}
	return b, false, err
}

// headersOf lists response headers sorted by canonical name, values in
// received order. PORT: Go's http.Header is a map and loses wire order;
// sorting makes the stored capture deterministic (the headers@1
// normalizer sorts again).
func headersOf(h http.Header) []*pb.HttpHeader {
	names := make([]string, 0, len(h))
	for k := range h {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []*pb.HttpHeader
	for _, k := range names {
		for _, v := range h[k] {
			out = append(out, &pb.HttpHeader{Name: k, Value: v})
		}
	}
	return out
}

func profileProto(p content.Profile) *pb.CaptureProfile {
	out := &pb.CaptureProfile{Name: p.Name, Method: p.Method, UserAgent: p.UserAgent, CookieJarRef: p.CookieJarRef,
		FollowRedirects: p.FollowRedirects, Accept: p.Accept}
	for _, h := range p.Headers {
		out.Headers = append(out.Headers, &pb.HttpHeader{Name: h.Name, Value: h.Value})
	}
	return out
}

// snapshotRecord is the prelude Snapshot record (dsl contentRecords).
type snapshotRecord struct {
	ID              string           `json:"id"`
	URL             string           `json:"url"`
	Profile         string           `json:"profile"`
	Status          int              `json:"status"`
	Mime            string           `json:"mime"`
	Kind            string           `json:"kind"`
	BodySHA256      string           `json:"body_sha256"`
	RawSize         int64            `json:"raw_size"`
	FetchedAt       string           `json:"fetched_at"`
	ParentID        *string          `json:"parent_id"`
	LineageParentID *string          `json:"lineage_parent_id"`
	Coverage        string           `json:"coverage"`
	Degraded        bool             `json:"degraded"`
	NewInRun        bool             `json:"new_in_run"`
	Redirects       []string         `json:"redirects"`
	Resources       []snapshotRecord `json:"resources"`
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// snapshotOf converts a stored snapshot. new_in_run compares the run that
// stored the version with the calling run, so a re-fetch of a version an
// earlier run stored is not new, while a version crawl stored earlier in
// the same run is.
func snapshotOf(s *pb.ContentSnapshot, tc *ToolCtx, redirects []string) snapshotRecord {
	run := convert.ID(core.PrefixRun, tc.Run)
	if redirects == nil {
		redirects = redirectTargets(s)
	}
	return snapshotRecord{ID: s.GetId(), URL: s.GetUrl(), Profile: s.GetProfile(), Status: int(s.GetStatus()), Mime: s.GetMime(),
		Kind: s.GetKind(), BodySHA256: s.GetBodySha256(), RawSize: s.GetRawSize(), FetchedAt: core.FormatTime(convert.Time(s.GetFetchedAt())),
		ParentID: optional(s.GetParentId()), LineageParentID: optional(s.GetLineageParentId()), Coverage: s.GetCoverage(),
		Degraded: s.GetDegraded(), NewInRun: run != "" && s.GetRunId() == run, Redirects: redirects, Resources: []snapshotRecord{}}
}

// redirectTargets lists the Location targets a stored capture followed:
// each next hop's URL, then the final URL.
func redirectTargets(s *pb.ContentSnapshot) []string {
	hs := s.GetRedirectChain()
	out := []string{}
	for i := range hs {
		if i+1 < len(hs) {
			out = append(out, hs[i+1].GetUrl())
		} else {
			out = append(out, finalURL(s))
		}
	}
	return out
}

// finalURL is the URL a stored capture's final response came from
// (content.Capture.FinalURL over the stored redirect chain).
func finalURL(s *pb.ContentSnapshot) string {
	c := content.Capture{URL: s.GetUrl()}
	for _, h := range s.GetRedirectChain() {
		hop := content.Hop{URL: h.GetUrl(), Status: int(h.GetStatus())}
		for _, x := range h.GetHeaders() {
			hop.Headers = append(hop.Headers, content.Header{Name: x.GetName(), Value: x.GetValue()})
		}
		c.RedirectChain = append(c.RedirectChain, hop)
	}
	return c.FinalURL()
}

// decodeSnapshotRef reads the id of a Snapshot record input, split into
// the stored snapshot id and a derived-document suffix ("#src/<path>").
func decodeSnapshotRef(input json.RawMessage) (id, suffix string, err error) {
	var s struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(input, &s); err != nil || s.ID == "" {
		return "", "", Errorf("invalid_input", false, "input must be a Snapshot record")
	}
	id, frag, _ := strings.Cut(s.ID, "#")
	if frag != "" {
		suffix = "#" + frag
		if !strings.HasPrefix(suffix, "#src/") {
			return "", "", Errorf("invalid_input", false, "unknown derived snapshot "+s.ID)
		}
	}
	return id, suffix, nil
}

// getSnapshot reads a snapshot of the calling program.
func (c *contentTools) getSnapshot(ctx context.Context, tc *ToolCtx, id string, features bool) (*pb.ContentSnapshot, error) {
	if c.cfg.Content == nil {
		return nil, errNoContent
	}
	s, err := c.cfg.Content.GetSnapshot(contentCtx(ctx, tc), &pb.GetSnapshotRequest{SnapshotId: id, IncludeFeatures: features})
	if err != nil {
		return nil, contentError("get snapshot "+id, err)
	}
	// Snapshot ids are tenant-scoped; a workflow reads only its own
	// program's content (the graph is per program, spec 5).
	if s.GetProgramId() != convert.ID(core.PrefixProgram, tc.Program) {
		return nil, Errorf("not_found", false, "snapshot "+id+" is not a snapshot of this program")
	}
	return s, nil
}

// features decodes a snapshot's features JSON (content.Processed.Features).
func features(s *pb.ContentSnapshot) ([]content.Feature, error) {
	if s.GetFeaturesJson() == "" {
		return nil, nil
	}
	var fs []content.Feature
	if err := json.Unmarshal([]byte(s.GetFeaturesJson()), &fs); err != nil {
		return nil, Errorf("invalid_content", false, "features of "+s.GetId()+": "+err.Error())
	}
	return fs, nil
}

// featureURL is the concrete URL a URL-valued feature names: Ref for a
// lineage-keyed script or style (its key has the build hash stripped),
// else the key.
func featureURL(f content.Feature) (string, bool) {
	u := f.Key
	if f.Ref != "" {
		u = f.Ref
	}
	return canonicalURL(u)
}

// appendUnique appends s unless present (lists stay small: features of
// one document).
func appendUnique(xs []string, s string) []string {
	if slices.Contains(xs, s) {
		return xs
	}
	return append(xs, s)
}
