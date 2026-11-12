package csr

import (
	"errors"
	"math"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/core"
)

func TestCSRBuildRejects(t *testing.T) {
	ids := nodeUUIDs(2)
	cases := map[string]func() error{
		"endpoint": func() error { _, err := Build(Meta{}, ids, nil, []Edge{{0, 2, 0, 1}}, []string{"l"}); return err },
		"label":    func() error { _, err := Build(Meta{}, ids, nil, []Edge{{0, 1, 1, 1}}, []string{"l"}); return err },
		"no dict":  func() error { _, err := Build(Meta{}, ids, nil, []Edge{{0, 1, 0, 1}}, nil); return err },
		"NaN": func() error {
			_, err := Build(Meta{}, ids, nil, []Edge{{0, 1, 0, float32(math.NaN())}}, []string{"l"})
			return err
		},
		"conf>1":  func() error { _, err := Build(Meta{}, ids, nil, []Edge{{0, 1, 0, 1.5}}, []string{"l"}); return err },
		"dup id":  func() error { _, err := Build(Meta{}, []core.UUID{ids[0], ids[0]}, nil, nil, nil); return err },
		"types":   func() error { _, err := Build(Meta{}, ids, []uint8{1}, nil, nil); return err },
		"flags":   func() error { _, err := Build(Meta{Flags: 1 << 8}, ids, nil, nil, nil); return err },
		"dup lbl": func() error { _, err := Build(Meta{}, ids, nil, nil, []string{"a", "a"}); return err },
	}
	for name, fn := range cases {
		if err := fn(); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCSRBuilder(t *testing.T) {
	ids := nodeUUIDs(3)
	b := NewBuilder("owns")
	// Added in reverse order; Build renumbers by entity id.
	for i := 2; i >= 0; i-- {
		b.AddNode(ids[i], uint8(10+i))
	}
	b.AddNode(ids[0], 99) // duplicate keeps the first type
	if err := b.AddEdge(ids[2], ids[0], "port", 0.5); err != nil {
		t.Fatal(err)
	}
	if err := b.AddEdge(ids[0], ids[1], "owns", 1); err != nil {
		t.Fatal(err)
	}
	if err := b.AddEdge(ids[0], nodeUUID(7), "owns", 1); !errors.Is(err, ErrUnknownNode) {
		t.Fatal(err)
	}
	g, err := b.Build(Meta{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(g.NodeTypes(), []uint8{10, 11, 12}) || !slices.Equal(g.Labels(), []string{"owns", "port"}) {
		t.Fatalf("types %v labels %v", g.NodeTypes(), g.Labels())
	}
	if got := g.Edges(); !slices.Equal(got, []Edge{{0, 1, 0, 1}, {2, 0, 1, 0.5}}) {
		t.Fatalf("edges %v", got)
	}
}

func TestCSRBlastRadius(t *testing.T) {
	// org -owns-> fqdn -dns_record-> ip -port-> svc -certificate-> cert;
	// svc -product_used-> product is not followed.
	labels := []string{"owns", "dns_record", "port", "certificate", "product_used"}
	edges := []Edge{{0, 1, 0, 1}, {1, 2, 1, 1}, {2, 3, 2, 1}, {3, 4, 3, 1}, {3, 5, 4, 1}}
	g, err := Build(Meta{}, nodeUUIDs(6), nil, edges, labels)
	if err != nil {
		t.Fatal(err)
	}
	got, err := BlastRadius(g, 1, 6)
	if err != nil {
		t.Fatal(err)
	}
	want := []Reach{{2, 1}, {3, 2}, {4, 3}}
	if !slices.Equal(got, want) {
		t.Fatalf("blast radius %v, want %v", got, want)
	}
	if got, _ := BlastRadius(g, 0, 2); !slices.Equal(got, []Reach{{1, 1}, {2, 2}}) {
		t.Fatalf("depth 2: %v", got)
	}
	if _, err := BlastRadius(g, 0, 7); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("depth 7 accepted: %v", err)
	}
}

func TestCSRCommonNeighbors(t *testing.T) {
	// 0 and 1 share hosts 2 and 3 (in either direction); 4 only touches 0.
	g, err := Build(Meta{}, nodeUUIDs(5), nil, []Edge{{0, 2, 0, 1}, {1, 2, 0, 1}, {3, 0, 0, 1}, {1, 3, 0, 1}, {0, 4, 0, 1}, {0, 1, 0, 1}}, []string{"l"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := CommonNeighbors(g, 0, 1)
	if err != nil || !slices.Equal(got, []uint32{2, 3}) {
		t.Fatalf("common %v %v", got, err)
	}
	if TriangleCount(g) != 2 {
		t.Fatalf("triangles %d", TriangleCount(g))
	}
	if k := KCore(g, 2); !slices.Equal(k, []uint32{0, 1, 2, 3}) {
		t.Fatalf("2-core %v", k)
	}
}

func TestCSRHolder(t *testing.T) {
	var h Holder
	if h.Load() != nil {
		t.Fatal("zero Holder not empty")
	}
	mk := func(seq uint64, tenant byte) *Graph {
		m := Meta{WatermarkSeq: seq}
		m.TenantID[0] = tenant
		g, err := Build(m, nil, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	if _, err := h.Publish(nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	first := mk(1, 1)
	if old, err := h.Publish(first); err != nil || old != nil {
		t.Fatal(old, err)
	}
	if _, err := h.Publish(mk(0, 1)); !errors.Is(err, ErrStaleSnapshot) {
		t.Fatal(err)
	}
	if _, err := h.Publish(mk(5, 2)); !errors.Is(err, ErrInvalidInput) {
		t.Fatal(err)
	}
	// Concurrent readers always see a complete snapshot with a
	// non-decreasing watermark while a writer publishes.
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			last := uint64(0)
			for range 2000 {
				g := h.Load()
				if w := g.Meta().WatermarkSeq; w < last {
					t.Errorf("watermark went back %d -> %d", last, w)
					return
				} else {
					last = w
				}
			}
		})
	}
	for i := uint64(2); i < 200; i++ {
		if _, err := h.Publish(mk(i, 1)); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if h.Load().Meta().WatermarkSeq != 199 {
		t.Fatal("last publish lost")
	}
}

func TestCSRCompactionPolicy(t *testing.T) {
	var p CompactionPolicy
	t0 := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		events, edges int
		age           time.Duration
		want          bool
	}{
		{0, 0, time.Hour, false},            // empty overlay never triggers
		{10, 1000, time.Minute, false},      // exactly 1%: not above
		{11, 1000, time.Minute, true},       // above 1%
		{1, 1000, 5 * time.Minute, true},    // aged
		{1, 1000, 5*time.Minute - 1, false}, // not yet
		{1, 0, 0, true},                     // any event over an empty snapshot
	}
	for _, c := range cases {
		if got := p.ShouldCompact(c.events, c.edges, t0, t0.Add(c.age)); got != c.want {
			t.Errorf("%+v: got %v", c, got)
		}
	}
	custom := CompactionPolicy{MaxOverlayFraction: 0.5, MaxOverlayAge: time.Second}
	if custom.ShouldCompact(5, 100, t0, t0) || !custom.ShouldCompact(5, 100, t0, t0.Add(time.Second)) {
		t.Error("custom policy")
	}
}

func TestCSRAccessors(t *testing.T) {
	c := specCase(t)
	g := c.graph(t)
	if _, err := g.NodeID(5); !errors.Is(err, ErrNodeRange) {
		t.Error(err)
	}
	if _, err := g.OutNeighbors(9); !errors.Is(err, ErrNodeRange) {
		t.Error(err)
	}
	if _, err := g.EdgeAt(6); !errors.Is(err, ErrInvalidInput) {
		t.Error(err)
	}
	if _, ok := g.Index(nodeUUID(77)); ok {
		t.Error("unknown id found")
	}
	if l, ok := g.LabelIndex("port"); !ok || l != 1 {
		t.Error("label index")
	}
	if s := g.LabelSetOf("nope"); !s.Restricted() || s.Has(0) || s.Has(1) {
		t.Error("unknown-only label set must match nothing")
	}
	if !AnyLabel().Has(200) {
		t.Error("AnyLabel")
	}
	for _, d := range []Direction{DirOut, DirIn, DirBoth} {
		if p, err := ParseDirection(d.String()); err != nil || p != d {
			t.Error(d, err)
		}
	}
	if _, err := ParseDirection("sideways"); err == nil {
		t.Error("bad direction parsed")
	}
}
