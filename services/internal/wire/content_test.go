package wire_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/chunkstore"
	"github.com/genuinebnt/flowforge/chunkstore/gcsstore"
	"github.com/genuinebnt/flowforge/chunkstore/gcsstore/gcsfake"
	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/contentstore"
	"github.com/genuinebnt/flowforge/core"
	pb "github.com/genuinebnt/flowforge/gen/flowforge/v1"
	"github.com/genuinebnt/flowforge/services/internal/platform"
	"github.com/genuinebnt/flowforge/services/internal/wire"
	"github.com/genuinebnt/flowforge/testkit"
	"github.com/genuinebnt/flowforge/textindex"
)

var (
	cTenant  = core.MustParseUUID("0190a1b2-0000-7000-8000-00000000c0a1")
	cProgram = core.MustParseUUID("0190a1b2-0000-7000-8000-00000000c0a2")
	cT0      = time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
)

func spa(t *testing.T, name string) []byte { return testkit.ReadFile(t, "content/sites/spa", name) }

// The storage backend is chosen by configuration: FF_CONTENT_BUCKET, then
// FF_CONTENT_DIR, then memory, which prod refuses.
func TestContentStorageSelection(t *testing.T) {
	env := map[string]string{"FF_CONTENT_BUCKET": "b", "FF_CONTENT_DIR": "/d", "FF_CONTENT_KEK": strings.Repeat("ab", 32)}
	o, err := wire.ContentConfigFromEnv(func(k string) string { return env[k] }, true)
	if err != nil || o.Bucket != "b" || wire.ContentStorageKind(o) != "gcs" {
		t.Fatalf("bucket: %+v %v", o, err)
	}
	for _, c := range []struct {
		o    wire.ContentOptions
		want string
	}{{wire.ContentOptions{Dir: "/d"}, "dir"}, {wire.ContentOptions{}, "mem"}} {
		if got := wire.ContentStorageKind(c.o); got != c.want {
			t.Errorf("%+v: %s, want %s", c.o, got, c.want)
		}
	}
	e := &wire.Env{Service: "content", Logger: platform.DiscardLogger(), Clock: &core.SystemClock{}}
	if _, _, err := wire.Content(e, wire.ContentOptions{Prod: true, KEK: env["FF_CONTENT_KEK"], SweepInterval: -1}); err == nil {
		t.Fatal("prod content service started on memory storage")
	}
}

// TestRegress_G_CST_02_ContentServiceOnGCS: with FF_CONTENT_BUCKET the
// content service keeps its chunk packs under chunks/t/<tenant>/ and its
// text index under index/<tenant>/<program>/ in the bucket, not on the
// instance.
func TestRegress_G_CST_02_ContentServiceOnGCS(t *testing.T) {
	f := gcsfake.New(t)
	e := &wire.Env{Service: "content", Logger: platform.DiscardLogger(), Clock: &core.SystemClock{}}
	comp, srv, err := wire.Content(e, wire.ContentOptions{Bucket: "ff-content", ClientOptions: f.ClientOptions(), SweepInterval: -1})
	if err != nil {
		t.Fatal(err)
	}
	ctx := platform.WithIdentity(context.Background(), platform.Identity{Tenant: cTenant, Actor: platform.ServiceActor("tool-runner"), Role: auth.RoleAnalyst})
	for i, v := range []string{"v1", "v2"} {
		_, err := srv.IngestCapture(ctx, &pb.IngestCaptureRequest{ProgramId: core.PublicID(core.PrefixProgram, cProgram),
			Capture: &pb.Capture{Url: "https://shop.example/", Status: 200, Body: spa(t, v+".index.html"), Outcome: "complete",
				Headers:   []*pb.HttpHeader{{Name: "Content-Type", Value: "text/html"}},
				FetchedAt: timestamppb.New(cT0.AddDate(0, 0, 7*i))}})
		if err != nil {
			t.Fatalf("ingest %s: %v", v, err)
		}
	}
	comp.Close()
	var ref, index bool
	for _, n := range f.Names("ff-content") {
		ref = ref || n == "chunks/t/"+cTenant.String()+"/ref"
		index = index || strings.HasPrefix(n, "index/"+cTenant.String()+"/"+cProgram.String()+"/")
	}
	if !ref || !index {
		t.Fatalf("bucket holds %v", f.Names("ff-content"))
	}
}

// TestRegress_G_CST_02_RestartKeepsContent: a content store on GCS is
// restarted (a new Cloud Run instance: new backend and index directories
// on the same bucket, same database). Stored bodies read back and search
// still answers from the persisted index, without a rebuild.
func TestRegress_G_CST_02_RestartKeepsContent(t *testing.T) {
	ctx := context.Background()
	f := gcsfake.New(t)
	repo := contentstore.NewMemRepo()
	clock := testkit.NewFakeClock(cT0.AddDate(0, 0, 30))
	kek := &contentstore.LocalKEK{KeyID: "k1", Rng: core.NewSplitMix64(3)}
	for i := range kek.Key {
		kek.Key[i] = byte(i)
	}
	open := func() (*contentstore.Store, func()) {
		cl := f.Client(t)
		b, err := gcsstore.New(cl, "ff-content", gcsstore.Options{Prefix: "chunks/"})
		if err != nil {
			t.Fatal(err)
		}
		idx := &contentstore.Indexes{Dirs: func(tenant, program core.UUID) (textindex.Directory, error) {
			return gcsstore.NewDirectory(cl, "ff-content", "index/"+tenant.String()+"/"+program.String()+"/")
		}}
		chunks := contentstore.NewChunkStores(b, chunkstore.Config{Clock: clock})
		s, err := contentstore.New(contentstore.Config{Repo: repo, Chunks: chunks,
			Keys:  &contentstore.Keyring{Repo: repo, KEK: kek, Rng: core.NewSplitMix64(5), Clock: clock},
			Index: idx, Clock: clock})
		if err != nil {
			t.Fatal(err)
		}
		return s, func() { _ = chunks.Close(ctx); _ = idx.Close() }
	}
	s1, close1 := open()
	var last contentstore.IngestResult
	for i, v := range []string{"v1", "v2", "v3"} {
		c := content.Capture{URL: "https://shop.example/", Profile: content.DefaultProfile(), Status: 200,
			Headers: []content.Header{{Name: "Content-Type", Value: "text/html"}}, Body: spa(t, v+".index.html"),
			FetchedAt: cT0.AddDate(0, 0, 7*i), Outcome: content.OutcomeComplete}
		res, err := s1.Ingest(ctx, cTenant, contentstore.IngestRequest{Program: cProgram, Capture: c})
		if err != nil {
			t.Fatalf("ingest %s: %v", v, err)
		}
		last = res
	}
	before, err := s1.Search(ctx, cTenant, cProgram, "kind:form_added", contentstore.SearchOptions{})
	if err != nil || before.Total == 0 {
		t.Fatalf("search before the restart: %+v %v", before, err)
	}
	close1()

	s2, close2 := open()
	defer close2()
	raw, err := s2.SnapshotContent(ctx, cTenant, last.Snapshot.ID, contentstore.ContentOptions{Raw: true})
	if err != nil || !bytes.Equal(raw.Body, spa(t, "v3.index.html")) {
		t.Fatalf("raw read after the restart: %v", err)
	}
	after, err := s2.Search(ctx, cTenant, cProgram, "kind:form_added", contentstore.SearchOptions{})
	if err != nil || after.Total != before.Total {
		t.Fatalf("search after the restart: %d hits (%v), want %d", after.Total, err, before.Total)
	}
}
