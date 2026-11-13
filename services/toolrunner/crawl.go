package toolrunner

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/wellknown"
)

// Crawl is the active crawl tool (V2_DESIGN §8): `(InScope<Url>, Int) ->
// List<Url>`, the integer being the fetch budget. It walks a URL frontier
// breadth first from the root:
//
//   - every URL is canonicalized (core/netnorm) and scope-checked (class
//     active) before it enters the frontier; a denied URL is streamed as
//     Skipped and never contacted;
//   - robots.txt is honoured per origin (RFC 9309: fetched once per
//     origin and crawl, 2xx parsed, 4xx allows everything, 5xx or
//     unreachable disallows everything; groups chosen by UserAgent);
//   - a URL is fetched (and stored, profile "default") only when its
//     revisit time has come (RevisitPolicy, V2_DESIGN §11 item 8); a URL
//     that is not due is expanded from its stored open version;
//   - links, frames and sitemap entries of HTML and sitemap captures are
//     the next frontier.
//
// At most budget URLs are fetched (robots.txt fetches are not counted;
// they are bounded by the number of origins, at most budget). The output
// is every in-scope, robots-allowed URL the crawl reached, fetched or
// not, in breadth-first order.
type Crawl struct{ c *contentTools }

// maxFrontier bounds the URLs one crawl call keeps (and returns).
const maxFrontier = 5000

// Spec implements Adapter.
func (*Crawl) Spec() Spec {
	return Spec{Name: "crawl", Version: "1.0.0", Description: "Robots-aware, scope-checked crawl frontier with adaptive revisits",
		InputType: "(InScope<Url>, Int)", OutputType: "List<Url>", Observes: []string{oam.TypeURL},
		Class: core.ClassActive, RateRPS: 10, RateBurst: 20, Concurrency: 2, Timeout: 30 * time.Minute, Idempotent: true,
		Idempotency: engine.IdempotencyIdempotent, Retry: RetryContent}
}

// robotsRules is the robots.txt verdict for one origin: rules, or allow
// or disallow everything.
type robotsRules struct {
	r                 *wellknown.Robots
	allowAll, denyAll bool
}

func (r *robotsRules) allowed(agent string, u *url.URL) bool {
	switch {
	case r.denyAll:
		return false
	case r.allowAll || r.r == nil:
		return true
	}
	p := u.EscapedPath()
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return r.r.Allowed(agent, p)
}

// Run implements Adapter.
func (a *Crawl) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	args, err := decodeArgs(input, 2)
	if err != nil {
		return nil, err
	}
	root, err := decodeString(args[0])
	if err != nil {
		return nil, err
	}
	var budget int
	if err := json.Unmarshal(args[1], &budget); err != nil || budget < 1 {
		return nil, Errorf("invalid_input", false, "the crawl budget must be a positive integer")
	}
	budget = min(budget, a.c.cfg.MaxBudget)
	start, ok := canonicalURL(root)
	if !ok {
		return nil, Errorf("invalid_input", false, "crawl root "+root+" is not an http(s) URL")
	}
	if a.c.cfg.Content == nil {
		return nil, errNoContent
	}
	if tc.Network() == nil {
		return nil, ErrNoEgress
	}
	_, ok, err = allowed(ctx, tc, out, start)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, Errorf("not_captured", false, "crawl root "+start+" is out of scope")
	}
	cr := &crawler{a: a, tc: tc, out: out, prof: a.c.profiles["default"], robots: map[string]*robotsRules{},
		agent: robotsAgent(a.c.cfg.UserAgent), budget: budget}
	return cr.run(ctx, start)
}

type crawler struct {
	a      *Crawl
	tc     *ToolCtx
	out    Emitter
	prof   content.Profile
	robots map[string]*robotsRules
	agent  string
	budget int
}

func (cr *crawler) run(ctx context.Context, start string) (json.RawMessage, error) {
	frontier := []string{start}
	seen := map[string]bool{start: true}
	reached := []string{}
	fetched := 0
	for i := 0; i < len(frontier); i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		u := frontier[i]
		pu, err := url.Parse(u)
		if err != nil {
			continue
		}
		rules, err := cr.robotsFor(ctx, pu)
		if err != nil {
			return nil, err
		}
		if !rules.allowed(cr.agent, pu) {
			_ = cr.out.Skipped(u, "robots_disallowed")
			continue
		}
		if fetched >= cr.budget {
			// Discovered, in scope and allowed, but past the budget: part of
			// the frontier, not fetched in this call.
			reached = append(reached, u)
			continue
		}
		snap, didFetch, err := cr.visit(ctx, u)
		if didFetch {
			fetched++
		}
		if errors.Is(err, errSkipped) {
			continue
		}
		if err != nil {
			return nil, err
		}
		reached = append(reached, u)
		_ = cr.out.Progress(int64(fetched), int64(cr.budget), "crawling")
		if snap == nil || snap.GetDegraded() {
			continue
		}
		links, err := cr.links(ctx, snap.GetId())
		if err != nil {
			return nil, err
		}
		for _, l := range links {
			if seen[l] || len(frontier) >= maxFrontier {
				continue
			}
			seen[l] = true
			_, ok, err := allowed(ctx, cr.tc, cr.out, l)
			if err != nil {
				return nil, err
			}
			if ok {
				frontier = append(frontier, l)
			}
		}
	}
	_ = cr.out.Progress(int64(fetched), int64(cr.budget), "done")
	return json.Marshal(reached)
}

// visit returns the open version of u: fetched now when u is due (or
// has no stored version), else the stored one. didFetch reports a fetch.
func (cr *crawler) visit(ctx context.Context, u string) (snap *pb.ContentSnapshot, didFetch bool, err error) {
	c := cr.a.c
	key := cr.tc.Tenant.String() + "|" + cr.tc.Program.String() + "|" + cr.prof.Name + "|" + u
	now := cr.tc.Now()
	st, have := c.cfg.RevisitState.Get(key)
	if have && now.Before(st.Due) {
		if s, err := cr.openVersion(ctx, u); err != nil || s != nil {
			return s, false, err
		}
	}
	got, err := c.fetch(ctx, cr.tc, cr.out, u, cr.prof, fetchOpts{})
	if err != nil {
		return nil, !errors.Is(err, errSkipped), err
	}
	if got.reused {
		return got.snap, false, nil
	}
	c.cfg.RevisitState.Put(key, st.Visit(c.cfg.Revisit, now, got.changed()))
	return got.snap, true, nil
}

// openVersion returns the stored open version of (u, profile), or nil.
func (cr *crawler) openVersion(ctx context.Context, u string) (*pb.ContentSnapshot, error) {
	ls, err := cr.a.c.cfg.Content.ListSnapshots(contentCtx(ctx, cr.tc), &pb.ListSnapshotsRequest{
		ProgramId: convert.ID(core.PrefixProgram, cr.tc.Program), Url: u, Profile: cr.prof.Name, OpenOnly: true,
		Page: &pb.PageRequest{PageSize: 1}})
	if err != nil {
		return nil, contentError("list snapshots "+u, err)
	}
	if len(ls.GetSnapshots()) == 0 {
		return nil, nil
	}
	return ls.GetSnapshots()[0], nil
}

// links returns the next frontier of a capture: for a redirected one the
// redirect's target only, else the canonical http(s) URLs an HTML or
// sitemap capture links to (anchors and <link> references, frames,
// sitemap entries). The target of a redirect is crawled as a URL of its
// own: it is another (URL, profile) key, and its links are read from its
// own capture when the crawl reaches it. (Content resolves a redirected
// capture's references against the final URL since TestRegress_RedirectBase,
// so the requested URL is no longer a wrong base.)
func (cr *crawler) links(ctx context.Context, id string) ([]string, error) {
	s, err := cr.a.c.getSnapshot(ctx, cr.tc, id, true)
	if err != nil {
		return nil, err
	}
	if len(s.GetRedirectChain()) > 0 {
		if u, ok := canonicalURL(finalURL(s)); ok {
			return []string{u}, nil
		}
		return nil, nil
	}
	switch content.Kind(s.GetKind()) {
	case content.KindHTML, content.KindSitemap, content.KindXML:
	default:
		return nil, nil
	}
	fs, err := features(s)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, f := range fs {
		switch f.Kind {
		case content.FeatLink, content.FeatFrame, content.FeatSitemapEntry:
			if u, ok := featureURL(f); ok {
				out = appendUnique(out, u)
			}
		}
	}
	return out, nil
}

// robotsFor returns the robots.txt verdict of u's origin, fetching it on
// first use (RFC 9309 §2.3: one robots.txt per scheme, host and port).
func (cr *crawler) robotsFor(ctx context.Context, u *url.URL) (*robotsRules, error) {
	origin := u.Scheme + "://" + u.Host
	if r, ok := cr.robots[origin]; ok {
		return r, nil
	}
	if len(cr.robots) >= cr.budget {
		// More origins than the budget: unread robots.txt counts as
		// disallowing (RFC 9309 §2.3.1.4 errs the same way).
		return &robotsRules{denyAll: true}, nil
	}
	r := &robotsRules{}
	got, err := cr.a.c.fetch(ctx, cr.tc, cr.out, origin+"/robots.txt", cr.prof, fetchOpts{})
	switch {
	case errors.Is(err, errSkipped):
		// Unreachable, refused or out of scope: RFC 9309 §2.3.1.4 has
		// the crawler assume complete disallow.
		r.denyAll = true
	case err != nil:
		return nil, err
	case got.status >= 200 && got.status < 300:
		r.r = wellknown.ParseRobots(got.body, wellknown.RobotsOptions{})
	case got.status >= 400 && got.status < 500:
		// §2.3.1.3: "unavailable" — the crawler may access any resource.
		r.allowAll = true
	default:
		// §2.3.1.4: server errors (and an unfollowed redirect) disallow.
		r.denyAll = true
	}
	cr.robots[origin] = r
	return r, nil
}

// robotsAgent is the product token of a User-Agent (RFC 9309 §2.2.1:
// letters, "_" and "-" only), "flowforge" for the default agent.
func robotsAgent(ua string) string {
	end := 0
	for end < len(ua) {
		c := ua[end]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-') {
			break
		}
		end++
	}
	if end == 0 {
		return "*"
	}
	return ua[:end]
}
