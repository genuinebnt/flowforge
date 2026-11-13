package chunkstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// readManifest returns a stored blob's decoded manifest.
func readManifest(t testing.TB, s *Store, id ManifestID) *Manifest {
	t.Helper()
	raw, err := s.readChunk(context.Background(), ChunkID(id), CodeManifestNotFound)
	if err != nil {
		t.Fatal(err)
	}
	m, err := DecodeManifest(raw)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// CHUNK-INV-24: one namespace written by every chunker in turn (each
// reopen picks another) serves every blob; each manifest records the
// chunker that cut it, and re-cutting the blob with the recorded chunker
// reproduces its chunk list exactly; identical content dedupes under the
// same chunker (same manifest id) and is stored separately under another
// (different manifest id, both readable); fsck is clean and checks the
// recorded bounds; GC keeps every named blob of every chunker.
func TestChunkInv24_MixedChunkers(t *testing.T) {
	r := testkit.Rng(t)
	ctx := context.Background()
	b := NewMemBackend()
	shared := textish(r, 150_000)
	type blob struct {
		id   ManifestID
		data []byte
		cfg  ChunkerConfig
	}
	var blobs []blob
	for range 2 {
		for _, cc := range chunkerCandidates() {
			s := openStore(t, b, Config{Chunking: cc})
			for i := range 3 {
				var x []byte
				switch i {
				case 0:
					x = shared
				case 1:
					x = mutate(r, shared, 5)
				default:
					x = randBytes(r, core.IntN(r, 200_000))
				}
				id, st := mustPut(t, s, x)
				if i == 0 {
					// Same content, same chunker: one manifest.
					for _, o := range blobs {
						if o.cfg == s.Chunking() && bytes.Equal(o.data, x) && (o.id != id || st.NewManifest) {
							t.Fatalf("%s: same content and chunker gave a new manifest", cfgName(o.cfg))
						}
						if o.cfg != s.Chunking() && bytes.Equal(o.data, x) && o.id == id {
							t.Fatalf("%s and %s share a manifest id", cfgName(o.cfg), cfgName(s.Chunking()))
						}
					}
				}
				blobs = append(blobs, blob{id, x, s.Chunking()})
			}
			if err := s.Close(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	s := openStore(t, b, Config{})
	var ids []ManifestID
	for _, bl := range blobs {
		ids = append(ids, bl.id)
		if got := mustGet(t, s, bl.id); !bytes.Equal(got, bl.data) {
			t.Fatalf("%s: blob differs after reopening with FastCDC", cfgName(bl.cfg))
		}
		info, err := s.Stat(ctx, bl.id)
		if err != nil || info.Chunking != bl.cfg {
			t.Fatalf("Stat records %+v, want %+v (%v)", info.Chunking, bl.cfg, err)
		}
		m := readManifest(t, s, bl.id)
		cut, err := NewCutter(m.Chunking)
		if err != nil {
			t.Fatal(err)
		}
		var recut []ChunkID
		prev := 0
		for _, c := range Boundaries(cut, bl.data) {
			recut = append(recut, SumChunk(bl.data[prev:c]))
			prev = c
		}
		if !slices.Equal(recut, m.Chunks) {
			t.Fatalf("%s: re-cutting with the recorded chunker does not reproduce the manifest", cfgName(bl.cfg))
		}
	}
	fr, err := s.Fsck(ctx, ids)
	if err != nil || len(fr.Problems) != 0 {
		t.Fatalf("fsck: %v %+v", err, fr.Problems)
	}
	if _, err := s.GC(ctx, ids, nil, GCOptions{LiveThresholdPercent: 100}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(ctx, ids, nil, GCOptions{LiveThresholdPercent: 100}); err != nil {
		t.Fatal(err)
	}
	for _, bl := range blobs {
		if got := mustGet(t, s, bl.id); !bytes.Equal(got, bl.data) {
			t.Fatalf("%s: blob lost by GC", cfgName(bl.cfg))
		}
	}

	// fsck rejects chunk lengths the recorded chunker cannot produce: a
	// FastCDC manifest naming one chunk below MinSize followed by another.
	ps := &putState{ids: map[ChunkID]struct{}{}}
	small, rest := randBytes(r, 100), randBytes(r, 5000)
	for _, c := range [][]byte{small, rest} {
		if _, _, err := s.add(ctx, ps, SumChunk(c), c); err != nil {
			t.Fatal(err)
		}
	}
	bad := Manifest{TotalLen: 5100, Chunks: []ChunkID{SumChunk(small), SumChunk(rest)}, Chunking: DefaultChunkerConfig()}
	if _, _, err := s.add(ctx, ps, ChunkID(bad.ID()), bad.Encode()); err != nil {
		t.Fatal(err)
	}
	fr, err = s.Fsck(ctx, append(ids, bad.ID()))
	if err != nil || !fr.Has(CodeManifestChunking) || fr.OK() {
		t.Fatalf("fsck of an impossible chunking: %v %+v", err, fr.Problems)
	}
}

// CHUNK-INV-25: the manifest's chunker descriptor. Every chunker's
// canonical configuration encodes as version 2 and decodes to itself;
// an unset Chunking encodes as version 1 (the bytes of manifests written
// before version 2) and both versions are read; an unknown algorithm is
// MANIFEST_UNSUPPORTED; non-zero reserved bytes, a non-canonical or
// invalid configuration, or a version-2 header cut short are
// MANIFEST_MALFORMED/MANIFEST_TRUNCATED; the descriptor changes the id.
func TestChunkInv25_ManifestChunkerDescriptor(t *testing.T) {
	ids := []ChunkID{SumChunk([]byte("a")), SumChunk([]byte("b"))}
	seen := map[ManifestID]bool{}
	for _, cc := range append(chunkerCandidates(), ChunkerConfig{}) {
		var canon ChunkerConfig
		if cc != (ChunkerConfig{}) {
			canon = mustCutter(t, cc).Config()
		}
		m := Manifest{TotalLen: 2, Chunks: ids, Chunking: canon}
		enc := m.Encode()
		wantV := uint16(ManifestVersion)
		if canon.Algo == 0 {
			wantV = ManifestVersionLegacy
		}
		if v := binary.LittleEndian.Uint16(enc[4:]); v != wantV {
			t.Fatalf("%s: version %d, want %d", cfgName(canon), v, wantV)
		}
		d, err := DecodeManifest(enc)
		if err != nil || d.Chunking != canon || !slices.Equal(d.Chunks, ids) || !bytes.Equal(d.Encode(), enc) {
			t.Fatalf("%s: decode %+v %v", cfgName(canon), d, err)
		}
		if seen[m.ID()] {
			t.Fatalf("%s: manifest id shared with another chunker", cfgName(canon))
		}
		seen[m.ID()] = true
	}
	good := (&Manifest{TotalLen: 2, Chunks: ids, Chunking: DefaultChunkerConfig()}).Encode()
	resum := func(b []byte) []byte {
		b = slices.Clone(b)
		binary.LittleEndian.PutUint32(b[len(b)-4:], CRC32C(b[:len(b)-4]))
		return b
	}
	edit := func(f func(b []byte)) []byte {
		b := slices.Clone(good)
		f(b)
		return resum(b)
	}
	cases := map[string]struct {
		data []byte
		code Code
	}{
		"algo 0":           {edit(func(b []byte) { b[20] = 0 }), CodeManifestUnsupported},
		"algo 5":           {edit(func(b []byte) { b[20] = 5 }), CodeManifestUnsupported},
		"reserved":         {edit(func(b []byte) { b[22] = 1 }), CodeManifestMalformed},
		"avg not pow2":     {edit(func(b []byte) { binary.LittleEndian.PutUint32(b[28:], 8000) }), CodeManifestMalformed},
		"zero sizes":       {edit(func(b []byte) { clear(b[24:36]) }), CodeManifestMalformed},
		"fixed min != avg": {edit(func(b []byte) { b[20] = byte(ChunkFixed) }), CodeManifestMalformed},
		"whole with min":   {edit(func(b []byte) { b[20] = byte(ChunkWhole) }), CodeManifestMalformed},
		"v2 header short":  {resum(append(append([]byte{}, good[:20]...), 0, 0, 0, 0)), CodeManifestTruncated},
		"version 3":        {edit(func(b []byte) { b[4] = 3 }), CodeManifestUnsupported},
	}
	for name, c := range cases {
		if _, err := DecodeManifest(c.data); CodeOf(err) != c.code {
			t.Errorf("%s: got %v, want %s", name, err, c.code)
		}
	}
}

// The Rabin polynomial is irreducible over GF(2) (Ben-Or 1981: a
// polynomial p of degree n is irreducible iff gcd(x^(2^i) - x mod p, p)
// = 1 for i = 1..n/2), and the rolling update equals fingerprinting the
// window from scratch at every position.
func TestChunkRabinPolynomialIrreducible(t *testing.T) {
	p := RabinPolynomial
	if polyDeg(p) != rabinDegree {
		t.Fatalf("degree %d", polyDeg(p))
	}
	mulmod := func(a, b uint64) uint64 {
		var r uint64
		for ; b != 0; b >>= 1 {
			if b&1 != 0 {
				r ^= a
			}
			a <<= 1
			if a>>rabinDegree&1 != 0 {
				a ^= p
			}
		}
		return r
	}
	gcd := func(a, b uint64) uint64 {
		for b != 0 {
			a, b = b, polyMod(a, b)
		}
		return a
	}
	h := uint64(2) // x
	for i := 1; i <= rabinDegree/2; i++ {
		h = mulmod(h, h)
		if g := gcd(h^2, p); g != 1 {
			t.Fatalf("gcd(x^(2^%d) - x, p) = %#x: reducible", i, g)
		}
	}
	// Rolling equals direct: fingerprint data[i-48:i] by appending bytes.
	data := splitmixBytes(9, 4096)
	var win [RabinWindow]byte
	var fp uint64
	w := 0
	for i, b := range data {
		fp ^= rabinOut[win[w]]
		win[w] = b
		w = (w + 1) % RabinWindow
		fp = (fp<<8 | uint64(b)) ^ rabinMod[fp>>rabinShift]
		if i+1 < RabinWindow {
			continue
		}
		var direct uint64
		for _, c := range data[i+1-RabinWindow : i+1] {
			direct = polyMod(direct<<8|uint64(c), p)
		}
		if direct != fp {
			t.Fatalf("position %d: rolling %#x, direct %#x", i, fp, direct)
		}
	}
}

// Invalid configurations of every algorithm are INVALID_CONFIG; the zero
// value of each algorithm takes its defaults; names round-trip.
func TestChunkerAlgoConfigValidation(t *testing.T) {
	bad := []ChunkerConfig{
		{Algo: ChunkRabin, MinSize: 2048, AvgSize: 8000, MaxSize: 65536},
		{Algo: ChunkRabin, MinSize: 32, AvgSize: 256, MaxSize: 1024},
		{Algo: ChunkFixed, AvgSize: 10},
		{Algo: ChunkFixed, MinSize: 1, AvgSize: 4096},
		{Algo: ChunkFixed, AvgSize: 4096, MaxSize: 8192},
		{Algo: ChunkWhole, MaxSize: 32 << 20},
		{Algo: ChunkWhole, MinSize: 64, MaxSize: 4096},
		{Algo: 9, MaxSize: 4096},
	}
	for _, c := range bad {
		if _, err := NewCutter(c); CodeOf(err) != CodeInvalidConfig {
			t.Errorf("%+v: got %v", c, err)
		}
	}
	if _, err := NewFastCDC(ChunkerConfig{Algo: ChunkRabin}); CodeOf(err) != CodeInvalidConfig {
		t.Errorf("NewFastCDC accepted Rabin: %v", err)
	}
	want := map[ChunkAlgo]ChunkerConfig{
		ChunkFastCDC: DefaultChunkerConfig(),
		ChunkRabin:   {Algo: ChunkRabin, MinSize: DefaultMinSize, AvgSize: DefaultAvgSize, MaxSize: DefaultMaxSize},
		ChunkFixed:   {Algo: ChunkFixed, MinSize: DefaultFixedSize, AvgSize: DefaultFixedSize, MaxSize: DefaultFixedSize},
		ChunkWhole:   {Algo: ChunkWhole, MaxSize: DefaultWholeMaxSize},
	}
	for _, a := range ChunkAlgos() {
		if got := mustCutter(t, ChunkerConfig{Algo: a}).Config(); got != want[a] {
			t.Errorf("%s defaults %+v", a, got)
		}
		if p, err := ParseChunkAlgo(a.String()); err != nil || p != a {
			t.Errorf("ParseChunkAlgo(%q) = %v %v", a.String(), p, err)
		}
	}
	if _, err := ParseChunkAlgo("gear"); CodeOf(err) != CodeInvalidConfig {
		t.Errorf("ParseChunkAlgo accepted gear: %v", err)
	}
}
