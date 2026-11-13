package contentstore

import (
	"container/list"
	"context"
	"sync"

	"github.com/genuinebnt/flowforge/core"
)

// plainBlobs are a snapshot's four blobs, decoded: the capture record
// (headers_ref), the redacted body (body_manifest), the main normalized
// text (normalized_manifest) and the features record (features_ref).
type plainBlobs struct {
	Record, Body, Normalized, Features []byte
}

func (p plainBlobs) size() int {
	return len(p.Record) + len(p.Body) + len(p.Normalized) + len(p.Features)
}

// dicts are the raw dictionaries of each blob under the delta codec: the
// base's same blob, then (for the derived blobs) this snapshot's body or
// normalized text, which they restate.
func dicts(base *plainBlobs, own plainBlobs) [4][]byte {
	var b plainBlobs
	if base != nil {
		b = *base
	}
	cat := func(a, c []byte) []byte { return append(append(make([]byte, 0, len(a)+len(c)), a...), c...) }
	return [4][]byte{b.Record, b.Body, cat(b.Normalized, own.Body), cat(b.Features, own.Normalized)}
}

// encodeSnapshotBlobs encodes a snapshot's blobs for the codec.
func encodeSnapshotBlobs(codec BlobCodec, base *plainBlobs, p plainBlobs) (plainBlobs, error) {
	if codec == CodecChunk {
		return p, nil
	}
	d := dicts(base, p)
	var out plainBlobs
	var err error
	for i, pair := range [4]struct {
		dst *[]byte
		src []byte
	}{{&out.Record, p.Record}, {&out.Body, p.Body}, {&out.Normalized, p.Normalized}, {&out.Features, p.Features}} {
		if *pair.dst, err = encodeBlob(pair.src, d[i]); err != nil {
			return plainBlobs{}, err
		}
	}
	return out, nil
}

// putSnapshotBlobs stores encoded blobs and sets the snapshot's refs; it
// returns the record bytes added.
func putSnapshotBlobs(ctx context.Context, tc *tenantChunks, snap *Snapshot, enc plainBlobs) (int64, error) {
	var added int64
	for _, pair := range [4]struct {
		ref  *string
		data []byte
	}{{&snap.HeadersRef, enc.Record}, {&snap.BodyManifest, enc.Body}, {&snap.NormalizedManifest, enc.Normalized}, {&snap.FeaturesRef, enc.Features}} {
		ref, n, err := tc.put(ctx, pair.data)
		if err != nil {
			return added, err
		}
		*pair.ref = ref
		added += n
	}
	return added, nil
}

// snapshotPlain decodes a snapshot's blobs, following its delta chain
// (bounded by the keyframe interval) through the plain-blob cache.
func (s *Store) snapshotPlain(ctx context.Context, tc *tenantChunks, snap Snapshot) (plainBlobs, error) {
	if p, ok := s.cache.get(snap.Tenant, snap.ID); ok {
		return p, nil
	}
	var raw plainBlobs
	for _, pair := range [4]struct {
		dst *[]byte
		ref string
	}{{&raw.Record, snap.HeadersRef}, {&raw.Body, snap.BodyManifest}, {&raw.Normalized, snap.NormalizedManifest}, {&raw.Features, snap.FeaturesRef}} {
		b, err := tc.get(ctx, pair.ref)
		if err != nil {
			return plainBlobs{}, err
		}
		*pair.dst = b
	}
	if snap.Codec == CodecChunk.String() || snap.Codec == "" {
		s.cache.put(snap.Tenant, snap.ID, raw)
		return raw, nil
	}
	var base *plainBlobs
	if !snap.DeltaBase.IsZero() {
		if snap.DeltaDepth > s.cfg.KeyframeInterval+1 {
			return plainBlobs{}, integrity("delta chain of %s deeper than %d", snap.PublicID(), s.cfg.KeyframeInterval)
		}
		bs, err := s.GetSnapshot(ctx, snap.Tenant, snap.DeltaBase)
		if err != nil {
			return plainBlobs{}, integrity("delta base of %s: %v", snap.PublicID(), err)
		}
		bp, err := s.snapshotPlain(ctx, tc, bs)
		if err != nil {
			return plainBlobs{}, err
		}
		base = &bp
	}
	var out plainBlobs
	var err error
	var b plainBlobs
	if base != nil {
		b = *base
	}
	if out.Record, err = decodeBlob(raw.Record, b.Record); err != nil {
		return plainBlobs{}, err
	}
	if out.Body, err = decodeBlob(raw.Body, b.Body); err != nil {
		return plainBlobs{}, err
	}
	d := dicts(base, plainBlobs{Body: out.Body})
	if out.Normalized, err = decodeBlob(raw.Normalized, d[2]); err != nil {
		return plainBlobs{}, err
	}
	d = dicts(base, plainBlobs{Body: out.Body, Normalized: out.Normalized})
	if out.Features, err = decodeBlob(raw.Features, d[3]); err != nil {
		return plainBlobs{}, err
	}
	s.cache.put(snap.Tenant, snap.ID, out)
	return out, nil
}

// encodeDiffBlob encodes a diff record: FFZB without a dictionary under
// the delta codec (whole-record zstd 19), plain under the chunk codec.
// Readers tell them apart by the magic (records are JSON objects).
func encodeDiffBlob(codec BlobCodec, b []byte) ([]byte, error) {
	if codec == CodecChunk {
		return b, nil
	}
	return encodeBlob(b, nil)
}

func decodeDiffBlob(b []byte) ([]byte, error) {
	if isBlob(b) {
		return decodeBlob(b, nil)
	}
	return b, nil
}

// blobCache is an LRU of decoded snapshot blobs, bounded by bytes. A
// snapshot's plain blobs change only when it is re-derived (a keyframe
// rewrite changes only their encoding), which drops its entry; the cache
// is per Store, which is why one process writes a tenant (ChunkStores).
//
// PORT: container/list stores interface values; a Rust port uses an
// indexed LRU (e.g. the lru crate). The cache never affects output.
type blobCache struct {
	mu    sync.Mutex
	max   int
	used  int
	order *list.List
	items map[[2]core.UUID]*list.Element
}

type cacheEntry struct {
	key [2]core.UUID
	val plainBlobs
}

func newBlobCache(maxBytes int) *blobCache {
	return &blobCache{max: maxBytes, order: list.New(), items: map[[2]core.UUID]*list.Element{}}
}

func (c *blobCache) get(tenant, id core.UUID) (plainBlobs, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[[2]core.UUID{tenant, id}]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*cacheEntry).val, true
	}
	return plainBlobs{}, false
}

func (c *blobCache) put(tenant, id core.UUID, v plainBlobs) {
	if v.size() > c.max/4 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := [2]core.UUID{tenant, id}
	if e, ok := c.items[k]; ok {
		c.order.MoveToFront(e)
		return
	}
	c.items[k] = c.order.PushFront(&cacheEntry{key: k, val: v})
	c.used += v.size()
	for c.used > c.max && c.order.Len() > 0 {
		e := c.order.Back()
		ce := e.Value.(*cacheEntry)
		c.order.Remove(e)
		delete(c.items, ce.key)
		c.used -= ce.val.size()
	}
}

// drop removes a snapshot's entry (its derived blobs were rewritten).
func (c *blobCache) drop(tenant, id core.UUID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := [2]core.UUID{tenant, id}
	if e, ok := c.items[k]; ok {
		c.order.Remove(e)
		delete(c.items, k)
		c.used -= e.Value.(*cacheEntry).val.size()
	}
}
