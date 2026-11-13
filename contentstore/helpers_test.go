package contentstore

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/chunkstore"
	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

var (
	t0       = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	tenantA  = core.MustParseUUID("00000000-0000-7000-8000-0000000000aa")
	tenantB  = core.MustParseUUID("00000000-0000-7000-8000-0000000000bb")
	programP = core.MustParseUUID("00000000-0000-7000-8000-0000000000c1")
)

// harness is a store over memory adapters with a fake clock.
type harness struct {
	t       testing.TB
	repo    Repo
	mem     *MemRepo
	backend *chunkstore.MemBackend
	chunks  *ChunkStores
	clock   *testkit.FakeClock
	store   *Store
	index   *Indexes
}

type harnessOpt func(*Config)

func newHarness(t testing.TB, opts ...harnessOpt) *harness {
	t.Helper()
	return newHarnessRepo(t, NewMemRepo(), opts...)
}

func newHarnessRepo(t testing.TB, repo Repo, opts ...harnessOpt) *harness {
	t.Helper()
	h := &harness{t: t, repo: repo, backend: chunkstore.NewMemBackend(), clock: testkit.NewFakeClock(t0.Add(time.Hour))}
	h.mem, _ = repo.(*MemRepo)
	h.chunks = NewChunkStores(h.backend, chunkstore.Config{})
	h.index = &Indexes{}
	kek := &LocalKEK{KeyID: "test", Rng: core.NewSplitMix64(7)}
	for i := range kek.Key {
		kek.Key[i] = byte(i * 7)
	}
	cfg := Config{Repo: repo, Chunks: h.chunks, Keys: &Keyring{Repo: repo, KEK: kek, Rng: core.NewSplitMix64(11), Clock: h.clock},
		Index: h.index, Clock: h.clock}
	for _, o := range opts {
		o(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h.store = s
	t.Cleanup(func() { _ = h.chunks.Close(context.Background()); _ = h.index.Close() })
	return h
}

func (h *harness) ingest(tenant core.UUID, c content.Capture, pageURL string) IngestResult {
	h.t.Helper()
	r, err := h.store.Ingest(context.Background(), tenant, IngestRequest{Program: programP, Capture: c, PageURL: pageURL})
	if err != nil {
		h.t.Fatalf("ingest %s @%s: %v", c.URL, c.FetchedAt.Format(time.RFC3339), err)
	}
	return r
}

func capture(u string, body []byte, hs []content.Header, at time.Time) content.Capture {
	return content.Capture{URL: u, Profile: content.DefaultProfile(), Status: 200, Headers: hs, Body: body, FetchedAt: at, Outcome: content.OutcomeComplete}
}

// spaFile reads conformance/content/sites/spa/<name>.
func spaFile(t testing.TB, name string) []byte {
	t.Helper()
	return testkit.ReadFile(t, "content/sites/spa", name)
}

// spaHeaders parses a "Name: value" fixture.
func spaHeaders(t testing.TB, name string) []content.Header {
	t.Helper()
	var hs []content.Header
	for _, l := range strings.Split(strings.TrimSpace(string(spaFile(t, name))), "\n") {
		k, v, _ := strings.Cut(l, ":")
		hs = append(hs, content.Header{Name: strings.TrimSpace(k), Value: strings.TrimSpace(v)})
	}
	return hs
}

const spaPage = "https://shop.example/"

var jsHeaders = []content.Header{{Name: "Content-Type", Value: "application/javascript"}}

// spaVersions are the SPA fixture's captures in deploy order: page v1,
// the first bundle and vite chunk, page v2, the reordered bundle, page
// v3, the edited bundle, the renamed vite chunk and the unrelated chunk.
func spaVersions(t testing.TB) []struct {
	C    content.Capture
	Page string
} {
	day := func(d int) time.Time { return t0.AddDate(0, 0, d) }
	page := func(v string, d int) content.Capture {
		return capture(spaPage, spaFile(t, v+".index.html"), spaHeaders(t, v+".headers.txt"), day(d))
	}
	js := func(name string, d int) content.Capture {
		return capture("https://shop.example/static/js/"+name, spaFile(t, name), jsHeaders, day(d).Add(time.Minute))
	}
	return []struct {
		C    content.Capture
		Page string
	}{
		{page("v1", 0), ""},
		{js("main.3f9a1c2b.js", 0), spaPage},
		{js("index-a1b2c3d4.js", 0), spaPage},
		{page("v2", 7), ""},
		{js("main.77e0d41a.js", 7), spaPage},
		{page("v3", 14), ""},
		{js("main.5b6c7d8e.js", 14), spaPage},
		{js("app-e5f6a7b8.js", 14), spaPage},
		{js("other-0f0f0f0f.js", 14), spaPage},
	}
}
