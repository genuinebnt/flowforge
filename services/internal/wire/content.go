package wire

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/grpc"

	"github.com/genuinebnt/flowforge/chunkstore"
	"github.com/genuinebnt/flowforge/chunkstore/fsstore"
	"github.com/genuinebnt/flowforge/contentstore"
	contentpg "github.com/genuinebnt/flowforge/contentstore/pg"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/bus"
	"github.com/genuinebnt/flowforge/textindex"

	contentsvc "github.com/genuinebnt/flowforge/services/content"
)

// ContentOptions configure the content service.
type ContentOptions struct {
	// Dir holds the chunk store (Dir/chunks) and the text indexes
	// (Dir/index/<tenant>/<program>); "" keeps both in memory.
	Dir string
	// KEK is the 32-byte key-encryption key (hex or base64) and KEKID its
	// name; empty generates an ephemeral development key (refused in prod).
	KEK, KEKID string
	Prod       bool
	// Limits are the default per-tenant quotas (zero: unlimited).
	Limits contentstore.Limits
	// Codec, IndexMode: pluggable algorithms (docs/ALGORITHMS.md).
	Codec     contentstore.BlobCodec
	IndexMode contentstore.IndexMode
	// IndexLatest also indexes each key's latest normalized snapshot.
	IndexLatest bool
	// SweepInterval is the retention/GC (and batched indexing) pass
	// interval (default 1 h; negative disables).
	SweepInterval time.Duration
	// IndexInterval is the batched indexing interval (default 5 s).
	IndexInterval time.Duration
	// Retention overrides the default retention policy.
	Retention contentstore.RetentionPolicy
}

// parseKey decodes a 32-byte key given as hex or base64.
func parseKey(s string) ([32]byte, error) {
	var k [32]byte
	s = strings.TrimSpace(s)
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 32 {
		if b, err = base64.StdEncoding.DecodeString(s); err != nil || len(b) != 32 {
			return k, errors.New("content KEK must be 32 bytes, hex or base64")
		}
	}
	copy(k[:], b)
	return k, nil
}

// Content builds the content service. The returned repo is the outbox
// source in memory mode (nil with Postgres).
func Content(env *Env, o ContentOptions) (*Component, *contentsvc.Server, error) {
	kek := &contentstore.LocalKEK{KeyID: o.KEKID, Rng: core.SystemRng{}}
	if kek.KeyID == "" {
		kek.KeyID = "dev"
	}
	switch {
	case o.KEK != "":
		k, err := parseKey(o.KEK)
		if err != nil {
			return nil, nil, err
		}
		kek.Key = k
	case o.Prod:
		return nil, nil, errors.New("content: FF_CONTENT_KEK is required in prod")
	default:
		// Ephemeral development key: data keys wrapped with it are lost on
		// restart, which only a memory store survives consistently.
		rng := core.SystemRng{}
		for i := 0; i < 32; i += 8 {
			v := rng.Uint64()
			for j := range 8 {
				kek.Key[i+j] = byte(v >> (8 * j))
			}
		}
		env.Logger.Warn("content.ephemeral_kek", "detail", "FF_CONTENT_KEK unset: a per-process development key encrypts findings")
	}
	var repo contentstore.Repo
	var mem *contentstore.MemRepo
	if env.DB != nil {
		repo = contentpg.New(env.DB)
	} else {
		mem = contentstore.NewMemRepo()
		repo = mem
	}
	var backend chunkstore.Backend = chunkstore.NewMemBackend()
	idx := &contentstore.Indexes{Mode: o.IndexMode, IndexLatest: o.IndexLatest}
	if o.Dir != "" {
		d, err := fsstore.New(filepath.Join(o.Dir, "chunks"), fsstore.Options{})
		if err != nil {
			return nil, nil, err
		}
		backend = d
		idx.Dirs = func(tenant, program core.UUID) (textindex.Directory, error) {
			return textindex.NewFSDirectory(filepath.Join(o.Dir, "index", tenant.String(), program.String()))
		}
	}
	chunks := contentstore.NewChunkStores(backend, chunkstore.Config{Clock: env.Clock})
	store, err := contentstore.New(contentstore.Config{Repo: repo, Chunks: chunks,
		Keys: &contentstore.Keyring{Repo: repo, KEK: kek, Rng: core.SystemRng{}, Clock: env.Clock}, Index: idx, Clock: env.Clock,
		DefaultLimits: o.Limits, Codec: o.Codec, Retention: o.Retention})
	if err != nil {
		return nil, nil, err
	}
	srv := contentsvc.NewServer(contentsvc.Options{Store: store, Logger: env.Logger})
	c := &Component{Register: func(g *grpc.Server) { pb.RegisterContentServiceServer(g, srv) }, Ready: repo.Ping,
		Close: func() { _ = chunks.Close(context.Background()); _ = idx.Close() }}
	if env.Bus != nil {
		var src bus.OutboxSource = bus.PgOutbox{DB: env.DB, Table: "content.outbox"}
		if mem != nil {
			src = memContentOutbox{mem}
		}
		c.Run = append(c.Run, relayLoop(env, src))
	}
	sweep := o.SweepInterval
	if sweep == 0 {
		sweep = time.Hour
	}
	if sweep > 0 {
		c.Run = append(c.Run, func(ctx context.Context) error {
			every(ctx, sweep, func() { srv.Sweep(ctx, repo.Tenants, false) })
			return nil
		})
	}
	if o.IndexMode == contentstore.IndexBatched {
		iv := o.IndexInterval
		if iv <= 0 {
			iv = 5 * time.Second
		}
		c.Run = append(c.Run, func(ctx context.Context) error {
			every(ctx, iv, func() {
				ts, err := repo.Tenants(ctx)
				if err != nil {
					env.Logger.WarnContext(ctx, "content.index_failed", "error", err.Error())
					return
				}
				for _, t := range ts {
					srv.IndexTenant(ctx, t)
				}
			})
			return nil
		})
	}
	return c, srv, nil
}

// every runs f every d until ctx ends.
func every(ctx context.Context, d time.Duration, f func()) {
	t := time.NewTicker(d)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f()
		}
	}
}

// memContentOutbox adapts the in-memory content outbox to bus.OutboxSource.
type memContentOutbox struct{ r *contentstore.MemRepo }

func (m memContentOutbox) Claim(ctx context.Context, limit int, publish func(context.Context, []bus.OutboxRow) error) (int, error) {
	return m.r.ClaimOutbox(ctx, limit, func(ctx context.Context, es []contentstore.OutboxEvent) error {
		rows := make([]bus.OutboxRow, len(es))
		for i, e := range es {
			rows[i] = bus.OutboxRow{ID: int64(i + 1), EventID: e.EventID.String(), Topic: e.Topic, OrderingKey: e.OrderingKey, Payload: e.Payload}
		}
		return publish(ctx, rows)
	})
}

// ContentConfigFromEnv reads the content service's variables:
// FF_CONTENT_DIR, FF_CONTENT_KEK, FF_CONTENT_KEK_ID, FF_CONTENT_CODEC
// (delta, chunk), FF_CONTENT_INDEX_MODE (inline, batched),
// FF_CONTENT_INDEX_LATEST, FF_CONTENT_QUOTA_CAPTURES_PER_DAY,
// FF_CONTENT_QUOTA_STORED_BYTES, FF_CONTENT_QUOTA_INDEX_BYTES,
// FF_CONTENT_SWEEP_INTERVAL (Go duration; "-1s" disables).
func ContentConfigFromEnv(getenv func(string) string, prod bool) (ContentOptions, error) {
	o := ContentOptions{Dir: getenv("FF_CONTENT_DIR"), KEK: getenv("FF_CONTENT_KEK"), KEKID: getenv("FF_CONTENT_KEK_ID"), Prod: prod,
		IndexLatest: getenv("FF_CONTENT_INDEX_LATEST") == "true"}
	var err error
	if o.Codec, err = contentstore.ParseBlobCodec(getenv("FF_CONTENT_CODEC")); err != nil {
		return o, err
	}
	if o.IndexMode, err = contentstore.ParseIndexMode(getenv("FF_CONTENT_INDEX_MODE")); err != nil {
		return o, err
	}
	for _, q := range []struct {
		name string
		dst  *int64
	}{{"FF_CONTENT_QUOTA_CAPTURES_PER_DAY", &o.Limits.CapturesPerDay}, {"FF_CONTENT_QUOTA_STORED_BYTES", &o.Limits.StoredBytes},
		{"FF_CONTENT_QUOTA_INDEX_BYTES", &o.Limits.IndexBytes}} {
		if v := getenv(q.name); v != "" {
			if _, err := fmt.Sscan(v, q.dst); err != nil || *q.dst < 0 {
				return o, fmt.Errorf("%s: want a non-negative integer, got %q", q.name, v)
			}
		}
	}
	if v := getenv("FF_CONTENT_SWEEP_INTERVAL"); v != "" {
		if o.SweepInterval, err = time.ParseDuration(v); err != nil {
			return o, fmt.Errorf("FF_CONTENT_SWEEP_INTERVAL: %w", err)
		}
	}
	return o, nil
}
