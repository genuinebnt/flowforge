package kernel

import (
	"cmp"
	"errors"
	"maps"
	"slices"
	"strings"

	"github.com/genuinebnt/flowforge/oam"
)

// ErrMissingRecords means an authoritative replacement needs records the
// caller did not load (members of the source's units and their targets);
// the caller loads Preload's ids and retries. The Postgres store reads
// the heads before locking, so it only sees this when another writer
// changed them in between, and then retries the transaction.
var ErrMissingRecords = errors.New("kernel: authority replacement needs records that were not loaded")

// Preload returns the edge and entity ids an authoritative observation
// may withdraw, given its source entity as stored (nil when new): every
// effective member edge of the units its complete coverage replaces, and
// those edges' targets. Tag members live on the source itself.
func (p *Prepared) Preload(src *Entity) (edges, entities []string) {
	if p.Auth == nil || !p.Auth.Complete || src == nil {
		return nil, nil
	}
	for _, fam := range p.Auth.Covered {
		h, ok := src.Heads[oam.UnitKey(p.Auth.Profile, fam)]
		if !ok {
			continue
		}
		for m := range h.Effective() {
			if strings.HasPrefix(m, "t:") {
				continue
			}
			edges = append(edges, m)
			if to := h.Targets[m]; to != "" {
				entities = append(entities, to)
			}
		}
	}
	slices.Sort(edges)
	slices.Sort(entities)
	return slices.Compact(edges), slices.Compact(entities)
}

// checkHeads verifies that st holds what Preload asks for.
func (st *State) checkHeads(p *Prepared) error {
	if p.Auth == nil {
		return nil
	}
	edges, ents := p.Preload(st.Entities[p.SourceID()])
	for _, id := range edges {
		if _, ok := st.Edges[id]; !ok {
			return ErrMissingRecords
		}
	}
	for _, id := range ents {
		if _, ok := st.Entities[id]; !ok {
			return ErrMissingRecords
		}
	}
	return nil
}

type tagRef struct {
	edge   bool
	target string
	ptype  string
	name   string
}

func compareTagRef(a, b tagRef) int {
	if a.edge != b.edge {
		if !a.edge {
			return -1
		}
		return 1
	}
	return cmp.Or(cmp.Compare(a.target, b.target), cmp.Compare(a.ptype, b.ptype), cmp.Compare(a.name, b.name))
}

type tagSnap struct {
	visible bool
	content map[string]any
}

// authTx mirrors oam's authTx over a kernel State.
type authTx struct {
	st          *State
	touchedEnt  map[string]bool
	touchedEdge map[string]bool
	tagBefore   map[tagRef]tagSnap
}

func (tx *authTx) snapTag(ref tagRef, r *Record) {
	if _, ok := tx.tagBefore[ref]; ok {
		return
	}
	ts, ok := r.Tags[TagKey{ref.ptype, ref.name}]
	if !ok || !ts.Visible() {
		tx.tagBefore[ref] = tagSnap{}
		return
	}
	tx.tagBefore[ref] = tagSnap{visible: true, content: maps.Clone(ts.Content.Values)}
}

func (tx *authTx) applyAuthority(src *Entity, p *Prepared, units map[string][]string) {
	if src.Heads == nil {
		src.Heads = map[string]*Head{}
	}
	src.HeadsDirty, src.Dirty = true, true
	for _, fam := range oam.UnitFamilies(units, p.Auth.Complete, p.Auth.Covered) {
		complete := p.Auth.Complete && slices.Contains(p.Auth.Covered, fam)
		k := oam.UnitKey(p.Auth.Profile, fam)
		h := src.Heads[k]
		if h == nil {
			h = &Head{AuthUnit: *oam.NewAuthUnit(), Targets: map[string]string{}}
			src.Heads[k] = h
		}
		before := h.Effective()
		h.Apply(p.Stamp, units[fam], complete)
		after := h.Effective()
		all := slices.Collect(maps.Keys(before))
		for m := range after {
			all = append(all, m)
		}
		slices.Sort(all)
		for _, m := range slices.Compact(all) {
			if before[m] != after[m] {
				tx.setMember(src, p.Auth.Profile, m, after[m])
			}
		}
		// Keep the targets of the effective edge members only.
		for m := range h.Targets {
			if !after[m] {
				delete(h.Targets, m)
			}
		}
		for m := range after {
			if ed, ok := tx.st.Edges[m]; ok {
				h.Targets[m] = ed.To
			}
		}
	}
}

func (tx *authTx) setMember(src *Entity, profile, m string, in bool) {
	if ptype, name, ok := oam.ParseTagMember(m); ok {
		ref := tagRef{target: src.ID, ptype: ptype, name: name}
		tx.snapTag(ref, &src.Record)
		if ts, ok := src.Tags[TagKey{ptype, name}]; ok {
			ts.AuthIn = oam.SetProfile(ts.AuthIn, profile, in)
			src.Dirty = true
		}
		return
	}
	ed, ok := tx.st.Edges[m]
	if !ok {
		return
	}
	was := len(ed.AuthIn) > 0
	ed.AuthIn = oam.SetProfile(ed.AuthIn, profile, in)
	ed.Dirty = true
	tx.touchedEdge[m] = true
	if is := len(ed.AuthIn) > 0; is != was {
		if t, ok := tx.st.Entities[ed.To]; ok {
			if is {
				t.AuthRefs++
			} else {
				t.AuthRefs--
			}
			t.Dirty = true
			tx.touchedEnt[ed.To] = true
		}
	}
}

func (tx *authTx) settle(em *emitter) {
	for _, id := range slices.Sorted(maps.Keys(tx.touchedEdge)) {
		ed := tx.st.Edges[id]
		if ed.settle() {
			if ed.State == oam.StateActive {
				em.emit(oam.ChangeEdgeAdded, "", id, nil)
			} else {
				em.emit(oam.ChangeEdgeRemoved, "", id, nil)
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(tx.touchedEnt)) {
		e := tx.st.Entities[id]
		if e.settle() {
			if e.State == oam.StateActive {
				em.emit(oam.ChangeEntityReactivated, id, "", nil)
			} else {
				em.emit(oam.ChangeEntityStale, id, "", nil)
			}
		}
	}
}

func (tx *authTx) emitTags(em *emitter) {
	for _, ref := range slices.SortedFunc(maps.Keys(tx.tagBefore), compareTagRef) {
		var r *Record
		if ref.edge {
			r = &tx.st.Edges[ref.target].Record
		} else {
			r = &tx.st.Entities[ref.target].Record
		}
		after := tagSnap{}
		if ts, ok := r.Tags[TagKey{ref.ptype, ref.name}]; ok && ts.Visible() {
			after = tagSnap{visible: true, content: ts.Content.Values}
		}
		b := tx.tagBefore[ref]
		ops := oam.TagPatch(b.visible, b.content, after.visible, after.content)
		if len(ops) == 0 {
			continue
		}
		ev := Event{Type: oam.ChangeTagChanged, TagType: ref.ptype, TagName: ref.name, Patch: ops}
		if ref.edge {
			ev.EdgeID = ref.target
		} else {
			ev.EntityID = ref.target
		}
		em.emitEvent(ev)
	}
}
