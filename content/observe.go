package content

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/oam"
)

// ObserveOptions configures Observe.
type ObserveOptions struct {
	// Hasher computes the keyed hash of exposed secrets; without one no
	// exposed_secret tags are emitted (the graph requires the hash).
	Hasher KeyedHasher
	// SnapshotID, when set, adds the ContentProperty pointer tag.
	SnapshotID string
	// Cause and Baseline are copied onto the observation (V2 §12.2,
	// §12.3); empty cause means target.
	Cause    string
	Baseline bool
	// DiffFellBack marks a reading whose diff fell back to hash only;
	// such a reading is never complete (V2 §3.4).
	DiffFellBack bool
	// Predecessor is the lineage match of a script capture
	// (previous_version edge).
	Predecessor *LineageMatch
	// ToolVersion names the producing tool version (default "content@1").
	ToolVersion string
}

// ObservationTool is the tool name of content observations.
const ObservationTool = "content"

// Observe turns a processed capture into an oam observation (V2 §3, §5):
// the source entity (a URL for pages and documents, a File for scripts,
// styles and source maps), one entity and edge per graph-mapped feature,
// HTTPHeaderProperty / ParameterProperty / VulnProperty tags, and the
// authority {source, profile, coverage, covered_rules}. A degraded or
// failed capture yields only the source and its redirects, with coverage
// failed, so it can add but never withdraw. The observation id is derived
// from (URL, profile, body hash, fetch time), so re-observing is
// idempotent.
func Observe(p *Processed, opts ObserveOptions) (oam.Observation, error) {
	main := p.Main()
	cause := opts.Cause
	if cause == "" {
		cause = oam.CauseTarget
	}
	tv := opts.ToolVersion
	if tv == "" {
		tv = "content@1"
	}
	o := oam.Observation{
		ObservationID: observationID(p, main.BodySHA256),
		Tool:          oam.Tool{Name: ObservationTool, Version: tv},
		ObservedAt:    p.FetchedAt.UTC(),
		Confidence:    0.9,
		Cause:         cause,
		Baseline:      opts.Baseline,
	}
	b := &obsBuilder{o: &o, refs: map[string]string{}}
	srcIsFile := main.Kind == KindJS || main.Kind == KindCSS || main.Kind == KindSourceMap
	var src string
	if srcIsFile {
		src = b.entity(fileAsset(p.URL, main.Kind))
	} else {
		src = b.entity(oam.URL{URL: p.URL})
	}
	cov, rules := p.Coverage, p.CoveredRules
	if opts.DiffFellBack && cov == oam.CoverageComplete {
		cov = oam.CoveragePartial
	}
	for _, r := range p.Redirects {
		from := src
		if r.From != p.URL {
			from = b.entity(oam.URL{URL: r.From})
		}
		to := b.entity(oam.URL{URL: r.To})
		if from != "" && to != "" {
			status := r.Status
			if status < 300 || status > 399 {
				status = 0
			}
			b.edge(from, to, oam.LabelRedirectsTo, status)
		}
	}
	if p.Integrity.Degraded() || cov == oam.CoverageFailed {
		o.Authority = &oam.Authority{Source: src, Profile: p.Profile, Coverage: oam.CoverageFailed}
		return o, o.Validate()
	}
	feats := p.Features()
	epRef := map[string]string{} // endpoint key → ref
	endpointRef := func(key, source string) string {
		if r, ok := epRef[key]; ok {
			return r
		}
		method, tpl, _ := strings.Cut(strings.TrimPrefix(key, "ep:"), " ")
		r := b.entity(oam.Endpoint{Method: method, URLTemplate: tpl, Source: source})
		epRef[key] = r
		return r
	}
	for _, f := range feats {
		switch f.Kind {
		case FeatLink, FeatSitemapEntry:
			if !srcIsFile {
				b.edge(src, b.entity(oam.URL{URL: f.Key}), oam.LabelLinksTo, 0)
			}
		case FeatFrame:
			if !srcIsFile {
				b.edge(src, b.entity(oam.URL{URL: f.Key}), oam.LabelEmbeds, 0)
			}
		case FeatScript:
			if !srcIsFile {
				b.edge(src, b.entity(fileAsset(f.Ref, KindJS)), oam.LabelLoadsScript, 0)
			}
		case FeatStyle, FeatCSSImport:
			if f.Kind == FeatStyle && srcIsFile || f.Kind == FeatCSSImport && !srcIsFile {
				continue
			}
			b.edge(src, b.entity(fileAsset(f.Ref, KindCSS)), oam.LabelLoadsStyle, 0)
		case FeatForm:
			if !srcIsFile {
				b.edge(src, endpointRef(f.Key, oam.EndpointSourceHTMLForm), oam.LabelFormAction, 0)
			}
		case FeatEndpoint:
			b.edge(src, endpointRef(f.Key, f.Attr("source")), oam.LabelCalls, 0)
		case FeatSchemaEndpoint:
			if !srcIsFile {
				b.edge(src, endpointRef(f.Key, f.Attr("source")), oam.LabelDocuments, 0)
			}
		case FeatSourceMap:
			if srcIsFile {
				b.edge(src, b.entity(fileAsset(f.Ref, KindSourceMap)), oam.LabelSourceMap, 0)
			}
		case FeatLibrary:
			if srcIsFile && f.Value != "" {
				rel := oam.Opaque{Type: oam.TypeProductRelease, Content: map[string]any{"name": f.Key, "version": f.Value}}
				b.edge(src, b.entity(rel), oam.LabelIncludesLibrary, 0)
			}
		case FeatHeader:
			name, _, _ := strings.Cut(f.Key, ":")
			prop := oam.HTTPHeaderProperty{Name: name, Value: redactString(f.Value)}
			if prop.Validate() == nil {
				b.tag(src, prop)
			}
		case FeatSourceDisclosure:
			b.tag(src, oam.VulnProperty{ID: "source_disclosure:" + f.Key, Description: "server-side source served as content",
				Source: "ff-content@1", Category: "source_disclosure", Enum: f.Key})
		}
	}
	// Parameters attach to endpoints that exist in this observation.
	for _, f := range feats {
		if f.Kind != FeatParameter && f.Kind != FeatGraphQLOperation {
			continue
		}
		ref, ok := epRef[f.Attr("endpoint")]
		if !ok {
			continue
		}
		prop := oam.ParameterProperty{Name: paramName(f), Location: f.Attr("location"), TypeHint: f.Attr("type_hint")}
		if f.Kind == FeatGraphQLOperation {
			prop.TypeHint = "graphql_" + f.Value
		}
		if prop.Validate() == nil {
			b.tag(ref, prop)
		}
	}
	if opts.Hasher != nil {
		for _, fd := range p.Findings() {
			// oam.CategoryExposedSecret conventions: rule id, redacted
			// preview and the caller's keyed hash; never the secret.
			v := oam.VulnProperty{ID: "secret:" + fd.RuleID + ":" + opts.Hasher.KeyedHash(fd.Secret), Description: fd.Preview,
				Source: "ff-secrets@1", Category: oam.CategoryExposedSecret, Enum: fd.RuleID}
			if oam.ValidateExposedSecret(v) == nil {
				b.tag(src, v)
			}
		}
	}
	if opts.Predecessor != nil && srcIsFile && opts.Predecessor.URL != p.URL {
		b.edge(src, b.entity(fileAsset(opts.Predecessor.URL, main.Kind)), oam.LabelPreviousVersion, 0)
	}
	if opts.SnapshotID != "" {
		b.tag(src, oam.ContentProperty{SnapshotID: opts.SnapshotID, BodySHA256: main.BodySHA256,
			Size: int64(main.Size), Profile: p.Profile, NormalizerVersion: main.Normalized.Version,
			LastChangedAt: p.FetchedAt.UTC().Format(oam.TimeLayoutMillis)})
	}
	if b.overflow {
		cov = oam.CoveragePartial
	}
	o.Authority = &oam.Authority{Source: src, Profile: p.Profile, Coverage: cov, CoveredRules: rules}
	return o, o.Validate()
}

func paramName(f Feature) string {
	if f.Kind == FeatGraphQLOperation {
		_, n, _ := strings.Cut(f.Key, " ")
		return n
	}
	i := strings.LastIndexByte(f.Key, ':')
	return f.Key[i+1:]
}

func fileAsset(u string, k Kind) oam.File {
	name := path.Base(urlPath(u))
	if name == "." || name == "/" {
		name = ""
	}
	return oam.File{URL: u, Name: name, FileType: string(k)}
}

func redactString(s string) string {
	r, _ := Redact([]byte(s))
	return string(r)
}

func observationID(p *Processed, bodySHA string) string {
	in := strings.Join([]string{p.URL, p.Profile, bodySHA, p.FetchedAt.UTC().Format("2006-01-02T15:04:05.000000000Z")}, "\x00")
	return "obs-content-" + sha256Hex([]byte(in))[:32]
}

// obsBuilder assembles an observation with deduplicated entities.
type obsBuilder struct {
	o        *oam.Observation
	refs     map[string]string // asset type + key → ref
	edges    map[string]bool
	tags     map[string]bool
	overflow bool
}

// entity adds an asset once and returns its ref ("" when the batch is
// full or the asset is invalid).
func (b *obsBuilder) entity(a oam.Asset) string {
	key, err := a.Key()
	if err != nil {
		return ""
	}
	k := a.AssetType() + "\x00" + key
	if r, ok := b.refs[k]; ok {
		return r
	}
	if len(b.o.Entities) >= oam.MaxBatchEntities {
		b.overflow = true
		return ""
	}
	r := "e" + strconv.Itoa(len(b.o.Entities))
	b.refs[k] = r
	b.o.Entities = append(b.o.Entities, oam.ObservedEntity{Ref: r, Asset: a})
	return r
}

func (b *obsBuilder) edge(from, to, label string, status int) {
	if from == "" || to == "" {
		return
	}
	if b.edges == nil {
		b.edges = map[string]bool{}
	}
	k := from + "\x00" + to + "\x00" + label
	if b.edges[k] {
		return
	}
	b.edges[k] = true
	b.o.Edges = append(b.o.Edges, oam.ObservedEdge{From: from, To: to, Relation: oam.SimpleRelation{Label: label, Status: status}})
}

func (b *obsBuilder) tag(target string, p oam.Property) {
	if target == "" {
		return
	}
	if b.tags == nil {
		b.tags = map[string]bool{}
	}
	k := target + "\x00" + p.PropertyType() + "\x00" + p.PropertyName()
	if b.tags[k] {
		return
	}
	b.tags[k] = true
	b.o.Tags = append(b.o.Tags, oam.ObservedTag{Target: target, Property: p})
}

// ObservationJSON encodes an observation canonically (sorted keys), the
// form tests compare byte for byte.
func ObservationJSON(o oam.Observation) ([]byte, error) {
	b, err := oam.EncodeObservation(o)
	if err != nil {
		return nil, fmt.Errorf("content: encode observation: %w", err)
	}
	return core.CanonicalJSON(jsonRaw(b))
}

// jsonRaw lets CanonicalJSON re-encode already-encoded JSON.
type jsonRaw []byte

// MarshalJSON implements json.Marshaler.
func (r jsonRaw) MarshalJSON() ([]byte, error) { return r, nil }
