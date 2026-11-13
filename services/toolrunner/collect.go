package toolrunner

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/engine"
	"github.com/genuinebnt/flowforge/oam"
)

// Collect is the active collect tool (V2_DESIGN §8): `(InScope<Url>,
// String) -> Snapshot`, the string naming a capture profile. It fetches
// the URL through the egress proxy with the profile's request shape,
// following redirects hop by hop (each re-checked and fetched under its
// own decision, recorded in the capture's redirect chain), reads at most
// MaxBody bytes (a longer body is cut and the capture partial), and
// stores the response with the content service. For an HTML page it also
// captures the page's scripts and styles, and for each script its source
// map, with page_url set so renamed bundles find their predecessor
// (V2 §5 chunk lineage); every subresource is scope-checked first and a
// denied one is skipped, never contacted. The output is the page's
// Snapshot with the subresources' Snapshots in resources.
type Collect struct{ c *contentTools }

// Spec implements Adapter.
func (*Collect) Spec() Spec {
	return Spec{Name: "collect", Version: "1.0.0", Description: "Capture a URL (and a page's scripts and styles) into the content store",
		InputType: "(InScope<Url>, String)", OutputType: "Snapshot",
		Observes: []string{oam.TypeURL, oam.TypeFile, oam.TypeEndpoint, oam.TypeProductRelease},
		Class:    core.ClassActive, RateRPS: 10, RateBurst: 20, Concurrency: 4, Timeout: 5 * time.Minute, Idempotent: true,
		Idempotency: engine.IdempotencyIdempotent, Retry: RetryContent}
}

// Run implements Adapter.
func (a *Collect) Run(ctx context.Context, input json.RawMessage, tc *ToolCtx, out Emitter) (json.RawMessage, error) {
	args, err := decodeArgs(input, 2)
	if err != nil {
		return nil, err
	}
	target, err := decodeString(args[0])
	if err != nil {
		return nil, err
	}
	name, err := decodeString(args[1])
	if err != nil {
		return nil, err
	}
	prof, err := a.c.profile(name)
	if err != nil {
		return nil, err
	}
	page, err := a.c.fetch(ctx, tc, out, target, prof, fetchOpts{})
	if errors.Is(err, errSkipped) {
		// The workflow held an InScope proof, so a skip here means scope
		// changed under the run, the proxy refused, or the target is
		// down: a failure of this capture, not an empty result.
		return nil, Errorf("not_captured", false, "collect "+target+": the target was skipped (see the skipped events)")
	}
	if err != nil {
		return nil, err
	}
	rec := snapshotOf(page.snap, tc, page.redirects)
	resources, err := a.resources(ctx, tc, out, page, prof)
	if err != nil {
		return nil, err
	}
	rec.Resources = resources
	_ = out.Progress(int64(1+len(resources)), int64(1+len(resources)), "captured")
	return json.Marshal(rec)
}

// resources captures the scripts and styles a page loads and the source
// maps those scripts name (one level), at most MaxResources. The order is
// that of the page's stored features (scripts, then styles, each sorted
// by lineage key, not document order), then each captured script's source
// map as it is found; what the limit cuts is reported as resource_limit.
// Two bundles sharing a lineage key are one feature, so only the one its
// Ref names is captured.
func (a *Collect) resources(ctx context.Context, tc *ToolCtx, out Emitter, page *capture, prof content.Profile) ([]snapshotRecord, error) {
	out2 := []snapshotRecord{}
	if a.c.cfg.MaxResources < 0 || page.snap.GetKind() != string(content.KindHTML) || page.snap.GetDegraded() {
		return out2, nil
	}
	type sub struct{ url, page string }
	var queue []sub
	seen := map[string]bool{page.snap.GetUrl(): true}
	add := func(fs []content.Feature, from string, kinds ...content.FeatureKind) {
		for _, f := range fs {
			for _, k := range kinds {
				if f.Kind != k {
					continue
				}
				if u, ok := featureURL(f); ok && !seen[u] {
					seen[u] = true
					queue = append(queue, sub{u, from})
				}
			}
		}
	}
	pf, err := a.c.getSnapshot(ctx, tc, page.snap.GetId(), true)
	if err != nil {
		return nil, err
	}
	fs, err := features(pf)
	if err != nil {
		return nil, err
	}
	add(fs, page.snap.GetUrl(), content.FeatScript, content.FeatStyle)
	i := 0
	for ; i < len(queue) && len(out2) < a.c.cfg.MaxResources; i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s := queue[i]
		got, err := a.c.fetch(ctx, tc, out, s.url, prof, fetchOpts{page: s.page})
		if errors.Is(err, errSkipped) {
			continue
		}
		if err != nil {
			if te, ok := isToolError(err); ok && te.Retryable {
				return nil, err
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			_ = out.Skipped(s.url, "capture_failed")
			continue
		}
		out2 = append(out2, snapshotOf(got.snap, tc, got.redirects))
		if got.snap.GetKind() == string(content.KindJS) && !got.snap.GetDegraded() {
			sf, err := a.c.getSnapshot(ctx, tc, got.snap.GetId(), true)
			if err != nil {
				return nil, err
			}
			fs, err := features(sf)
			if err != nil {
				return nil, err
			}
			add(fs, s.url, content.FeatSourceMap)
		}
	}
	for _, s := range queue[i:] {
		_ = out.Skipped(s.url, "resource_limit")
	}
	return out2, nil
}
