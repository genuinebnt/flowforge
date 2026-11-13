// Package similarity is the similarity service (spec 3.1, 22.9) over
// vecindex.Registry: one index per (tenant, program, model). It consumes
// graph.changes to embed new and changed entities and keep the index
// watermark; SearchByText and HybridSearch embed through the Embedder port.
// Every hit passes through the scope service: out-of-scope hits are dropped
// unless include_out_of_scope is set by an analyst or above.
//
// The bundled HashEmbedder is a deterministic feature-hashing embedder for
// dev and tests. It is NOT semantic: it captures shared tokens and
// character trigrams only. A semantic embedder is G-VEC-05 (v4).
package similarity

import (
	"context"
	"encoding/binary"
	"hash/fnv"
	"log/slog"
	"math"
	"strings"
	"sync"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	simv1 "github.com/genuinebnt/flowforge/gen/flowforge/similarity/v1"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/graphstore"
	"github.com/genuinebnt/flowforge/oam"
	"github.com/genuinebnt/flowforge/scope"
	"github.com/genuinebnt/flowforge/services/internal/convert"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	scopesvc "github.com/genuinebnt/flowforge/services/scope"
	"github.com/genuinebnt/flowforge/vecindex"
)

// Embedder computes text embeddings (spec 24.4 Embedder port).
type Embedder interface {
	Model() (id, version string, dim int)
	Embed(ctx context.Context, text string) ([]float32, error)
}

// HashEmbedder is the deterministic dev embedder: tokens and character
// trigrams hashed (FNV-1a) into Dim signed buckets, L2-normalized. Not
// semantic; see the package documentation.
type HashEmbedder struct{ Dim int }

// DefaultModel is the HashEmbedder's model id.
const DefaultModel = "ff-hash-embed"

// Model implements Embedder.
func (h HashEmbedder) Model() (string, string, int) { return DefaultModel, "1", h.dim() }

func (h HashEmbedder) dim() int {
	if h.Dim <= 0 {
		return 256
	}
	return h.Dim
}

// Embed implements Embedder.
func (h HashEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	d := h.dim()
	v := make([]float32, d)
	add := func(f string, w float32) {
		hs := fnv.New64a()
		_, _ = hs.Write([]byte(f))
		x := hs.Sum64()
		sign := float32(1)
		if x&1 == 1 {
			sign = -1
		}
		v[(x>>1)%uint64(d)] += sign * w
	}
	for _, tok := range vecindex.Tokens(strings.ToLower(text)) {
		add("t:"+tok, 1)
	}
	for _, g := range vecindex.CharNGrams(strings.ToLower(text), 3) {
		add("g:"+g, 0.5)
	}
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	if n == 0 {
		v[0] = 1
		return v, nil
	}
	inv := float32(1 / math.Sqrt(n))
	for i := range v {
		v[i] *= inv
	}
	return v, nil
}

// Scope is what the service asks the scope service.
type Scope interface {
	CheckBatch(ctx context.Context, ts []scopesvc.Target) ([]scope.Decision, error)
}

// Options configure the server.
type Options struct {
	Embedder Embedder
	Graph    pb.GraphServiceClient
	Scope    Scope
	Logger   *slog.Logger
}

// Server implements flowforge.similarity.v1.Similarity.
type Server struct {
	simv1.UnimplementedSimilarityServer
	o   Options
	reg *vecindex.Registry

	mu      sync.Mutex
	vectors map[vecindex.Key]map[core.UUID][]float32
	hashes  map[vecindex.Key]map[core.UUID]string
	seqs    map[vecindex.Key]map[core.UUID]uint64
	marks   map[vecindex.Key]uint64
	models  map[vecindex.Key]string // model version
}

// NewServer returns a similarity service.
func NewServer(o Options) *Server {
	if o.Embedder == nil {
		o.Embedder = HashEmbedder{}
	}
	if o.Logger == nil {
		o.Logger = platform.DiscardLogger()
	}
	return &Server{o: o, reg: vecindex.NewRegistry(), vectors: map[vecindex.Key]map[core.UUID][]float32{},
		hashes: map[vecindex.Key]map[core.UUID]string{}, seqs: map[vecindex.Key]map[core.UUID]uint64{},
		marks: map[vecindex.Key]uint64{}, models: map[vecindex.Key]string{}}
}

func (s *Server) key(ctx context.Context, sc *simv1.Scope, model string) (vecindex.Key, platform.Identity, error) {
	id, err := platform.MustIdentity(ctx)
	if err != nil {
		return vecindex.Key{}, id, err
	}
	if t := sc.GetTenantId(); t != "" {
		u, err := core.ParsePublicID(core.PrefixTenant, t)
		if err != nil || u != id.Tenant {
			return vecindex.Key{}, id, core.Errorf(core.KindNotFound, core.ReasonNotFound, "index not found")
		}
	}
	p, err := convert.ParseID(core.PrefixProgram, "scope.program_id", sc.GetProgramId())
	if err != nil {
		return vecindex.Key{}, id, err
	}
	if model == "" {
		model, _, _ = s.o.Embedder.Model()
	}
	return vecindex.Key{Tenant: id.Tenant, Program: p, Model: model}, id, nil
}

// upsert applies a vector unless its seq is not newer or its content hash
// is unchanged; it creates the index on first use (cosine, the vector's
// dimension).
func (s *Server) upsert(k vecindex.Key, ent core.UUID, vec []float32, hash, version string, seq uint64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vectors[k] == nil {
		idx, err := vecindex.NewBruteForce(vecindex.Cosine, len(vec))
		if err != nil {
			return false, convert.Invalid("vector", "%v", err)
		}
		if err := s.reg.Register(k, idx); err != nil {
			return false, err
		}
		s.vectors[k], s.hashes[k], s.seqs[k], s.models[k] = map[core.UUID][]float32{}, map[core.UUID]string{}, map[core.UUID]uint64{}, version
	}
	if last, ok := s.seqs[k][ent]; ok && seq <= last && seq != 0 {
		return false, nil
	}
	if hash != "" && s.hashes[k][ent] == hash {
		return false, nil
	}
	if seq == 0 {
		seq = s.seqs[k][ent] + 1
	}
	if err := s.reg.Upsert(k, ent, vec, seq); err != nil {
		return false, convert.Invalid("vector", "%v", err)
	}
	s.vectors[k][ent] = append([]float32(nil), vec...)
	s.hashes[k][ent] = hash
	s.seqs[k][ent] = seq
	s.marks[k] = max(s.marks[k], seq)
	return true, nil
}

// Upsert implements Similarity (idempotent per (entity, model)).
func (s *Server) Upsert(ctx context.Context, req *simv1.UpsertRequest) (*simv1.UpsertResponse, error) {
	k, _, err := s.key(ctx, req.GetScope(), req.GetModelId())
	if err != nil {
		return nil, err
	}
	ent, err := convert.ParseID(core.PrefixEntity, "entity_id", req.GetEntityId())
	if err != nil {
		return nil, err
	}
	if len(req.GetVector()) == 0 {
		return nil, convert.Invalid("vector", "vector is required")
	}
	ok, err := s.upsert(k, ent, req.GetVector(), req.GetContentHash(), req.GetModelVersion(), req.GetSourceSeq())
	if err != nil {
		return nil, err
	}
	return &simv1.UpsertResponse{Applied: ok}, nil
}

// Delete implements Similarity.
func (s *Server) Delete(ctx context.Context, req *simv1.DeleteRequest) (*simv1.DeleteResponse, error) {
	k, _, err := s.key(ctx, req.GetScope(), req.GetModelId())
	if err != nil {
		return nil, err
	}
	ent, err := convert.ParseID(core.PrefixEntity, "entity_id", req.GetEntityId())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.vectors[k] == nil {
		return &simv1.DeleteResponse{}, nil
	}
	seq := req.GetSourceSeq()
	if seq == 0 {
		seq = s.seqs[k][ent] + 1
	}
	if err := s.reg.Delete(k, ent, seq); err != nil {
		return nil, err
	}
	delete(s.vectors[k], ent)
	delete(s.hashes[k], ent)
	s.seqs[k][ent] = max(s.seqs[k][ent], seq)
	return &simv1.DeleteResponse{}, nil
}

func (s *Server) index(k vecindex.Key) (vecindex.Index, error) {
	idx, err := s.reg.Lookup(k)
	if err != nil {
		return nil, core.Errorf(core.KindNotFound, core.ReasonNotFound, "no index for model %s", k.Model)
	}
	return idx, nil
}

func params(p *simv1.SearchParams) (vecindex.SearchParams, int, error) {
	k := int(p.GetK())
	if k == 0 {
		k = 10
	}
	if k < 1 || k > vecindex.MaxK {
		return vecindex.SearchParams{}, 0, convert.Invalid("params.k", "k must be 1..100")
	}
	l := int(p.GetSearchListSize())
	if l != 0 && l < k {
		return vecindex.SearchParams{}, 0, convert.Invalid("params.search_list_size", "search_list_size must be >= k")
	}
	// Over-fetch for the post-filters (scope, asset type, tags, last_seen).
	fetch := min(vecindex.MaxK, 4*k)
	return vecindex.SearchParams{K: fetch, L: max(l, fetch)}, k, nil
}

// finish post-filters hits by entity metadata and scope (spec 22.7, 22.9).
func (s *Server) finish(ctx context.Context, id platform.Identity, k vecindex.Key, hits []vecindex.Hit, p *simv1.SearchParams, want int, exclude core.UUID) (*simv1.SearchResponse, error) {
	if p.GetIncludeOutOfScope() && !id.AtLeast(auth.RoleAnalyst) && !id.IsService() {
		return nil, core.Errorf(core.KindPermissionDenied, core.ReasonForbiddenRole, "include_out_of_scope needs the analyst role")
	}
	f := p.GetFilter()
	type cand struct {
		h   vecindex.Hit
		t   scopesvc.Target
		has bool
	}
	var cands []cand
	gctx := platform.AsService(ctx, id.Tenant, "similarity")
	for _, h := range hits {
		if h.ID == exclude {
			continue
		}
		c := cand{h: h}
		if s.o.Graph != nil {
			e, err := s.o.Graph.GetEntity(gctx, &pb.GetEntityRequest{EntityId: core.PublicID(core.PrefixEntity, h.ID), IncludeTags: len(f.GetTags()) > 0})
			if err != nil {
				continue // deleted, or not visible to the tenant: never returned (the index is per program)
			}
			if len(f.GetAssetTypes()) > 0 && !contains(f.GetAssetTypes(), e.GetAssetType()) {
				continue
			}
			if t := convert.Time(f.GetLastSeenAfter()); !t.IsZero() && convert.Time(e.GetLastSeen()).Before(t) {
				continue
			}
			if len(f.GetTags()) > 0 && !hasTags(e, f.GetTags()) {
				continue
			}
			if a, err := oam.JoinAsset(e.GetAssetType(), convert.StructToMap(e.GetContent()), oam.Lenient); err == nil {
				if tgt, kind, ok := graphstore.ScopeTarget(a); ok {
					c.t = scopesvc.Target{Tenant: id.Tenant, Program: k.Program, Target: tgt, Kind: kind, Class: core.ClassPassive}
					c.has = true
				}
			}
		}
		cands = append(cands, c)
	}
	var ts []scopesvc.Target
	for _, c := range cands {
		if c.has {
			ts = append(ts, c.t)
		}
	}
	var ds []scope.Decision
	if len(ts) > 0 && s.o.Scope != nil {
		var err error
		if ds, err = s.o.Scope.CheckBatch(ctx, ts); err != nil {
			ds = nil // fail closed: nothing is in scope
		}
	}
	out := &simv1.SearchResponse{SegmentsSearched: 1}
	s.mu.Lock()
	out.IndexWatermarkSeq = s.marks[k]
	s.mu.Unlock()
	di := 0
	for _, c := range cands {
		in := false
		if c.has {
			in = di < len(ds) && ds[di].Allowed()
			di++
		}
		if !in && !p.GetIncludeOutOfScope() {
			continue
		}
		dist := c.h.Distance
		if math.IsInf(float64(dist), 0) {
			dist = float32(math.MaxFloat32)
		}
		out.Hits = append(out.Hits, &simv1.Hit{EntityId: core.PublicID(core.PrefixEntity, c.h.ID), Distance: dist, Score: c.h.Score, InScope: in})
		if len(out.Hits) == want {
			break
		}
	}
	return out, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func hasTags(e *pb.Entity, want []string) bool {
	for _, w := range want {
		found := false
		for _, t := range e.GetTags() {
			if t.GetName() == w || t.GetPropertyType()+":"+t.GetName() == w {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (s *Server) search(ctx context.Context, sc *simv1.Scope, model string, q []float32, p *simv1.SearchParams, exclude core.UUID) (*simv1.SearchResponse, error) {
	k, id, err := s.key(ctx, sc, model)
	if err != nil {
		return nil, err
	}
	idx, err := s.index(k)
	if err != nil {
		return nil, err
	}
	vp, want, err := params(p)
	if err != nil {
		return nil, err
	}
	hits, err := idx.Search(q, vp)
	if err != nil {
		return nil, convert.Invalid("vector", "%v", err)
	}
	return s.finish(ctx, id, k, hits, p, want, exclude)
}

// SearchByVector implements Similarity.
func (s *Server) SearchByVector(ctx context.Context, req *simv1.SearchByVectorRequest) (*simv1.SearchResponse, error) {
	return s.search(ctx, req.GetScope(), req.GetModelId(), req.GetVector(), req.GetParams(), core.Nil)
}

// SearchByEntity implements Similarity ("find assets like this one").
func (s *Server) SearchByEntity(ctx context.Context, req *simv1.SearchByEntityRequest) (*simv1.SearchResponse, error) {
	k, _, err := s.key(ctx, req.GetScope(), req.GetModelId())
	if err != nil {
		return nil, err
	}
	ent, err := convert.ParseID(core.PrefixEntity, "entity_id", req.GetEntityId())
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	v, ok := s.vectors[k][ent]
	s.mu.Unlock()
	if !ok {
		return nil, core.Errorf(core.KindNotFound, core.ReasonNotFound, "entity has no embedding for this model")
	}
	return s.search(ctx, req.GetScope(), req.GetModelId(), v, req.GetParams(), ent)
}

// SearchByText implements Similarity: the text is embedded, then searched.
func (s *Server) SearchByText(ctx context.Context, req *simv1.SearchByTextRequest) (*simv1.SearchResponse, error) {
	if req.GetText() == "" {
		return nil, convert.Invalid("text", "text is required")
	}
	q, err := s.o.Embedder.Embed(ctx, req.GetText())
	if err != nil {
		return nil, core.Errorf(core.KindUnavailable, core.ReasonUnavailable, "embedding failed")
	}
	return s.search(ctx, req.GetScope(), req.GetModelId(), q, req.GetParams(), core.Nil)
}

// HybridSearch implements Similarity: reciprocal rank fusion of the vector
// search and the graph's trigram search (spec 22.6).
func (s *Server) HybridSearch(ctx context.Context, req *simv1.HybridSearchRequest) (*simv1.SearchResponse, error) {
	k, id, err := s.key(ctx, req.GetScope(), req.GetModelId())
	if err != nil {
		return nil, err
	}
	idx, err := s.index(k)
	if err != nil {
		return nil, err
	}
	vp, want, err := params(req.GetParams())
	if err != nil {
		return nil, err
	}
	q, err := s.o.Embedder.Embed(ctx, req.GetText())
	if err != nil {
		return nil, core.Errorf(core.KindUnavailable, core.ReasonUnavailable, "embedding failed")
	}
	var lexical []core.UUID
	if s.o.Graph != nil {
		res, err := s.o.Graph.Search(platform.AsService(ctx, id.Tenant, "similarity"), &pb.SearchRequest{ProgramId: core.PublicID(core.PrefixProgram, k.Program),
			Query: req.GetText(), Limit: int32(vp.K), Fuzzy: true})
		if err == nil {
			for _, h := range res.GetHits() {
				if u, err := core.ParsePublicID(core.PrefixEntity, h.GetEntity().GetId()); err == nil {
					lexical = append(lexical, u)
				}
			}
		}
	}
	hits, err := vecindex.HybridSearch(idx, q, vp, lexical, int(req.GetRrfK()))
	if err != nil {
		return nil, convert.Invalid("params", "%v", err)
	}
	return s.finish(ctx, id, k, hits, req.GetParams(), want, core.Nil)
}

// GetIndexInfo implements Similarity.
func (s *Server) GetIndexInfo(ctx context.Context, req *simv1.GetIndexInfoRequest) (*simv1.IndexInfo, error) {
	k, _, err := s.key(ctx, req.GetScope(), req.GetModelId())
	if err != nil {
		return nil, err
	}
	idx, err := s.index(k)
	if err != nil {
		return nil, err
	}
	in := idx.Info()
	s.mu.Lock()
	ver, wm := s.models[k], s.marks[k]
	s.mu.Unlock()
	return &simv1.IndexInfo{ModelId: k.Model, ModelVersion: ver, Dimension: uint32(in.Dimension), Metric: simv1.Metric(in.Metric),
		Vectors: in.Vectors, Tombstones: in.Tombstones, Segments: uint32(in.Segments), MemtableVectors: in.MemtableVectors,
		RamBytes: in.RAMBytes, DiskBytes: in.DiskBytes, WatermarkSeq: max(wm, in.WatermarkSeq)}, nil
}

// HandleChange is the graph.changes consumer: added, changed and
// reactivated entities are embedded (their display text, graphstore
// SearchText) and upserted with the change seq; the watermark follows.
// Upserts are ordered by seq, so redeliveries are no-ops.
func (s *Server) HandleChange(ctx context.Context, env *pb.EventEnvelope) error {
	c := env.GetGraphChange()
	if c == nil || c.GetEntity() == nil {
		return nil
	}
	tenant, err := convert.EnvelopeTenant(env)
	if err != nil {
		return nil
	}
	program := convert.EnvelopeProgram(env)
	ent, err := core.ParsePublicID(core.PrefixEntity, c.GetEntityId())
	if err != nil || program.IsZero() {
		return nil
	}
	model, version, _ := s.o.Embedder.Model()
	k := vecindex.Key{Tenant: tenant, Program: program, Model: model}
	switch c.GetType() {
	case string(oam.ChangeEntityAdded), "entity_changed", "entity_reactivated":
	default:
		s.mu.Lock()
		if s.vectors[k] != nil {
			s.marks[k] = max(s.marks[k], uint64(c.GetSeq()))
		}
		s.mu.Unlock()
		return nil
	}
	text := graphstore.SearchText(convert.StructToMap(c.GetEntity().GetContent()))
	if text == "" {
		text = c.GetEntity().GetKey()
	}
	v, err := s.o.Embedder.Embed(ctx, c.GetEntity().GetAssetType()+" "+text)
	if err != nil {
		return err
	}
	var hb [8]byte
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	binary.BigEndian.PutUint64(hb[:], h.Sum64())
	_, err = s.upsert(k, ent, v, string(hb[:]), version, uint64(c.GetSeq()))
	return err
}
