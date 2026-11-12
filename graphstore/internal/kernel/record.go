package kernel

import (
	"cmp"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/genuinebnt/flowforge/oam"
)

// Sighting is one report of a record: its write stamp and run.
type Sighting struct {
	Stamp oam.Stamp
	Run   string
}

// Source is one SourceProperty: the highest confidence (percent) the
// source reported and when.
type Source struct {
	Confidence        int
	Created, LastSeen time.Time
}

// TagKey identifies a tag on a record.
type TagKey struct{ PType, Name string }

// TagState is a non-source tag.
type TagState struct {
	Content           oam.Content
	Created, LastSeen time.Time
}

// RunState is one tool's covering-run bookkeeping on a record: its latest
// sighting, and once the tool has run a covering run over the record, the
// number of consecutive covering runs that missed it.
type RunState struct {
	Last    Sighting
	Covered bool
	Missed  int
}

// Record is the state shared by entities and edges, exactly what
// oam.Graph keeps per record.
type Record struct {
	ID, Tenant, Program string
	Content             oam.Content
	State               oam.State
	First, Last         Sighting
	Sources             map[string]*Source
	Tags                map[TagKey]*TagState
	Runs                map[string]*RunState
	// Dirty marks a record the operation changed, to be written back.
	Dirty bool
}

// Entity is a graph node record.
type Entity struct {
	Record
	AssetType, Key string
	Scope          oam.ScopeStatus
}

// Edge is a graph edge record.
type Edge struct {
	Record
	RelType, Label, Disc string
	From, To             string
}

// State holds the records an operation reads and writes, by id.
type State struct {
	Entities map[string]*Entity
	Edges    map[string]*Edge
}

// NewState returns an empty State.
func NewState() *State { return &State{Entities: map[string]*Entity{}, Edges: map[string]*Edge{}} }

func newRecord(id, tenant, program string, p *Prepared, content map[string]any) Record {
	s := Sighting{p.Stamp, p.Obs.RunID}
	r := Record{
		ID: id, Tenant: tenant, Program: program,
		Content: oam.NewContent(content, p.Stamp), State: oam.StateActive,
		First: s, Last: s,
		Sources: map[string]*Source{}, Tags: map[TagKey]*TagState{},
		Runs:  map[string]*RunState{p.Stamp.Source: {Last: s}},
		Dirty: true,
	}
	r.addSource(p.Stamp.Source, p.Percent, p.Stamp.At)
	return r
}

// see records a sighting and reports whether it reactivated the record.
func (r *Record) see(p *Prepared) bool {
	s := Sighting{p.Stamp, p.Obs.RunID}
	r.Dirty = true
	if s.Stamp.Compare(r.First.Stamp) < 0 {
		r.First = s
	}
	if s.Stamp.Compare(r.Last.Stamp) > 0 {
		r.Last = s
	}
	tool := p.Stamp.Source
	r.addSource(tool, p.Percent, p.Stamp.At)
	rs, ok := r.Runs[tool]
	if !ok {
		rs = &RunState{Last: s}
		r.Runs[tool] = rs
	} else if s.Stamp.Compare(rs.Last.Stamp) > 0 {
		rs.Last = s
	}
	if rs.Covered {
		rs.Missed = 0
	}
	if r.State == oam.StateStale {
		r.State = oam.StateActive
		return true
	}
	return false
}

func (r *Record) addSource(name string, percent int, at time.Time) {
	s, ok := r.Sources[name]
	if !ok {
		r.Sources[name] = &Source{Confidence: percent, Created: at, LastSeen: at}
		return
	}
	s.Confidence = max(s.Confidence, percent)
	s.Created, s.LastSeen = oam.MinTime(s.Created, at), oam.MaxTime(s.LastSeen, at)
}

func (r *Record) tag(t PTag, stamp oam.Stamp) {
	r.Dirty = true
	if t.Source != nil {
		r.addSource(t.Source.Name, t.Source.Confidence, stamp.At)
		return
	}
	k := TagKey{t.PType, t.Name}
	ts, ok := r.Tags[k]
	if !ok {
		r.Tags[k] = &TagState{Content: oam.NewContent(t.Content, stamp), Created: stamp.At, LastSeen: stamp.At}
		return
	}
	ts.Content.Merge(t.Content, stamp)
	ts.Created, ts.LastSeen = oam.MinTime(ts.Created, stamp.At), oam.MaxTime(ts.LastSeen, stamp.At)
}

// Confidence is the noisy-or over the record's sources (oam rule).
func (r *Record) Confidence() float64 {
	s := make(oam.Sources, len(r.Sources))
	for name, st := range r.Sources {
		s[name] = float64(st.Confidence) / 100
	}
	return s.Confidence()
}

// MissedRuns returns the per-tool missed counters of tools that covered
// the record (nil when none), as oam.Entity.MissedRuns.
func (r *Record) MissedRuns() map[string]int {
	var out map[string]int
	for tool, rs := range r.Runs {
		if rs.Covered {
			if out == nil {
				out = map[string]int{}
			}
			out[tool] = rs.Missed
		}
	}
	return out
}

// MinMissed is the smallest missed counter over covering tools (0 when
// none), stored in graph.entities.missed_runs: the record is stale once
// it reaches stale_after_runs.
func (r *Record) MinMissed() int {
	m, any := 0, false
	for _, rs := range r.Runs {
		if rs.Covered && (!any || rs.Missed < m) {
			m, any = rs.Missed, true
		}
	}
	return m
}

// TagViews returns the record's tags as oam presents them: one
// SourceProperty per source plus the other tags, sorted by (type, name).
func (r *Record) TagViews() []oam.Tag {
	out := make([]oam.Tag, 0, len(r.Sources)+len(r.Tags))
	for name, s := range r.Sources {
		out = append(out, oam.Tag{
			PropertyType: oam.PropertySource, Name: name, Content: SourceContent(name, s.Confidence),
			CreatedAt: s.Created, LastSeen: s.LastSeen,
		})
	}
	for k, t := range r.Tags {
		out = append(out, oam.Tag{PropertyType: k.PType, Name: k.Name, Content: maps.Clone(t.Content.Values), CreatedAt: t.Created, LastSeen: t.LastSeen})
	}
	slices.SortFunc(out, func(a, b oam.Tag) int {
		return cmp.Or(cmp.Compare(a.PropertyType, b.PropertyType), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// SourceContent is the content of a SourceProperty tag.
func SourceContent(name string, percent int) map[string]any {
	return map[string]any{"name": name, "confidence": json.Number(strconv.Itoa(percent))}
}

// View returns the entity as oam.Entity.
func (e *Entity) View() oam.Entity {
	return oam.Entity{
		ID: e.ID, TenantID: e.Tenant, ProgramID: e.Program, AssetType: e.AssetType, Key: e.Key,
		Content: maps.Clone(e.Content.Values), ScopeStatus: e.Scope, Confidence: e.Confidence(), State: e.State,
		CreatedAt: e.First.Stamp.At, LastSeen: e.Last.Stamp.At, FirstSeenRun: e.First.Run, LastSeenRun: e.Last.Run,
		MissedRuns: e.MissedRuns(), Tags: e.TagViews(),
	}
}

// View returns the edge as oam.Edge.
func (e *Edge) View() oam.Edge {
	return oam.Edge{
		ID: e.ID, TenantID: e.Tenant, ProgramID: e.Program, RelationType: e.RelType, Label: e.Label,
		Discriminator: e.Disc, Content: maps.Clone(e.Content.Values), FromEntity: e.From, ToEntity: e.To,
		Confidence: e.Confidence(), State: e.State, CreatedAt: e.First.Stamp.At, LastSeen: e.Last.Stamp.At,
		MissedRuns: e.MissedRuns(), Tags: e.TagViews(),
	}
}
