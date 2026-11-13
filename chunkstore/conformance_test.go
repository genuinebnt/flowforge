package chunkstore

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

// go test ./chunkstore -run Conformance -update rewrites
// conformance/chunkstore/*.json from this implementation.
var update = flag.Bool("update", false, "rewrite conformance/chunkstore data")

const suite = "chunkstore"

func hexU64(v uint64) string { return fmt.Sprintf("0x%016x", v) }
func hexU32(v uint32) string { return fmt.Sprintf("0x%08x", v) }

func writeOrCompare(t *testing.T, name string, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	path := filepath.Join(testkit.ConformanceDir(t, suite), name)
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		return b
	}
	have := testkit.ReadFile(t, suite, name)
	if !bytes.Equal(have, b) {
		t.Fatalf("%s: this implementation's output differs from the conformance file (run with -update only for a deliberate format change)", name)
	}
	return have
}

type gearFile struct {
	Description string   `json:"description"`
	Generator   string   `json:"generator"`
	Seed        string   `json:"seed"`
	Values      []string `json:"values"`
}

type maskInfo struct {
	MinSize  int    `json:"min_size"`
	AvgSize  int    `json:"avg_size"`
	MaxSize  int    `json:"max_size"`
	MaskS    string `json:"mask_s"`
	MaskL    string `json:"mask_l"`
	MaskSBit int    `json:"mask_s_bits"`
	MaskLBit int    `json:"mask_l_bits"`
}

type boundaryCase struct {
	Name      string   `json:"name"`
	Config    maskInfo `json:"config"`
	Generator string   `json:"generator"`
	Seed      uint64   `json:"seed"`
	Length    int      `json:"length"`
	Cuts      []int    `json:"cuts"`
	SHA256    []string `json:"chunk_sha256"`
}

type boundaryFile struct {
	Description string            `json:"description"`
	Generators  map[string]string `json:"generators"`
	BitOrder    []uint8           `json:"mask_bit_order"`
	Cases       []boundaryCase    `json:"cases"`
}

func infoOf(f *FastCDC) maskInfo {
	c := f.Config()
	return maskInfo{c.MinSize, c.AvgSize, c.MaxSize, hexU64(f.MaskS()), hexU64(f.MaskL()), popcount(f.MaskS()), popcount(f.MaskL())}
}

func boundaryCases(t *testing.T) []boundaryCase {
	type spec struct {
		cfg  ChunkerConfig
		gen  string
		seed uint64
		n    int
	}
	var specs []spec
	for i, n := range []int{0, 1, 2047, 2048, 2049, 8191, 8192, 8193, 65535, 65536, 65537, 131079, 1 << 20} {
		specs = append(specs, spec{DefaultChunkerConfig(), "splitmix64", uint64(i + 1), n})
	}
	specs = append(specs,
		spec{DefaultChunkerConfig(), "splitmix64_ascii", 100, 300_000},
		spec{DefaultChunkerConfig(), "zeros", 0, 200_000},
		spec{smallChunking, "splitmix64", 200, 100_000},
		spec{smallChunking, "splitmix64_ascii", 201, 50_000},
		spec{ChunkerConfig{MinSize: 4096, AvgSize: 16384, MaxSize: 131072}, "splitmix64", 300, 500_000},
		spec{ChunkerConfig{MinSize: 64, AvgSize: 256, MaxSize: 1024}, "splitmix64", 400, 20_000},
	)
	var out []boundaryCase
	for _, sp := range specs {
		f := mustCDC(t, sp.cfg)
		var data []byte
		switch sp.gen {
		case "splitmix64":
			data = splitmixBytes(sp.seed, sp.n)
		case "splitmix64_ascii":
			data = asciiBytes(sp.seed, sp.n)
		default:
			data = make([]byte, sp.n)
		}
		cuts := f.Boundaries(data)
		c := boundaryCase{
			Name:   fmt.Sprintf("%s-seed%d-len%d-avg%d", sp.gen, sp.seed, sp.n, sp.cfg.AvgSize),
			Config: infoOf(f), Generator: sp.gen, Seed: sp.seed, Length: sp.n, Cuts: cuts, SHA256: []string{},
		}
		if c.Cuts == nil {
			c.Cuts = []int{}
		}
		prev := 0
		for _, cut := range cuts {
			id := SumChunk(data[prev:cut])
			c.SHA256 = append(c.SHA256, hex.EncodeToString(id[:]))
			prev = cut
		}
		out = append(out, c)
	}
	return out
}

// CHUNK-INV-13: the gear table and the chunk boundaries of the seeded
// conformance inputs equal conformance/chunkstore/{gear,boundaries}.json,
// and the default masks are the paper's 8 KiB MaskS/MaskL.
func TestChunkInv13_ConformanceBoundaries(t *testing.T) {
	g := gearFile{
		Description: "FastCDC gear table: values[i] is the (i+1)-th output of SplitMix64 seeded with 0x46464344 (\"FFCD\"); SplitMix64: state += 0x9e3779b97f4a7c15; z = state; z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9; z = (z ^ (z >> 27)) * 0x94d049bb133111eb; return z ^ (z >> 31)",
		Generator:   "core.SplitMix64",
		Seed:        hexU64(GearSeed),
	}
	for _, v := range GearTable() {
		g.Values = append(g.Values, hexU64(v))
	}
	writeOrCompare(t, "gear.json", g)

	f := mustCDC(t, DefaultChunkerConfig())
	if f.MaskS() != 0x0000d9f003530000 || f.MaskL() != 0x0000d90003530000 {
		t.Fatalf("default masks %#x %#x differ from FastCDC 2016's 8 KiB masks", f.MaskS(), f.MaskL())
	}
	bf := boundaryFile{
		Description: "FastCDC (normalized chunking, NC=2) cut points. cuts[i] is the end offset of chunk i; chunk_sha256[i] its SHA-256. Masks: a mask with k bits sets the first k positions of mask_bit_order; MaskS has log2(avg)+2 bits, MaskL log2(avg)-2.",
		Generators: map[string]string{
			"splitmix64":       "SplitMix64(seed) outputs, each appended as 8 little-endian bytes, truncated to length",
			"splitmix64_ascii": "one SplitMix64(seed) output per byte: byte = 0x61 + (u64 mod 16)",
			"zeros":            "length zero bytes",
		},
		BitOrder: slices.Clone(maskBitOrder[:]),
		Cases:    boundaryCases(t),
	}
	writeOrCompare(t, "boundaries.json", bf)

	// Independently of the JSON bytes, decode the file and re-check every
	// case through the streaming API, as a port would.
	var loaded boundaryFile
	if err := json.Unmarshal(testkit.ReadFile(t, suite, "boundaries.json"), &loaded); err != nil {
		t.Fatal(err)
	}
	for _, c := range loaded.Cases {
		fc := mustCDC(t, ChunkerConfig{MinSize: c.Config.MinSize, AvgSize: c.Config.AvgSize, MaxSize: c.Config.MaxSize})
		var data []byte
		switch c.Generator {
		case "splitmix64":
			data = splitmixBytes(c.Seed, c.Length)
		case "splitmix64_ascii":
			data = asciiBytes(c.Seed, c.Length)
		default:
			data = make([]byte, c.Length)
		}
		got := streamCuts(t, fc, bytes.NewReader(data))
		if !slices.Equal(got, c.Cuts) && !(len(got) == 0 && len(c.Cuts) == 0) {
			t.Fatalf("%s: cuts differ", c.Name)
		}
		if hexU64(fc.MaskS()) != c.Config.MaskS || hexU64(fc.MaskL()) != c.Config.MaskL {
			t.Fatalf("%s: masks differ", c.Name)
		}
	}
	conformanceChunkers(t)
}

type chunkerCase struct {
	Name      string `json:"name"`
	Algo      string `json:"algo"`
	AlgoByte  uint8  `json:"algo_byte"`
	MinSize   int    `json:"min_size"`
	AvgSize   int    `json:"avg_size"`
	MaxSize   int    `json:"max_size"`
	Generator string `json:"generator"`
	Seed      uint64 `json:"seed"`
	Length    int    `json:"length"`
	Cuts      []int  `json:"cuts"`
	// ChunksSHA256 is the SHA-256 of the concatenated chunk ids, which
	// pins every chunk without listing hundreds of hashes.
	ChunksSHA256 string `json:"chunk_ids_sha256"`
}

type chunkersFile struct {
	Description string            `json:"description"`
	Generators  map[string]string `json:"generators"`
	Polynomial  string            `json:"rabin_polynomial"`
	Window      int               `json:"rabin_window"`
	ModTable    []string          `json:"rabin_mod_table"`
	OutTable    []string          `json:"rabin_out_table"`
	Cases       []chunkerCase     `json:"cases"`
}

func genData(t testing.TB, gen string, seed uint64, n int) []byte {
	switch gen {
	case "splitmix64":
		return splitmixBytes(seed, n)
	case "splitmix64_ascii":
		return asciiBytes(seed, n)
	case "zeros":
		return make([]byte, n)
	}
	t.Fatalf("unknown generator %q", gen)
	return nil
}

// conformanceChunkers is CHUNK-INV-13 for the Rabin, fixed-size and
// whole-blob chunkers: their cut points on seeded inputs, and the Rabin
// tables, equal conformance/chunkstore/chunkers.json (FORMAT.md §1.2-1.4);
// a port checks its chunkers against the file.
func conformanceChunkers(t *testing.T) {
	type spec struct {
		cfg  ChunkerConfig
		gen  string
		seed uint64
		n    int
	}
	var specs []spec
	for i, n := range []int{0, 1, 2047, 2048, 2049, 10_000, 65536, 65537, 300_000} {
		specs = append(specs, spec{ChunkerConfig{Algo: ChunkRabin}, "splitmix64", uint64(i + 1), n})
	}
	specs = append(specs,
		spec{ChunkerConfig{Algo: ChunkRabin}, "splitmix64_ascii", 100, 300_000},
		spec{ChunkerConfig{Algo: ChunkRabin}, "zeros", 0, 200_000},
		spec{ChunkerConfig{Algo: ChunkRabin, MinSize: 64, AvgSize: 256, MaxSize: 1024}, "splitmix64", 400, 20_000},
		spec{ChunkerConfig{Algo: ChunkRabin, MinSize: 256, AvgSize: 1024, MaxSize: 8192}, "splitmix64_ascii", 401, 50_000},
		spec{ChunkerConfig{Algo: ChunkFixed}, "splitmix64", 500, 100_000},
		spec{ChunkerConfig{Algo: ChunkFixed, AvgSize: 1000}, "splitmix64_ascii", 501, 10_001},
		spec{ChunkerConfig{Algo: ChunkWhole}, "splitmix64", 600, 100_000},
		spec{ChunkerConfig{Algo: ChunkWhole, MaxSize: 4096}, "zeros", 0, 10_000},
	)
	cf := chunkersFile{
		Description: "Rabin, fixed-size and whole-blob chunkers (FORMAT.md 1.2-1.4). Rabin: fingerprint over GF(2) modulo rabin_polynomial of the last rabin_window bytes; mod[b] = ((b << 53) mod P) | (b << 53); out[b] = fingerprint of b followed by 47 zero bytes; cut after byte i (i+1 >= min) when fp & (avg-1) == avg-1, hashing from min-48 with an all-zero window. cuts[i] is the end offset of chunk i; chunk_ids_sha256 is SHA-256 over the concatenated 32-byte chunk ids.",
		Generators: map[string]string{
			"splitmix64":       "SplitMix64(seed) outputs, each appended as 8 little-endian bytes, truncated to length",
			"splitmix64_ascii": "one SplitMix64(seed) output per byte: byte = 0x61 + (u64 mod 16)",
			"zeros":            "length zero bytes",
		},
		Polynomial: hexU64(RabinPolynomial), Window: RabinWindow,
	}
	for b := range 256 {
		cf.ModTable = append(cf.ModTable, hexU64(rabinMod[b]))
		cf.OutTable = append(cf.OutTable, hexU64(rabinOut[b]))
	}
	for _, sp := range specs {
		c := mustCutter(t, sp.cfg)
		cc := c.Config()
		data := genData(t, sp.gen, sp.seed, sp.n)
		cuts := Boundaries(c, data)
		var ids []byte
		prev := 0
		for _, x := range cuts {
			id := SumChunk(data[prev:x])
			ids = append(ids, id[:]...)
			prev = x
		}
		sum := SumChunk(ids)
		if cuts == nil {
			cuts = []int{}
		}
		cf.Cases = append(cf.Cases, chunkerCase{
			Name: fmt.Sprintf("%s-%s-seed%d-len%d-%d-%d-%d", cc.Algo, sp.gen, sp.seed, sp.n, cc.MinSize, cc.AvgSize, cc.MaxSize),
			Algo: cc.Algo.String(), AlgoByte: uint8(cc.Algo), MinSize: cc.MinSize, AvgSize: cc.AvgSize, MaxSize: cc.MaxSize,
			Generator: sp.gen, Seed: sp.seed, Length: sp.n, Cuts: cuts, ChunksSHA256: hexs(sum[:]),
		})
	}
	writeOrCompare(t, "chunkers.json", cf)

	var loaded chunkersFile
	if err := json.Unmarshal(testkit.ReadFile(t, suite, "chunkers.json"), &loaded); err != nil {
		t.Fatal(err)
	}
	for _, c := range loaded.Cases {
		algo, err := ParseChunkAlgo(c.Algo)
		if err != nil || uint8(algo) != c.AlgoByte {
			t.Fatalf("%s: algorithm %q byte %d", c.Name, c.Algo, c.AlgoByte)
		}
		cut := mustCutter(t, ChunkerConfig{Algo: algo, MinSize: c.MinSize, AvgSize: c.AvgSize, MaxSize: c.MaxSize})
		got := streamCuts(t, cut, bytes.NewReader(genData(t, c.Generator, c.Seed, c.Length)))
		if !slices.Equal(got, c.Cuts) && !(len(got) == 0 && len(c.Cuts) == 0) {
			t.Fatalf("%s: streaming cuts differ", c.Name)
		}
	}
}

type goldenRecord struct {
	Offset     uint64 `json:"offset"`
	RawLen     uint32 `json:"raw_len"`
	StoredLen  uint32 `json:"stored_len"`
	Codec      uint8  `json:"codec"`
	CRC32C     string `json:"crc32c"`
	PayloadHex string `json:"payload_hex"`
	RawSHA256  string `json:"raw_sha256"`
}

type goldenPack struct {
	Description   string         `json:"description"`
	Hex           string         `json:"hex"`
	PackID        string         `json:"pack_id"`
	CreatedUnixNs int64          `json:"created_unix_ns"`
	FooterCount   int            `json:"footer_record_count"`
	FooterCRC32C  string         `json:"footer_crc32c"`
	Records       []goldenRecord `json:"records"`
}

type goldenEntry struct {
	ID      string `json:"id"`
	Offset  uint64 `json:"offset"`
	CompLen uint32 `json:"comp_len"`
	RawLen  uint32 `json:"raw_len"`
}

type goldenIndex struct {
	Description string            `json:"description"`
	Hex         string            `json:"hex"`
	PackID      string            `json:"pack_id"`
	PackSize    uint64            `json:"pack_size"`
	PackCRC32C  string            `json:"pack_crc32c"`
	Fanout      map[string]uint32 `json:"fanout_changes"`
	Entries     []goldenEntry     `json:"entries"`
	Absent      []string          `json:"lookup_absent"`
}

type goldenManifest struct {
	Description string         `json:"description"`
	Hex         string         `json:"hex"`
	ManifestID  string         `json:"manifest_id"`
	TotalLen    uint64         `json:"total_len"`
	Chunks      []string       `json:"chunks"`
	Chunker     *goldenChunker `json:"chunker,omitempty"`
	Content     string         `json:"content"`
}

type goldenChunker struct {
	Algo     string `json:"algo"`
	AlgoByte uint8  `json:"algo_byte"`
	MinSize  int    `json:"min_size"`
	AvgSize  int    `json:"avg_size"`
	MaxSize  int    `json:"max_size"`
}

type goldenRef struct {
	Description string   `json:"description"`
	Hex         string   `json:"hex"`
	Generation  uint64   `json:"generation"`
	Packs       []string `json:"packs"`
}

type zstdFile struct {
	Description string            `json:"description"`
	Settings    map[string]string `json:"settings"`
	Input       string            `json:"input_generator"`
	FrameSHA256 string            `json:"go_klauspost_frame_sha256"`
	FrameLen    int               `json:"go_klauspost_frame_len"`
}

func hexs(b []byte) string { return hex.EncodeToString(b) }

// CHUNK-INV-14: the golden pack, index, manifest and ref files decode to
// their documented meaning, and encoding that meaning reproduces the
// exact bytes.
func TestChunkInv14_GoldenFormats(t *testing.T) {
	c, err := newCodec()
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	// Golden pack: three records (raw, zstd, raw) with a fixed timestamp.
	contents := [][]byte{[]byte("hello, flowforge"), bytes.Repeat([]byte("abcdefgh"), 40), []byte("xyz")}
	w := NewPackWriter(1_700_000_000_123_456_789)
	var es []IndexEntry
	gp := goldenPack{Description: "FFPK v1: header magic|version u16|flags u16|created_unix_ns i64; records raw_len u32|stored_len u32|codec u8|reserved[3]|crc32c(header[0:12]||payload) u32|payload; footer record_count u32|crc32c(all before) u32. Little-endian."}
	for i, x := range contents {
		codec, payload := CodecRaw, x
		if i == 1 {
			codec, payload = c.encode(x)
			if codec != CodecZstd {
				t.Fatal("repetitive input not compressed")
			}
		}
		off := w.Append(uint32(len(x)), codec, payload)
		es = append(es, IndexEntry{ID: SumChunk(x), Offset: off, CompLen: uint32(len(payload)), RawLen: uint32(len(x))})
	}
	pack, pid, pcrc := w.Finish()
	pk, err := DecodePack(pack)
	if err != nil {
		t.Fatal(err)
	}
	gp.Hex, gp.PackID, gp.CreatedUnixNs, gp.FooterCount, gp.FooterCRC32C = hexs(pack), hexs(pid[:]), pk.CreatedUnixNs, len(pk.Records), hexU32(pk.CRC)
	for i, r := range pk.Records {
		gp.Records = append(gp.Records, goldenRecord{r.Offset, r.RawLen, r.CompLen, r.Codec, hexU32(r.CRC), hexs(r.Payload(pack)), hexs(es[i].ID[:])})
	}
	writeOrCompare(t, "golden_pack.json", gp)

	ix, _ := NewIndex(pid, uint64(len(pack)), pcrc, es)
	idx := ix.Encode()
	gi := goldenIndex{
		Description: "FFPI v1: magic|version u16|flags u16|count u32|pack_crc32c u32|pack_size u64|pack_id[32]; fanout[256] u32 (cumulative count of ids with first byte <= b); ids[count][32] ascending; offsets[count] u64; comp_lens[count] u32; raw_lens[count] u32; crc32c(all before) u32. fanout_changes lists b: fanout[b] where fanout[b] != fanout[b-1].",
		Hex:         hexs(idx), PackID: hexs(pid[:]), PackSize: uint64(len(pack)), PackCRC32C: hexU32(pcrc), Fanout: map[string]uint32{},
	}
	prev := uint32(0)
	for b, v := range ix.fanout {
		if v != prev {
			gi.Fanout[strconv.Itoa(b)] = v
			prev = v
		}
	}
	for _, e := range ix.Entries {
		gi.Entries = append(gi.Entries, goldenEntry{hexs(e.ID[:]), e.Offset, e.CompLen, e.RawLen})
	}
	absent := SumChunk([]byte("absent"))
	gi.Absent = []string{hexs(absent[:])}
	writeOrCompare(t, "golden_index.json", gi)

	var ids []ChunkID
	var total uint64
	var content []byte
	for _, x := range contents {
		ids = append(ids, SumChunk(x))
		total += uint64(len(x))
		content = append(content, x...)
	}
	m := Manifest{TotalLen: total, Chunks: ids}
	mb := m.Encode()
	mid := m.ID()
	gm := goldenManifest{
		Description: "FFMF v1: magic|version u16|flags u16|total_len u64|chunk_count u32|ids[count][32]|crc32c(all before) u32. manifest_id = SHA-256 of these bytes. content is the blob (hex) the manifest describes: the golden pack's records in order.",
		Hex:         hexs(mb), ManifestID: hexs(mid[:]), TotalLen: total, Content: hexs(content),
	}
	for _, id := range ids {
		gm.Chunks = append(gm.Chunks, hexs(id[:]))
	}
	writeOrCompare(t, "golden_manifest.json", gm)

	m2 := Manifest{TotalLen: total, Chunks: ids, Chunking: ChunkerConfig{Algo: ChunkRabin, MinSize: 64, AvgSize: 256, MaxSize: 1024}}
	mb2 := m2.Encode()
	mid2 := m2.ID()
	gm2 := goldenManifest{
		Description: "FFMF v2: magic|version u16 = 2|flags u16|total_len u64|chunk_count u32|algo u8 (1 fastcdc, 2 rabin, 3 fixed, 4 whole)|reserved[3]|min_size u32|avg_size u32|max_size u32|ids[count][32]|crc32c(all before) u32. The golden v1 manifest's chunks, recorded as cut by Rabin 64/256/1024.",
		Hex:         hexs(mb2), ManifestID: hexs(mid2[:]), TotalLen: total, Content: hexs(content), Chunks: gm.Chunks,
		Chunker: &goldenChunker{Algo: "rabin", AlgoByte: uint8(ChunkRabin), MinSize: 64, AvgSize: 256, MaxSize: 1024},
	}
	writeOrCompare(t, "golden_manifest_v2.json", gm2)

	ref := Ref{Generation: 7, Packs: []PackID{pid, PackID(SumChunk([]byte("another pack")))}}
	rb, _ := ref.Encode()
	dr, _ := DecodeRef(rb)
	gr := goldenRef{Description: "FFRF v1: magic|version u16|flags u16|generation u64|pack_count u32|pack_ids[count][32] ascending|crc32c(all before) u32.", Hex: hexs(rb), Generation: 7}
	for _, p := range dr.Packs {
		gr.Packs = append(gr.Packs, hexs(p[:]))
	}
	writeOrCompare(t, "golden_ref.json", gr)

	zin := asciiBytes(7, 65536)
	frame := c.enc.EncodeAll(zin, nil)
	fsum := SumChunk(frame)
	writeOrCompare(t, "zstd.json", zstdFile{
		Description: "Pinned writer settings. Informative only: the format never depends on compressed bytes (ids hash raw data; any RFC 8878 encoder may write records). The frame hash pins the Go writer's reproducibility for this library version.",
		Settings: map[string]string{
			"level": "3 (klauspost SpeedDefault)", "window_log": "23", "frame_checksum": "on", "single_segment": "true",
			"dictionary": "none", "encoder_concurrency": "1", "gc_recompress_level": "19 (klauspost SpeedBestCompression)",
			"keep_compressed_if": "8*stored_len <= 7*raw_len", "library": "github.com/klauspost/compress v1.20.4",
		},
		Input:       "splitmix64_ascii seed 7 length 65536",
		FrameSHA256: hexs(fsum[:]), FrameLen: len(frame),
	})

	// Decode the files as a port would and check the meaning.
	var lp goldenPack
	var li goldenIndex
	var lm, lm2 goldenManifest
	var lr goldenRef
	for name, v := range map[string]any{"golden_pack.json": &lp, "golden_index.json": &li, "golden_manifest.json": &lm,
		"golden_manifest_v2.json": &lm2, "golden_ref.json": &lr} {
		if err := json.Unmarshal(testkit.ReadFile(t, suite, name), v); err != nil {
			t.Fatal(err)
		}
	}
	unhex := func(s string) []byte {
		b, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	pw := NewPackWriter(lp.CreatedUnixNs)
	for _, r := range lp.Records {
		pw.Append(r.RawLen, r.Codec, unhex(r.PayloadHex))
	}
	if got, _, _ := pw.Finish(); !bytes.Equal(got, unhex(lp.Hex)) {
		t.Fatal("golden pack: re-encoding differs")
	}
	dpk, err := DecodePack(unhex(lp.Hex))
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range dpk.Records {
		e := IndexEntry{Offset: r.Offset, CompLen: r.CompLen, RawLen: r.RawLen}
		copy(e.ID[:], unhex(lp.Records[i].RawSHA256))
		if _, err := c.openRecord(e, unhex(lp.Hex)[r.Offset:r.Offset+e.RecordSize()]); err != nil {
			t.Fatalf("golden pack record %d: %v", i, err)
		}
	}
	dix, err := DecodeIndex(unhex(li.Hex))
	if err != nil {
		t.Fatal(err)
	}
	for _, ge := range li.Entries {
		var id ChunkID
		copy(id[:], unhex(ge.ID))
		e, ok := dix.Lookup(id)
		if !ok || e.Offset != ge.Offset || e.CompLen != ge.CompLen || e.RawLen != ge.RawLen {
			t.Fatalf("golden index lookup %s", ge.ID)
		}
	}
	dm, err := DecodeManifest(unhex(lm.Hex))
	if err != nil || dm.TotalLen != lm.TotalLen || len(dm.Chunks) != len(lm.Chunks) {
		t.Fatalf("golden manifest: %v", err)
	}
	if !bytes.Equal(dm.Encode(), unhex(lm.Hex)) || SumChunk(unhex(lm.Hex)).String() != lm.ManifestID || dm.Chunking.Algo != 0 {
		t.Fatal("golden manifest: id or re-encoding differs")
	}
	dm2, err := DecodeManifest(unhex(lm2.Hex))
	if err != nil || dm2.TotalLen != lm2.TotalLen || len(dm2.Chunks) != len(lm2.Chunks) || lm2.Chunker == nil ||
		dm2.Chunking != (ChunkerConfig{Algo: ChunkAlgo(lm2.Chunker.AlgoByte), MinSize: lm2.Chunker.MinSize, AvgSize: lm2.Chunker.AvgSize, MaxSize: lm2.Chunker.MaxSize}) {
		t.Fatalf("golden manifest v2: %+v %v", dm2, err)
	}
	if !bytes.Equal(dm2.Encode(), unhex(lm2.Hex)) || SumChunk(unhex(lm2.Hex)).String() != lm2.ManifestID {
		t.Fatal("golden manifest v2: id or re-encoding differs")
	}
	if drr, err := DecodeRef(unhex(lr.Hex)); err != nil || drr.Generation != lr.Generation || len(drr.Packs) != len(lr.Packs) {
		t.Fatalf("golden ref: %v", err)
	}
}
