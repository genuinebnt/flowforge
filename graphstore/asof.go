package graphstore

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/core/netnorm"
	"github.com/genuinebnt/flowforge/oam"
)

// AsOf reads (V2 §3.4) reconstruct a record from its change events up to
// a seq, starting from the empty graph (the "nearest snapshot" of the
// design is the empty one in this version; see OPEN_QUESTIONS.md):
//
//   - entity_added creates the entity from Base (content and scope
//     status), active;
//   - entity_changed applies its add/replace ops to the content;
//   - scope_status_changed sets the scope status;
//   - entity_stale / entity_reactivated set the state;
//   - edge_added with Base creates the edge (active); without Base it
//     reactivates it; edge_removed makes it stale;
//   - tag_changed applies its patch to the named tag (a root add creates
//     it, a root remove deletes it);
//   - content_changed changes nothing.
//
// Identity (asset type, key, relation, endpoints) never changes and is
// taken from the current row. Everything else (confidence, times, runs,
// SourceProperty tags, edge content) has no events and is left zero.

// AsOfState is the change-event-determined state of one record.
type AsOfState struct {
	Exists  bool
	Content map[string]any
	Scope   oam.ScopeStatus
	State   oam.State
	Tags    map[[2]string]map[string]any // (type, name) -> content
}

// ReplayRecord folds one record's change events (seq-ordered) into its
// AsOfState.
func ReplayRecord(evs []ChangeEvent) (AsOfState, error) {
	st := AsOfState{Tags: map[[2]string]map[string]any{}}
	for _, ev := range evs {
		switch ev.Type {
		case oam.ChangeEntityAdded:
			c, _ := ev.Base["content"].(map[string]any)
			s, _ := ev.Base["scope_status"].(string)
			st.Exists, st.Content, st.Scope, st.State = true, maps.Clone(c), oam.ScopeStatus(s), oam.StateActive
			if st.Content == nil {
				st.Content = map[string]any{}
			}
		case oam.ChangeEntityChanged:
			for _, op := range ev.Patch {
				k, err := topField(op.Path)
				if err != nil || st.Content == nil {
					return st, fmt.Errorf("graphstore: bad entity_changed op %+v", op)
				}
				st.Content[k] = op.Value
			}
		case oam.ChangeScopeStatusChanged:
			if len(ev.Patch) == 1 {
				s, _ := ev.Patch[0].Value.(string)
				st.Scope = oam.ScopeStatus(s)
			}
		case oam.ChangeEntityStale, oam.ChangeEdgeRemoved:
			st.State = oam.StateStale
		case oam.ChangeEntityReactivated:
			st.State = oam.StateActive
		case oam.ChangeEdgeAdded:
			if ev.Base != nil {
				st.Exists = true
			}
			st.State = oam.StateActive
		case oam.ChangeTagChanged:
			k := [2]string{ev.TagType, ev.TagName}
			v, err := applyTagPatch(st.Tags[k], ev.Patch)
			if err != nil {
				return st, err
			}
			if v == nil {
				delete(st.Tags, k)
			} else {
				st.Tags[k] = v
			}
		}
	}
	return st, nil
}

// topField decodes the RFC 6901 pointer of a top-level field.
func topField(path string) (string, error) {
	s, ok := strings.CutPrefix(path, "/")
	if !ok || strings.Contains(s, "/") {
		return "", fmt.Errorf("graphstore: patch path %q is not a top-level field", path)
	}
	return strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~"), nil
}

// applyTagPatch applies a tag_changed patch; nil means the tag is absent.
func applyTagPatch(cur map[string]any, ops []oam.PatchOp) (map[string]any, error) {
	cur = maps.Clone(cur)
	for _, op := range ops {
		if op.Path == "" {
			switch op.Op {
			case "add", "replace":
				m, ok := op.Value.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("graphstore: tag patch root value is not an object")
				}
				cur = maps.Clone(m)
			case "remove":
				cur = nil
			}
			continue
		}
		k, err := topField(op.Path)
		if err != nil || cur == nil {
			return nil, fmt.Errorf("graphstore: bad tag_changed op %+v", op)
		}
		if op.Op == "remove" {
			delete(cur, k)
		} else {
			cur[k] = op.Value
		}
	}
	return cur, nil
}

// EntityAsOf applies an AsOfState to the entity's current identity.
func EntityAsOf(cur Entity, st AsOfState, withTags bool) Entity {
	out := Entity{ID: cur.ID, TenantID: cur.TenantID, ProgramID: cur.ProgramID, AssetType: cur.AssetType, Key: cur.Key,
		Content: st.Content, ScopeStatus: st.Scope, State: st.State}
	if withTags {
		out.Tags = asOfTags(cur.TenantID, TargetEntity, cur.ID, st)
	}
	return out
}

// EdgeAsOf applies an AsOfState to the edge's current identity.
func EdgeAsOf(cur Edge, st AsOfState) Edge {
	return Edge{ID: cur.ID, TenantID: cur.TenantID, ProgramID: cur.ProgramID, RelationType: cur.RelationType, Label: cur.Label,
		Discriminator: cur.Discriminator, FromEntityID: cur.FromEntityID, ToEntityID: cur.ToEntityID, State: st.State}
}

func asOfTags(tenant string, kind TargetKind, target string, st AsOfState) []Tag {
	keys := slices.SortedFunc(maps.Keys(st.Tags), func(a, b [2]string) int { return cmp.Or(cmp.Compare(a[0], b[0]), cmp.Compare(a[1], b[1])) })
	out := make([]Tag, 0, len(keys))
	for _, k := range keys {
		out = append(out, Tag{ID: core.PublicID(core.PrefixTag, TagUUID(tenant, kind, target, k[0], k[1])), TargetKind: kind, TargetID: target,
			PropertyType: k[0], Name: k[1], Content: st.Tags[k]})
	}
	return out
}

// ValidateAsOfSeq checks an AsOf seq.
func ValidateAsOfSeq(seq int64) error {
	if seq < 1 {
		return InvalidArgument("seq", "seq must be at least 1")
	}
	return nil
}

// ValidateNeighborsAsOf checks a NeighborsAsOfRequest.
func ValidateNeighborsAsOf(req NeighborsAsOfRequest) (NeighborsAsOfRequest, error) {
	if err := ValidateAsOfSeq(req.Seq); err != nil {
		return req, err
	}
	if !req.Since.IsZero() {
		return req, InvalidArgument("since", "since is not supported as of a seq (last_seen is not reconstructed)")
	}
	n, err := ValidateNeighbors(req.NeighborsRequest)
	req.NeighborsRequest = n
	return req, err
}

// EventSource is what AsOf reads need from a store besides a Reader:
// the change events of given records with seq <= at, in seq order.
type EventSource interface {
	EntityEvents(ctx context.Context, ids []string, at int64) (map[string][]ChangeEvent, error)
	EdgeEvents(ctx context.Context, ids []string, at int64) (map[string][]ChangeEvent, error)
}

// AsOfReader is the traversal Reader of a program as of a seq: it loads
// current rows from cur and replays their events from ev, dropping
// records that did not exist yet.
type AsOfReader struct {
	Cur Reader
	Ev  EventSource
	At  int64
}

// Entities implements Reader.
func (r AsOfReader) Entities(ctx context.Context, ids []string) (map[string]Entity, error) {
	cur, err := r.Cur.Entities(ctx, ids)
	if err != nil {
		return nil, err
	}
	evs, err := r.Ev.EntityEvents(ctx, slices.Sorted(maps.Keys(cur)), r.At)
	if err != nil {
		return nil, err
	}
	out := make(map[string]Entity, len(cur))
	for id, e := range cur {
		st, err := ReplayRecord(evs[id])
		if err != nil {
			return nil, err
		}
		if st.Exists {
			out[id] = EntityAsOf(e, st, false)
		}
	}
	return out, nil
}

// Edges implements Reader.
func (r AsOfReader) Edges(ctx context.Context, ids []string, out, in bool, labels []string) ([]Edge, error) {
	cur, err := r.Cur.Edges(ctx, ids, out, in, labels)
	if err != nil {
		return nil, err
	}
	eids := make([]string, len(cur))
	for i, e := range cur {
		eids[i] = e.ID
	}
	evs, err := r.Ev.EdgeEvents(ctx, eids, r.At)
	if err != nil {
		return nil, err
	}
	res := make([]Edge, 0, len(cur))
	for _, e := range cur {
		st, err := ReplayRecord(evs[e.ID])
		if err != nil {
			return nil, err
		}
		if st.Exists {
			res = append(res, EdgeAsOf(e, st))
		}
	}
	return res, nil
}

// GetAsOf reconstructs one current entity as of at (NOT_FOUND when it
// did not exist yet).
func GetAsOf(ctx context.Context, ev EventSource, cur Entity, at int64, withTags bool) (Entity, error) {
	evs, err := ev.EntityEvents(ctx, []string{cur.ID}, at)
	if err != nil {
		return Entity{}, err
	}
	st, err := ReplayRecord(evs[cur.ID])
	if err != nil {
		return Entity{}, Internal("replay", err)
	}
	if !st.Exists {
		return Entity{}, NotFound("entity", cur.ID)
	}
	return EntityAsOf(cur, st, withTags), nil
}

// HolderHost returns the host an entity stands for when it points at a
// shared node: the host of a URL, File or Endpoint, the name of an FQDN,
// the address of an IPAddress, and "entity:<id>" for anything else.
func HolderHost(e Entity) string {
	str := func(k string) string { s, _ := e.Content[k].(string); return s }
	switch e.AssetType {
	case oam.TypeURL, oam.TypeFile, oam.TypeEndpoint:
		u := str("url")
		if e.AssetType == oam.TypeEndpoint {
			u = str("url_template")
		}
		if i := strings.Index(u, "://"); i > 0 {
			auth := u[i+3:]
			if j := strings.IndexAny(auth, "/?#"); j >= 0 {
				auth = auth[:j]
			}
			if pu, err := netnorm.ParseURL(u[:i+3] + auth + "/"); err == nil {
				return "host:" + pu.Host.String()
			}
		}
	case oam.TypeFQDN:
		return "host:" + str("name")
	case oam.TypeIPAddress:
		return "host:" + str("address")
	}
	return "entity:" + e.ID
}

// ComputeCommonness counts the distinct active sources pointing at e over
// active edges and the distinct hosts they stand for (HolderHost).
func ComputeCommonness(ctx context.Context, r Reader, e Entity) (Commonness, error) {
	edges, err := r.Edges(ctx, []string{e.ID}, false, true, nil)
	if err != nil {
		return Commonness{}, Internal("load edges", err)
	}
	var from []string
	for _, ed := range edges {
		if ed.State == oam.StateActive && ed.ToEntityID == e.ID && ed.FromEntityID != e.ID {
			from = append(from, ed.FromEntityID)
		}
	}
	slices.Sort(from)
	from = slices.Compact(from)
	ents, err := r.Entities(ctx, from)
	if err != nil {
		return Commonness{}, Internal("load entities", err)
	}
	hosts := map[string]bool{}
	sources := 0
	for _, id := range from {
		if s, ok := ents[id]; ok && s.State == oam.StateActive {
			sources++
			hosts[HolderHost(s)] = true
		}
	}
	return Commonness{EntityID: e.ID, Sources: sources, Holders: len(hosts), Common: len(hosts) > CommonThreshold}, nil
}
