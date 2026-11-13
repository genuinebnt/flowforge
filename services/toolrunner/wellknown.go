package toolrunner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/wellknown"
)

// WellKnown is the active wellknown tool (V2_DESIGN §8): `(InScope<Host>)
// -> List<Url>`. For each configured scheme and port (Ports) it fetches
// and stores robots.txt (RFC 9309), security.txt (RFC 9116, at
// /.well-known/ and then the legacy root path), the sitemaps robots.txt
// names (else /sitemap.xml; the sitemaps a sitemap index lists are read
// too, at any depth, maxSitemaps in all, spec 36.1), OpenAPI/Swagger
// documents at their conventional paths and a
// GraphQL endpoint (a `{__typename}` query by GET, which every GraphQL
// server answers with a JSON data or errors member). A resource counts as
// found only when it answered 2xx and its sniffed kind is the expected one
// (an OpenAPI document must also yield a schema endpoint; GraphQL accepts
// 200 or 400 with a GraphQL response body), so a catch-all route proves
// nothing. Every URL is scope-checked before it
// is fetched; an origin whose robots.txt cannot be reached is skipped.
//
// The output is the found resources' URLs followed by the in-scope
// sitemap entries, without duplicates, in discovery order.
type WellKnown struct{ c *contentTools }

// maxSitemaps bounds the sitemaps wellknown reads per origin.
const maxSitemaps = 10

// OpenAPIPaths are the conventional OpenAPI and Swagger document paths
// wellknown tries.
var OpenAPIPaths = []string{"/openapi.json", "/swagger.json", "/v3/api-docs", "/api-docs", "/.well-known/openapi.json"}

// Spec implements Adapter.
func (*WellKnown) Spec() Spec {
	return Spec{Name: "wellknown", Version: "1.0.0", Description: "robots.txt, security.txt, sitemaps, OpenAPI and GraphQL discovery",
		InputType: "InScope<Host>", OutputType: "List<Url>", Observes: []string{oam.TypeURL, oam.TypeEndpoint},
		Class: core.ClassActive, RateRPS: 10, RateBurst: 20, Concurrency: 4, Timeout: 10 * time.Minute, Idempotent: true,
		Idempotency: engine.IdempotencyIdempotent, Retry: RetryContent}
}

// Run implements Adapter.
func (a *WellKnown) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	host, err := decodeHost(input)
	if err != nil {
		return nil, err
	}
	if a.c.cfg.Content == nil {
		return nil, errNoContent
	}
	if tc.Network() == nil {
		return nil, ErrNoEgress
	}
	found := []string{}
	add := func(u string) { found = appendUnique(found, u) }
	prof := a.c.profiles["default"]
	for i, pp := range a.c.cfg.Ports {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		_ = out.Progress(int64(i), int64(len(a.c.cfg.Ports)), "discovering")
		origin, ok := canonicalURL(fmt.Sprintf("%s://%s/", pp.Scheme, net.JoinHostPort(host, strconv.Itoa(pp.Port))))
		if !ok {
			return nil, Errorf("invalid_input", false, "host "+host+" does not form a URL")
		}
		origin = origin[:len(origin)-1] // without the trailing "/"
		if err := a.origin(ctx, tc, out, prof, origin, add); err != nil {
			return nil, err
		}
	}
	_ = out.Progress(int64(len(a.c.cfg.Ports)), int64(len(a.c.cfg.Ports)), "done")
	return json.Marshal(found)
}

// origin discovers the well-known resources of one origin.
func (a *WellKnown) origin(ctx context.Context, tc *ToolCtx, out Emitter, prof content.Profile, origin string, add func(string)) error {
	// robots.txt is stored whatever it answers (a 4xx means "allow
	// everything", RFC 9309 §2.3.1.3); the other resources are probes.
	get := func(path string) (*capture, error) {
		got, err := a.c.fetch(ctx, tc, out, origin+path, prof, fetchOpts{probe: path != "/robots.txt"})
		if errors.Is(err, errSkipped) {
			return nil, nil
		}
		return got, err
	}
	ok := func(c *capture, kinds ...content.Kind) bool {
		if c == nil || c.snap == nil || c.status < 200 || c.status > 299 || c.snap.GetDegraded() {
			return false
		}
		for _, k := range kinds {
			if c.snap.GetKind() == string(k) {
				return true
			}
		}
		return false
	}
	robots, err := get("/robots.txt")
	if err != nil {
		return err
	}
	if robots == nil {
		// Nothing answered (or the origin is out of scope): no well-known
		// resource can be read here.
		return nil
	}
	var sitemaps []string
	if ok(robots, content.KindRobots, content.KindText) {
		add(origin + "/robots.txt")
		for _, s := range wellknown.ParseRobots(robots.body, wellknown.RobotsOptions{}).Sitemaps {
			if u, ok := canonicalURL(s); ok {
				sitemaps = appendUnique(sitemaps, u)
			}
		}
	}
	if len(sitemaps) == 0 {
		sitemaps = []string{origin + "/sitemap.xml"}
	}
	for _, p := range []string{"/.well-known/security.txt", "/security.txt"} {
		c, err := get(p)
		if err != nil {
			return err
		}
		if ok(c, content.KindSecurityTxt) {
			add(origin + p)
			break
		}
	}
	var entries []string
	for i := 0; i < len(sitemaps) && i < maxSitemaps; i++ {
		c, err := a.c.fetch(ctx, tc, out, sitemaps[i], prof, fetchOpts{probe: true})
		if errors.Is(err, errSkipped) {
			continue
		}
		if err != nil {
			return err
		}
		if !ok(c, content.KindSitemap, content.KindXML) {
			continue
		}
		add(sitemaps[i])
		s, err := a.c.getSnapshot(ctx, tc, c.snap.GetId(), true)
		if err != nil {
			return err
		}
		fs, err := features(s)
		if err != nil {
			return err
		}
		for _, f := range fs {
			if f.Kind != content.FeatSitemapEntry {
				continue
			}
			u, okURL := featureURL(f)
			if !okURL {
				continue
			}
			if f.Attr("kind") == "sitemapindex" {
				sitemaps = appendUnique(sitemaps, u) // read in a later iteration; the loop stops at maxSitemaps
				continue
			}
			entries = appendUnique(entries, u)
		}
	}
	for _, p := range OpenAPIPaths {
		c, err := get(p)
		if err != nil {
			return err
		}
		if !ok(c, content.KindJSON) {
			continue
		}
		s, err := a.c.getSnapshot(ctx, tc, c.snap.GetId(), true)
		if err != nil {
			return err
		}
		fs, err := features(s)
		if err != nil {
			return err
		}
		for _, f := range fs {
			if f.Kind == content.FeatSchemaEndpoint {
				add(origin + p)
				break
			}
		}
	}
	gql, err := get("/graphql?query=%7B__typename%7D")
	if err != nil {
		return err
	}
	if gql != nil && (gql.status == 200 || gql.status == 400) && isGraphQLResponse(gql.body) {
		add(origin + "/graphql")
	}
	for _, u := range entries {
		_, ok, err := allowed(ctx, tc, out, u)
		if err != nil {
			return err
		}
		if ok {
			add(u)
		}
	}
	return nil
}

// isGraphQLResponse reports a GraphQL response body: a JSON object with a
// data or errors member (GraphQL over HTTP, "Response").
func isGraphQLResponse(body []byte) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(body, &m) != nil {
		return false
	}
	_, data := m["data"]
	_, errs := m["errors"]
	return data || errs
}
