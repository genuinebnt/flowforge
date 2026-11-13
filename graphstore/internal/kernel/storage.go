package kernel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/oam"
)

// Hidden tag types. graph.entity_tags / graph.edge_tags rows with these
// types hold the merge bookkeeping the spec 10.5 columns have no room
// for; reads never return them. oam rejects any property type outside its
// five, so no user tag can collide.
const (
	// PTypeMeta (name "") holds the per-field content stamps, the first
	// and last sightings and the per-field stamps of every other tag.
	PTypeMeta = "ff:meta"
	// PTypeRun (name = tool) holds a tool's latest sighting and its
	// covering-run miss counter.
	PTypeRun = "ff:run"
	// PTypeLive (name "") holds the liveness bookkeeping of oam's
	// authoritative staleness: classic staleness, the profiles holding an
	// edge, the authoritative references of an entity and the support of
	// every tag (V2 §3.4). A row without it (written before 0030) loads
	// as classic: classic_stale = (state = 'stale'), every tag classic.
	PTypeLive = "ff:live"
	// PTypeAuth (name = oam.UnitKey(profile, family)) holds one authority
	// unit of the entity as a source.
	PTypeAuth = "ff:auth"
	// HiddenPrefix prefixes every hidden tag type.
	HiddenPrefix = "ff:"
)

type tagLiveJSON struct {
	Classic bool     `json:"classic"`
	AuthIn  []string `json:"auth_in,omitempty"`
}

type liveJSON struct {
	ClassicStale bool                              `json:"classic_stale"`
	AuthIn       []string                          `json:"auth_in,omitempty"`
	AuthRefs     int                               `json:"auth_refs,omitempty"`
	Tags         map[string]map[string]tagLiveJSON `json:"tags,omitempty"`
}

type headJSON struct {
	Complete *stampJSON           `json:"complete,omitempty"`
	Members  []string             `json:"members"`
	Partial  map[string]stampJSON `json:"partial,omitempty"`
	Targets  map[string]string    `json:"targets,omitempty"`
}

// LiveTag returns the stored liveness row of r.
func (r *Record) LiveTag() (StoredTag, error) {
	lj := liveJSON{ClassicStale: r.ClassicStale, AuthIn: r.AuthIn, AuthRefs: r.AuthRefs}
	for k, t := range r.Tags {
		if t.Classic && len(t.AuthIn) == 0 {
			continue
		}
		if lj.Tags == nil {
			lj.Tags = map[string]map[string]tagLiveJSON{}
		}
		if lj.Tags[k.PType] == nil {
			lj.Tags[k.PType] = map[string]tagLiveJSON{}
		}
		lj.Tags[k.PType][k.Name] = tagLiveJSON{Classic: t.Classic, AuthIn: t.AuthIn}
	}
	b, err := json.Marshal(lj)
	if err != nil {
		return StoredTag{}, fmt.Errorf("kernel: encode live: %w", err)
	}
	return StoredTag{PType: PTypeLive, Content: b, CreatedAt: r.First.Stamp.At, LastSeen: r.Last.Stamp.At}, nil
}

// MergedLiveTag returns the liveness row of a record loaded without its
// tags (a covering run): the record-level fields from r, the tag entries
// from LiveRaw.
func (r *Record) MergedLiveTag() (StoredTag, error) {
	var lj liveJSON
	if len(r.LiveRaw) > 0 {
		if err := json.Unmarshal(r.LiveRaw, &lj); err != nil {
			return StoredTag{}, fmt.Errorf("kernel: decode live: %w", err)
		}
	}
	lj.ClassicStale, lj.AuthIn, lj.AuthRefs = r.ClassicStale, r.AuthIn, r.AuthRefs
	b, err := json.Marshal(lj)
	if err != nil {
		return StoredTag{}, fmt.Errorf("kernel: encode live: %w", err)
	}
	return StoredTag{PType: PTypeLive, Content: b, CreatedAt: r.First.Stamp.At, LastSeen: r.Last.Stamp.At}, nil
}

// headTags returns the stored rows of an entity's authority units.
func (e *Entity) headTags() ([]StoredTag, error) {
	var out []StoredTag
	for _, k := range slices.Sorted(maps.Keys(e.Heads)) {
		h := e.Heads[k]
		hj := headJSON{Members: slices.Sorted(maps.Keys(h.Members)), Targets: h.Targets}
		if hj.Members == nil {
			hj.Members = []string{}
		}
		if h.HasComplete {
			c := encStamp(h.Complete)
			hj.Complete = &c
		}
		if len(h.Partial) > 0 {
			hj.Partial = encStamps(h.Partial)
		}
		b, err := json.Marshal(hj)
		if err != nil {
			return nil, fmt.Errorf("kernel: encode head: %w", err)
		}
		out = append(out, StoredTag{PType: PTypeAuth, Name: k, Content: b, CreatedAt: e.First.Stamp.At, LastSeen: e.Last.Stamp.At})
	}
	return out, nil
}

// DecodeHead decodes a PTypeAuth row.
func DecodeHead(t StoredTag) (*Head, error) {
	var hj headJSON
	if err := json.Unmarshal(t.Content, &hj); err != nil {
		return nil, fmt.Errorf("kernel: decode head: %w", err)
	}
	h := &Head{AuthUnit: *oam.NewAuthUnit(), Targets: hj.Targets}
	if h.Targets == nil {
		h.Targets = map[string]string{}
	}
	if hj.Complete != nil {
		s, err := hj.Complete.stamp()
		if err != nil {
			return nil, err
		}
		h.HasComplete, h.Complete = true, s
	}
	for _, m := range hj.Members {
		h.Members[m] = true
	}
	ps, err := decStamps(hj.Partial)
	if err != nil {
		return nil, err
	}
	h.Partial = ps
	return h, nil
}

// HeadTargets lists every edge member and target of a decoded head (for
// preloading before locks).
func HeadTargets(h *Head) (edges, entities []string) {
	for m := range h.Effective() {
		if len(m) > 2 && m[:2] == "t:" {
			continue
		}
		edges = append(edges, m)
		if to := h.Targets[m]; to != "" {
			entities = append(entities, to)
		}
	}
	return edges, entities
}

// PublicTagRows returns the stored rows that are visible tags (hidden
// rows and withdrawn tags dropped), using the row set's ff:live row.
func PublicTagRows(stored []StoredTag) ([]StoredTag, error) {
	var lj liveJSON
	for _, t := range stored {
		if t.PType == PTypeLive {
			if err := json.Unmarshal(t.Content, &lj); err != nil {
				return nil, fmt.Errorf("kernel: decode live: %w", err)
			}
		}
	}
	var out []StoredTag
	for _, t := range stored {
		if len(t.PType) >= len(HiddenPrefix) && t.PType[:len(HiddenPrefix)] == HiddenPrefix {
			continue
		}
		if tl, ok := lj.Tags[t.PType][t.Name]; ok && !tl.Classic && len(tl.AuthIn) == 0 {
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// DecodeLive applies a PTypeLive row to r (state must be loaded).
func (r *Record) DecodeLive(t StoredTag) error {
	var lj liveJSON
	if err := json.Unmarshal(t.Content, &lj); err != nil {
		return fmt.Errorf("kernel: decode live: %w", err)
	}
	r.ClassicStale, r.AuthIn, r.AuthRefs = lj.ClassicStale, lj.AuthIn, lj.AuthRefs
	for p, names := range lj.Tags {
		for n, tl := range names {
			if ts, ok := r.Tags[TagKey{p, n}]; ok {
				ts.Classic, ts.AuthIn = tl.Classic, tl.AuthIn
			}
		}
	}
	return nil
}

// StoredTag is one tag row.
type StoredTag struct {
	PType, Name         string
	Content             []byte // JSON object
	CreatedAt, LastSeen time.Time
}

// EntityRow is one graph.entities row (content as JSON).
type EntityRow struct {
	ID, AssetType, Key        string
	Content                   []byte
	Scope                     oam.ScopeStatus
	Confidence                float64
	State                     oam.State
	CreatedAt, LastSeen       time.Time
	FirstSeenRun, LastSeenRun string
	MissedRuns                int
	SearchText                string
}

// EdgeRow is one graph.edges row (content as JSON).
type EdgeRow struct {
	ID, RelType, Label, Disc string
	Content                  []byte
	From, To                 string
	Confidence               float64
	State                    oam.State
	CreatedAt, LastSeen      time.Time
}

type stampJSON struct {
	At  string `json:"at"`
	Src string `json:"src"`
	Obs string `json:"obs"`
	Run string `json:"run,omitempty"`
}

type metaJSON struct {
	First     stampJSON                                  `json:"first"`
	Last      stampJSON                                  `json:"last"`
	Stamps    map[string]stampJSON                       `json:"stamps"`
	TagStamps map[string]map[string]map[string]stampJSON `json:"tag_stamps,omitempty"`
}

type runJSON struct {
	stampJSON
	Covered bool `json:"covered"`
	Missed  int  `json:"missed"`
}

func encStamp(s oam.Stamp) stampJSON {
	return stampJSON{At: s.At.UTC().Format(time.RFC3339Nano), Src: s.Source, Obs: s.Observation}
}

func encSighting(s Sighting) stampJSON {
	j := encStamp(s.Stamp)
	j.Run = s.Run
	return j
}

func (j stampJSON) stamp() (oam.Stamp, error) {
	if j.At == "" {
		return oam.Stamp{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, j.At)
	if err != nil {
		return oam.Stamp{}, fmt.Errorf("kernel: bad stamp time %q: %w", j.At, err)
	}
	return oam.Stamp{At: t.UTC(), Source: j.Src, Observation: j.Obs}, nil
}

func (j stampJSON) sighting() (Sighting, error) {
	s, err := j.stamp()
	return Sighting{Stamp: s, Run: j.Run}, err
}

func encStamps(m map[string]oam.Stamp) map[string]stampJSON {
	out := make(map[string]stampJSON, len(m))
	for k, s := range m {
		out[k] = encStamp(s)
	}
	return out
}

func decStamps(m map[string]stampJSON) (map[string]oam.Stamp, error) {
	out := make(map[string]oam.Stamp, len(m))
	for k, j := range m {
		s, err := j.stamp()
		if err != nil {
			return nil, err
		}
		out[k] = s
	}
	return out, nil
}

// DecodeObject decodes a JSON object keeping numbers as json.Number, the
// representation oam content uses.
func DecodeObject(b []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("kernel: decode object: %w", err)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m, nil
}

// DecodePatch decodes a stored change-event patch.
func DecodePatch(b []byte) ([]oam.PatchOp, error) {
	if len(b) == 0 || string(b) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var ops []oam.PatchOp
	if err := dec.Decode(&ops); err != nil {
		return nil, fmt.Errorf("kernel: decode patch: %w", err)
	}
	return ops, nil
}

// DecodeContentChange decodes the stored patch of a content_changed
// event (an object, V2 §3.4).
func DecodeContentChange(b []byte) (*oam.ContentChangePatch, error) {
	var p oam.ContentChangePatch
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("kernel: decode content change: %w", err)
	}
	return &p, nil
}

// EncodePatch encodes a patch for storage (nil for no patch).
func EncodePatch(ops []oam.PatchOp) ([]byte, error) {
	if len(ops) == 0 {
		return nil, nil
	}
	return json.Marshal(ops)
}

// StoredTags returns every tag row of r: the meta row, one run row per
// tool, one SourceProperty per source and the other tags, sorted by
// (type, name).
func (r *Record) StoredTags() ([]StoredTag, error) {
	meta := metaJSON{First: encSighting(r.First), Last: encSighting(r.Last), Stamps: encStamps(r.Content.Stamps)}
	var out []StoredTag
	add := func(ptype, name string, v any, created, last time.Time) error {
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Errorf("kernel: encode %s tag: %w", ptype, err)
		}
		out = append(out, StoredTag{PType: ptype, Name: name, Content: b, CreatedAt: created, LastSeen: last})
		return nil
	}
	for _, k := range slices.SortedFunc(maps.Keys(r.Tags), func(a, b TagKey) int {
		if a.PType != b.PType {
			return cmpStr(a.PType, b.PType)
		}
		return cmpStr(a.Name, b.Name)
	}) {
		t := r.Tags[k]
		if meta.TagStamps == nil {
			meta.TagStamps = map[string]map[string]map[string]stampJSON{}
		}
		if meta.TagStamps[k.PType] == nil {
			meta.TagStamps[k.PType] = map[string]map[string]stampJSON{}
		}
		meta.TagStamps[k.PType][k.Name] = encStamps(t.Content.Stamps)
		if err := add(k.PType, k.Name, t.Content.Values, t.Created, t.LastSeen); err != nil {
			return nil, err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(r.Sources)) {
		s := r.Sources[name]
		if err := add(oam.PropertySource, name, SourceContent(name, s.Confidence), s.Created, s.LastSeen); err != nil {
			return nil, err
		}
	}
	for _, tool := range slices.Sorted(maps.Keys(r.Runs)) {
		rs := r.Runs[tool]
		if err := add(PTypeRun, tool, runJSON{stampJSON: encSighting(rs.Last), Covered: rs.Covered, Missed: rs.Missed},
			rs.Last.Stamp.At, rs.Last.Stamp.At); err != nil {
			return nil, err
		}
	}
	if err := add(PTypeMeta, "", meta, r.First.Stamp.At, r.Last.Stamp.At); err != nil {
		return nil, err
	}
	live, err := r.LiveTag()
	if err != nil {
		return nil, err
	}
	return append(out, live), nil
}

// StoredTags returns every tag row of the entity: the record's rows plus
// its authority units.
func (e *Entity) StoredTags() ([]StoredTag, error) {
	out, err := e.Record.StoredTags()
	if err != nil {
		return nil, err
	}
	heads, err := e.headTags()
	if err != nil {
		return nil, err
	}
	return append(out, heads...), nil
}

// RunTag returns the stored run row of one tool (for covering-run
// updates that touch nothing else).
func (r *Record) RunTag(tool string) (StoredTag, bool, error) {
	rs, ok := r.Runs[tool]
	if !ok {
		return StoredTag{}, false, nil
	}
	b, err := json.Marshal(runJSON{stampJSON: encSighting(rs.Last), Covered: rs.Covered, Missed: rs.Missed})
	if err != nil {
		return StoredTag{}, false, err
	}
	return StoredTag{PType: PTypeRun, Name: tool, Content: b, CreatedAt: rs.Last.Stamp.At, LastSeen: rs.Last.Stamp.At}, true, nil
}

func cmpStr(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Row returns the graph.entities row of e.
func (e *Entity) Row() (EntityRow, error) {
	b, err := json.Marshal(e.Content.Values)
	if err != nil {
		return EntityRow{}, fmt.Errorf("kernel: encode content: %w", err)
	}
	return EntityRow{
		ID: e.ID, AssetType: e.AssetType, Key: e.Key, Content: b, Scope: e.Scope, Confidence: e.Confidence(),
		State: e.State, CreatedAt: e.First.Stamp.At, LastSeen: e.Last.Stamp.At,
		FirstSeenRun: e.First.Run, LastSeenRun: e.Last.Run, MissedRuns: e.MinMissed(),
		SearchText: graphstore.SearchText(e.Content.Values),
	}, nil
}

// Row returns the graph.edges row of e.
func (e *Edge) Row() (EdgeRow, error) {
	b, err := json.Marshal(e.Content.Values)
	if err != nil {
		return EdgeRow{}, fmt.Errorf("kernel: encode content: %w", err)
	}
	return EdgeRow{
		ID: e.ID, RelType: e.RelType, Label: e.Label, Disc: e.Disc, Content: b, From: e.From, To: e.To,
		Confidence: e.Confidence(), State: e.State, CreatedAt: e.First.Stamp.At, LastSeen: e.Last.Stamp.At,
	}, nil
}

// LoadEntity rebuilds an entity record from its row and tag rows.
func LoadEntity(tenant, program string, row EntityRow, tags []StoredTag) (*Entity, error) {
	e := &Entity{AssetType: row.AssetType, Key: row.Key, Scope: row.Scope, Heads: map[string]*Head{}}
	e.ID, e.Tenant, e.Program, e.State = row.ID, tenant, program, row.State
	if err := e.load(row.Content, tags, row.CreatedAt, row.LastSeen, row.FirstSeenRun, row.LastSeenRun); err != nil {
		return nil, fmt.Errorf("entity %s: %w", row.ID, err)
	}
	for _, t := range tags {
		if t.PType == PTypeAuth {
			h, err := DecodeHead(t)
			if err != nil {
				return nil, fmt.Errorf("entity %s: %w", row.ID, err)
			}
			e.Heads[t.Name] = h
		}
	}
	return e, nil
}

// LoadEdge rebuilds an edge record from its row and tag rows.
func LoadEdge(tenant, program string, row EdgeRow, tags []StoredTag) (*Edge, error) {
	e := &Edge{RelType: row.RelType, Label: row.Label, Disc: row.Disc, From: row.From, To: row.To}
	e.ID, e.Tenant, e.Program, e.State = row.ID, tenant, program, row.State
	if err := e.load(row.Content, tags, row.CreatedAt, row.LastSeen, "", ""); err != nil {
		return nil, fmt.Errorf("edge %s: %w", row.ID, err)
	}
	return e, nil
}

// load fills r from stored rows. A row without the meta tag (written by
// some other tool) loads with zero stamps, so any later write wins.
func (r *Record) load(content []byte, tags []StoredTag, created, last time.Time, firstRun, lastRun string) error {
	values, err := DecodeObject(content)
	if err != nil {
		return err
	}
	r.Content = oam.Content{Values: values, Stamps: map[string]oam.Stamp{}}
	r.First = Sighting{Stamp: oam.Stamp{At: created}, Run: firstRun}
	r.Last = Sighting{Stamp: oam.Stamp{At: last}, Run: lastRun}
	r.Sources, r.Tags, r.Runs = map[string]*Source{}, map[TagKey]*TagState{}, map[string]*RunState{}
	var meta metaJSON
	var live *StoredTag
	for _, t := range tags {
		switch t.PType {
		case PTypeAuth:
		case PTypeLive:
			live = &t
		case PTypeMeta:
			if err := json.Unmarshal(t.Content, &meta); err != nil {
				return fmt.Errorf("kernel: decode meta: %w", err)
			}
			if r.First, err = meta.First.sighting(); err != nil {
				return err
			}
			if r.Last, err = meta.Last.sighting(); err != nil {
				return err
			}
			if r.Content.Stamps, err = decStamps(meta.Stamps); err != nil {
				return err
			}
		case PTypeRun:
			rs, err := DecodeRunTag(t)
			if err != nil {
				return err
			}
			r.Runs[t.Name] = &rs
		case oam.PropertySource:
			c, err := DecodeObject(t.Content)
			if err != nil {
				return err
			}
			n, _ := c["confidence"].(json.Number)
			pct, err := strconv.Atoi(n.String())
			if err != nil {
				return fmt.Errorf("kernel: bad source confidence %q", n)
			}
			r.Sources[t.Name] = &Source{Confidence: pct, Created: t.CreatedAt.UTC(), LastSeen: t.LastSeen.UTC()}
		default:
			c, err := DecodeObject(t.Content)
			if err != nil {
				return err
			}
			r.Tags[TagKey{t.PType, t.Name}] = &TagState{Content: oam.Content{Values: c, Stamps: map[string]oam.Stamp{}},
				Created: t.CreatedAt.UTC(), LastSeen: t.LastSeen.UTC()}
		}
	}
	for k, ts := range r.Tags {
		if st, ok := meta.TagStamps[k.PType][k.Name]; ok {
			if ts.Content.Stamps, err = decStamps(st); err != nil {
				return err
			}
		}
	}
	for k := range r.Content.Values {
		if _, ok := r.Content.Stamps[k]; !ok {
			r.Content.Stamps[k] = oam.Stamp{}
		}
	}
	for _, ts := range r.Tags {
		ts.Classic = true
	}
	if live == nil {
		r.ClassicStale = r.State == oam.StateStale
		return nil
	}
	return r.DecodeLive(*live)
}

// DecodeRunTag decodes a PTypeRun tag row.
func DecodeRunTag(t StoredTag) (RunState, error) {
	var rj runJSON
	if err := json.Unmarshal(t.Content, &rj); err != nil {
		return RunState{}, fmt.Errorf("kernel: decode run state: %w", err)
	}
	s, err := rj.sighting()
	if err != nil {
		return RunState{}, err
	}
	return RunState{Last: s, Covered: rj.Covered, Missed: rj.Missed}, nil
}
