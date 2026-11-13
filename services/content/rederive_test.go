package content_test

import (
	"context"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/chunkstore"
	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/contentstore"
	"github.com/genuinebnt/flowforge/contentstore/storetest"
	"github.com/genuinebnt/flowforge/core"
	contentsvc "github.com/genuinebnt/flowforge/services/content"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestContentService_RederiveSweep: after a pipeline upgrade the
// maintenance sweep re-derives a tenant's snapshots in bounded batches
// (V2 §12.2), one batch per pass until none remain, without appending an
// outbox event (no graph observation, no alert).
func TestContentService_RederiveSweep(t *testing.T) {
	ctx := context.Background()
	tenant := core.MustParseUUID("00000000-0000-7000-8000-00000000a001")
	program := core.MustParseUUID("00000000-0000-7000-8000-00000000c003")
	clock := testkit.NewFakeClock(t0.AddDate(0, 0, 30))
	repo := contentstore.NewMemRepo()
	kek := &contentstore.LocalKEK{KeyID: "svc", Rng: core.NewSplitMix64(3)}
	cfg := contentstore.Config{Repo: repo, Chunks: contentstore.NewChunkStores(chunkstore.NewMemBackend(), chunkstore.Config{}),
		Keys: &contentstore.Keyring{Repo: repo, KEK: kek, Rng: core.NewSplitMix64(5), Clock: clock}, Index: &contentstore.Indexes{}, Clock: clock}
	old, err := contentstore.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var ids []core.UUID
	for i, v := range versions {
		hs := []content.Header{{Name: "Content-Type", Value: "application/javascript"}}
		if v.headers != "" {
			hs = nil
			for _, h := range headers(t, v.headers) {
				hs = append(hs, content.Header{Name: h.Name, Value: h.Value})
			}
		}
		r, err := old.Ingest(ctx, tenant, contentstore.IngestRequest{Program: program, PageURL: v.page, Capture: content.Capture{
			URL: v.url, Profile: content.DefaultProfile(), Status: 200, Headers: hs, Body: spa(t, v.file),
			FetchedAt: t0.AddDate(0, 0, v.day).Add(time.Duration(i) * time.Minute), Outcome: content.OutcomeComplete}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.Snapshot.ID)
	}
	events := len(repo.OutboxEvents())
	cfg.Pipeline = storetest.UpgradedPipeline()
	up, err := contentstore.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := contentsvc.NewServer(contentsvc.Options{Store: up, RederiveBatch: 2})
	stale := func() int {
		n := 0
		for _, id := range ids {
			sn, err := up.GetSnapshot(ctx, tenant, id)
			if err != nil {
				t.Fatal(err)
			}
			if sn.DeriveStamp != storetest.UpgradedStamp {
				n++
			}
		}
		return n
	}
	for pass := 1; ; pass++ {
		srv.Sweep(ctx, repo.Tenants, true)
		left := stale()
		if want := max(len(ids)-2*pass, 0); left != want {
			t.Fatalf("after pass %d: %d stale snapshots, want %d", pass, left, want)
		}
		if left == 0 {
			break
		}
	}
	if n := len(repo.OutboxEvents()); n != events {
		t.Fatalf("re-derive appended %d outbox events", n-events)
	}
}
