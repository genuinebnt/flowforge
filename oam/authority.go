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
type authHead struct {
	hasComplete bool
	complete    Stamp
	members     map[string]bool  // the latest complete reading
	partial     map[string]Stamp // member -> newest partial reading newer than complete
}

// unitKey names a unit of a source entity.
func unitKey(profile, family string) string { return profile + "/" + family }

// tagMember is the member string of a tag in a unit.
func tagMember(ptype, name string) string { return "t:" + ptype + "/" + name }

// parseTagMember is the inverse of tagMember.
func parseTagMember(m string) (ptype, name string, ok bool) {
	rest, ok := strings.CutPrefix(m, "t:")
	if !ok {
		return "", "", false
	}
	return strings.Cut(rest, "/")
}

func newAuthHead() *authHead {
	return &authHead{members: map[string]bool{}, partial: map[string]Stamp{}}
}

func (h *authHead) effective() map[string]bool {
	out := maps.Clone(h.members)
	for m := range h.partial {
		out[m] = true
	}
	return out
}

// apply records one reading of the unit.
func (h *authHead) apply(s Stamp, members []string, complete bool) {
	if complete {
		if h.hasComplete && s.Compare(h.complete) <= 0 {
			return
		}
		h.hasComplete, h.complete = true, s
		h.members = map[string]bool{}
		for _, m := range members {
			h.members[m] = true
		}
		for m, ps := range h.partial {
			if ps.Compare(s) <= 0 {
				delete(h.partial, m)
			}
		}
		return
	}
	if h.hasComplete && s.Compare(h.complete) <= 0 {
		return
	}
	for _, m := range members {
		if ps, ok := h.partial[m]; !ok || s.Compare(ps) > 0 {
			h.partial[m] = s
		}
	}
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
func (tx *authTx) applyAuthority(src *entityState, p *prepared, units map[string][]string) {
	fams := slices.Collect(maps.Keys(units))
	if p.auth.complete {
		fams = append(fams, p.auth.covered...)
	}
	slices.Sort(fams)
	fams = slices.Compact(fams)
	if src.heads == nil {
		src.heads = map[string]*authHead{}
	}
	for _, fam := range fams {
		complete := p.auth.complete && slices.Contains(p.auth.covered, fam)
		k := unitKey(p.auth.profile, fam)
		h := src.heads[k]
		if h == nil {
			h = newAuthHead()
			src.heads[k] = h
		}
		before := h.effective()
		h.apply(p.stamp, units[fam], complete)
		after := h.effective()
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
	if ptype, name, ok := parseTagMember(m); ok {
		ref := tagRef{target: src.id, ptype: ptype, name: name}
		tx.snapTag(ref, &src.record)
		if ts, ok := src.tags[tagKey{ptype, name}]; ok {
			ts.authIn = setProfile(ts.authIn, profile, in)
		}
		return
	}
	ed, ok := tx.g.edges[m]
	if !ok {
		return
	}
	was := len(ed.authIn) > 0
	ed.authIn = setProfile(ed.authIn, profile, in)
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

// setProfile adds (in) or removes p in a sorted profile list.
func setProfile(list []string, p string, in bool) []string {
	i, found := slices.BinarySearch(list, p)
	switch {
	case in && !found:
		return slices.Insert(list, i, p)
	case !in && found:
		return slices.Delete(list, i, i+1)
	}
	return list
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
