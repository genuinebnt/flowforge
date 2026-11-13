package oam

import (
	"bytes"
	"cmp"
	"maps"
	"slices"
	"strings"

	"github.com/genuinebnt/flowforge/core"
)

// An authority unit is (source entity, profile, rule family). Its members
// are the edges leaving the source (edge ids) and the tags on the source
// (tagMember strings) of that family. The unit's effective member set is
//
//	members(latest complete reading) ∪ { m : some partial reading newer
//	than the latest complete one reported m }
//
// where readings are ordered by their Stamp (observed_at, tool,
// observation id), a total order over distinct observations. The set is a
// function of the multiset of applied observations, not of their order or
// duplication, which is what makes authoritative staleness convergent
// (spec 23.5): a complete reading only replaces the unit when it is
// newer than the current one, and a partial reading only adds members
// when it is newer than the latest complete one.
//
// AuthUnit is that bookkeeping; the Postgres store persists it as a
// hidden tag row on the source entity.
type AuthUnit struct {
	HasComplete bool
	Complete    Stamp
	Members     map[string]bool  // the latest complete reading
	Partial     map[string]Stamp // member -> newest partial reading newer than Complete
}

// UnitKey names a unit of a source entity: "<profile>/<family>".
func UnitKey(profile, family string) string { return profile + "/" + family }

// TagMember is the member string of a tag in a unit: "t:<type>/<name>".
// Edge members are edge ids.
func TagMember(ptype, name string) string { return "t:" + ptype + "/" + name }

// ParseTagMember is the inverse of TagMember.
func ParseTagMember(m string) (ptype, name string, ok bool) {
	rest, ok := strings.CutPrefix(m, "t:")
	if !ok {
		return "", "", false
	}
	return strings.Cut(rest, "/")
}

// NewAuthUnit returns an empty unit.
func NewAuthUnit() *AuthUnit {
	return &AuthUnit{Members: map[string]bool{}, Partial: map[string]Stamp{}}
}

// Effective returns the unit's effective member set.
func (h *AuthUnit) Effective() map[string]bool {
	out := maps.Clone(h.Members)
	if out == nil {
		out = map[string]bool{}
	}
	for m := range h.Partial {
		out[m] = true
	}
	return out
}

// Apply records one reading (its stamp, the members it reports, and
// whether it is complete for the unit's family).
func (h *AuthUnit) Apply(s Stamp, members []string, complete bool) {
	if h.Members == nil {
		h.Members = map[string]bool{}
	}
	if h.Partial == nil {
		h.Partial = map[string]Stamp{}
	}
	if complete {
		if h.HasComplete && s.Compare(h.Complete) <= 0 {
			return
		}
		h.HasComplete, h.Complete = true, s
		h.Members = map[string]bool{}
		for _, m := range members {
			h.Members[m] = true
		}
		for m, ps := range h.Partial {
			if ps.Compare(s) <= 0 {
				delete(h.Partial, m)
			}
		}
		return
	}
	if h.HasComplete && s.Compare(h.Complete) <= 0 {
		return
	}
	for _, m := range members {
		if ps, ok := h.Partial[m]; !ok || s.Compare(ps) > 0 {
			h.Partial[m] = s
		}
	}
}

// UnitFamilies returns the families an authoritative observation reports
// to, sorted: those of its authoritative edges and tags, plus (when it
// is complete) its covered rules.
func UnitFamilies(reported map[string][]string, complete bool, covered []string) []string {
	fams := slices.Collect(maps.Keys(reported))
	if complete {
		fams = append(fams, covered...)
	}
	slices.Sort(fams)
	return slices.Compact(fams)
}

// SetProfile adds (in) or removes p in a sorted profile list.
func SetProfile(list []string, p string, in bool) []string {
	i, found := slices.BinarySearch(list, p)
	switch {
	case in && !found:
		return slices.Insert(list, i, p)
	case !in && found:
		return slices.Delete(list, i, i+1)
	}
	return list
}

// tagRef names one tag of one target.
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

// authTx collects the records and tags one observation touched whose
// liveness or visibility may change.
type authTx struct {
	g           *Graph
	touchedEnt  map[string]bool
	touchedEdge map[string]bool
	tagBefore   map[tagRef]tagSnap
}

// snapTag remembers the visible content of a tag before its first change
// in the operation.
func (tx *authTx) snapTag(ref tagRef, r *record) {
	if _, ok := tx.tagBefore[ref]; ok {
		return
	}
	ts, ok := r.tags[tagKey{ref.ptype, ref.name}]
	if !ok || !ts.visible() {
		tx.tagBefore[ref] = tagSnap{}
		return
	}
	tx.tagBefore[ref] = tagSnap{visible: true, content: cloneObject(ts.content.Values)}
}

// applyAuthority reports the observation's authoritative edges and tags
// (by family) to the source's units and updates memberships.
//
// PORT: families and members are visited in sorted order (Go maps
// iterate randomly); the order only affects which records are touched
// first, never the result, but keep it for identical event ids.
func (tx *authTx) applyAuthority(src *entityState, p *prepared, units map[string][]string) {
	fams := UnitFamilies(units, p.auth.complete, p.auth.covered)
	if src.heads == nil {
		src.heads = map[string]*AuthUnit{}
	}
	for _, fam := range fams {
		complete := p.auth.complete && slices.Contains(p.auth.covered, fam)
		k := UnitKey(p.auth.profile, fam)
		h := src.heads[k]
		if h == nil {
			h = NewAuthUnit()
			src.heads[k] = h
		}
		before := h.Effective()
		h.Apply(p.stamp, units[fam], complete)
		after := h.Effective()
		all := slices.Collect(maps.Keys(before))
		for m := range after {
			all = append(all, m)
		}
		slices.Sort(all)
		for _, m := range slices.Compact(all) {
			if before[m] != after[m] {
				tx.setMember(src, p.auth.profile, m, after[m])
			}
		}
	}
}

// setMember adds or removes profile from a member's authIn.
func (tx *authTx) setMember(src *entityState, profile, m string, in bool) {
	if ptype, name, ok := ParseTagMember(m); ok {
		ref := tagRef{target: src.id, ptype: ptype, name: name}
		tx.snapTag(ref, &src.record)
		if ts, ok := src.tags[tagKey{ptype, name}]; ok {
			ts.authIn = SetProfile(ts.authIn, profile, in)
		}
		return
	}
	ed, ok := tx.g.edges[m]
	if !ok {
		return
	}
	was := len(ed.authIn) > 0
	ed.authIn = SetProfile(ed.authIn, profile, in)
	tx.touchedEdge[m] = true
	if is := len(ed.authIn) > 0; is != was {
		if t, ok := tx.g.entities[ed.to]; ok {
			if is {
				t.authRefs++
			} else {
				t.authRefs--
			}
			tx.touchedEnt[ed.to] = true
		}
	}
}

// settle recomputes the state of every touched record and emits the
// transitions: edges first, then entities, each in id order.
func (tx *authTx) settle(em *emitter) {
	for _, id := range slices.Sorted(maps.Keys(tx.touchedEdge)) {
		ed := tx.g.edges[id]
		if ed.settle() {
			if ed.state == StateActive {
				em.emit(ChangeEdgeAdded, "", id, nil)
			} else {
				em.emit(ChangeEdgeRemoved, "", id, nil)
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(tx.touchedEnt)) {
		e := tx.g.entities[id]
		if e.settle() {
			if e.state == StateActive {
				em.emit(ChangeEntityReactivated, id, "", nil)
			} else {
				em.emit(ChangeEntityStale, id, "", nil)
			}
		}
	}
}

// emitTags emits one tag_changed per touched tag whose visible content
// changed, in tagRef order (entity tags before edge tags, then target id,
// property type, name).
func (tx *authTx) emitTags(em *emitter) {
	refs := slices.SortedFunc(maps.Keys(tx.tagBefore), compareTagRef)
	for _, ref := range refs {
		var r *record
		if ref.edge {
			r = &tx.g.edges[ref.target].record
		} else {
			r = &tx.g.entities[ref.target].record
		}
		after := tagSnap{}
		if ts, ok := r.tags[tagKey{ref.ptype, ref.name}]; ok && ts.visible() {
			after = tagSnap{visible: true, content: ts.content.Values}
		}
		ops := TagPatch(tx.tagBefore[ref].visible, tx.tagBefore[ref].content, after.visible, after.content)
		if len(ops) == 0 {
			continue
		}
		ev := ChangeEvent{Type: ChangeTagChanged, TagType: ref.ptype, TagName: ref.name, Patch: ops}
		if ref.edge {
			ev.EdgeID = ref.target
		} else {
			ev.EntityID = ref.target
		}
		em.emitEvent(ev)
	}
}

// TagPatch returns the tag_changed patch between two visible states of a
// tag: nil when nothing visible changed, a root "add" when it appears, a
// root "remove" when it disappears, and field-level add / replace /
// remove ops in field order otherwise. Values are compared by canonical
// JSON.
func TagPatch(beforeVisible bool, before map[string]any, afterVisible bool, after map[string]any) []PatchOp {
	switch {
	case !beforeVisible && !afterVisible:
		return nil
	case !beforeVisible:
		return []PatchOp{{Op: "add", Path: "", Value: cloneObject(after)}}
	case !afterVisible:
		return []PatchOp{{Op: "remove", Path: ""}}
	}
	keys := slices.Collect(maps.Keys(before))
	for k := range after {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var ops []PatchOp
	for _, k := range slices.Compact(keys) {
		b, inB := before[k]
		a, inA := after[k]
		switch {
		case !inB:
			ops = append(ops, PatchOp{Op: "add", Path: pointer(k), Value: cloneValue(a)})
		case !inA:
			ops = append(ops, PatchOp{Op: "remove", Path: pointer(k)})
		case !sameJSON(a, b):
			ops = append(ops, PatchOp{Op: "replace", Path: pointer(k), Value: cloneValue(a)})
		}
	}
	return ops
}

func sameJSON(a, b any) bool {
	ab, errA := core.CanonicalJSON(a)
	bb, errB := core.CanonicalJSON(b)
	return errA == nil && errB == nil && bytes.Equal(ab, bb)
}

func entityBase(e *entityState) map[string]any {
	return map[string]any{"content": cloneObject(e.content.Values), "scope_status": string(e.scope)}
}
