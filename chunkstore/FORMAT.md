# chunkstore byte formats

Normative. The Go code in this directory and the Rust port must produce
and accept exactly these bytes; `conformance/chunkstore/` holds the test
vectors. All integers are **little-endian**, unsigned unless noted.
**CRC32C** is CRC-32 with the Castagnoli polynomial (0x1EDC6F41,
reflected 0x82F63B78, init and xorout 0xFFFFFFFF; `crc32c("123456789") =
0xE3069283`). **SHA-256** is FIPS 180-4.

## 1. Chunking

A chunker is an algorithm byte and three sizes `(min, avg, max)`, the
*descriptor* that version-2 manifests record (§5). Stores dedupe only
between blobs cut by the same descriptor. The canonical descriptors:

| algo byte | Name | Sizes | Defaults |
|---|---|---|---|
| 1 | FastCDC (§1.1, the default) | `(min, avg, max)` | 2048 / 8192 / 65536 |
| 2 | Rabin (§1.2) | `(min, avg, max)` | 2048 / 8192 / 65536 |
| 3 | fixed-size (§1.3) | `(size, size, size)` | 8192 |
| 4 | whole blob (§1.4) | `(0, 0, max)` | 16 MiB |

For FastCDC and Rabin, `avg` is a power of two `2^b` with
`8 <= b <= 20` and `64 <= min < avg < max <= 16 MiB`; a fixed size is in
`[64, 16 MiB]`; a whole-blob `max` is in `[64, 16 MiB]`. Every chunker
cuts a stream by repeated cuts from offset 0, and every cut depends only
on the bytes from the previous cut on; an empty stream has no chunks.
`conformance/chunkstore/chunkers.json` holds cut points of the
non-FastCDC chunkers on seeded inputs.

### 1.1 FastCDC (normalized chunking)

Parameters `(min, avg, max)`; defaults **2048 / 8192 / 65536**.

**Gear table.** `gear[i]` for `i = 0..255` is the `(i+1)`-th output of
SplitMix64 seeded with `0x46464344` ("FFCD" as a big-endian u32):

```text
state += 0x9e3779b97f4a7c15
z = state
z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
z = (z ^ (z >> 27)) * 0x94d049bb133111eb
return z ^ (z >> 31)                       (all arithmetic mod 2^64)
```

`gear[0] = 0xd815cc1108e4a769`, `gear[1] = 0xa022ef08eccf3245`; the whole
table is `conformance/chunkstore/gear.json`.

**Masks.** Normalization level NC = 2. A mask with `k` one bits sets the
first `k` positions of this ordered bit list:

```text
16 17 20 22 24 25 40 43 44 46 47 | 36 37 | 38 39 | 41 42 45 26 27 28 29
```

`MaskS` has `b + 2` bits, `MaskL` has `b - 2` bits. For the default
`avg = 8192` (`b = 13`) these are exactly the FastCDC 2016 paper's 8 KiB
masks:

| Mask | Bits | Value |
|------|------|-------|
| MaskS | 15 | `0x0000d9f003530000` |
| (MaskA, unused) | 13 | `0x0000d93003530000` |
| MaskL | 11 | `0x0000d90003530000` |

**Cut.** The length of the first chunk of `data` (paper Algorithm 2 with
normalized chunking; cut-point skipping of the first `min` bytes):

```text
n = len(data)
if n <= min: return n
if n > max: n = max
normal = min(avg, n)
fp = 0
for i in [min, normal):  fp = (fp << 1) + gear[data[i]];  if fp & MaskS == 0: return i
for i in [normal, n):    fp = (fp << 1) + gear[data[i]];  if fp & MaskL == 0: return i
return n
```

The byte at the returned index took part in the decision but begins the
next chunk. A stream is chunked by repeated cuts from offset 0; an empty
stream has no chunks. Every chunk but the last is in `[min, max]`; the
last is in `[1, max]`. Boundaries depend only on the bytes.

### 1.2 Rabin fingerprint

The LBFS 2001 scheme with fixed constants. Fingerprints are polynomials
over GF(2) reduced modulo the irreducible degree-53 polynomial
`P = 0x3DA3358B4DC173` (bit `i` is the coefficient of `x^i`), over a
rolling window of `W = 48` bytes. Two 256-entry tables (also in
`chunkers.json`):

```text
mod[b] = ((b << 53) mod P) | (b << 53)
out[b] = fingerprint of the W bytes  b, 0, 0, ..., 0
```

where `x mod P` is GF(2) polynomial division (XOR in shifted copies of
`P` while `deg(x) >= 53`) and the fingerprint of a byte string is
`fp = ((fp << 8) | byte) mod P` folded over it from `fp = 0`. Cut:

```text
n = len(data)
if n <= min: return n
if n > max: n = max
fp = 0; window = W zero bytes (a ring; w = 0)
for i in [min - W, n):
    fp ^= out[window[w]];  window[w] = data[i];  w = (w + 1) mod W
    fp = ((fp << 8) | data[i]) ^ mod[fp >> 45]
    if i + 1 >= min and fp & (avg - 1) == avg - 1: return i + 1
return n
```

The cut follows the window's last byte (unlike FastCDC's). Starting the
window at `min - W` with zero bytes (whose fingerprint is 0) makes the
first test see exactly `data[min-W : min]`. Testing for all-one low bits
keeps runs of zero bytes (fingerprint 0) from cutting at every `min`.

### 1.3 Fixed-size

`cut(data) = min(len(data), size)`.

### 1.4 Whole blob

`cut(data) = min(len(data), max)`: one chunk per blob, split every `max`
bytes (a record holds at most 64 MiB; the default `max` is 16 MiB).

### Chunk ids

**Chunk id** = SHA-256 of the chunk's uncompressed bytes. It never depends
on compression or on the chunker.

## 2. Compression

A record payload is either the raw chunk (codec 0) or **one zstd frame**
(codec 1, RFC 8878) that decodes to exactly `raw_len` bytes. No
dictionary in version 1. Readers must accept any valid frame; the format
never depends on the compressed bytes (identity is SHA-256 of raw data,
integrity is re-checked after decoding).

Writer rule (both at ingest and when GC recompresses): keep the zstd
payload only if it saves at least 1/8 of the raw size, i.e.
`8 * stored_len <= 7 * raw_len`; otherwise store raw.

Pinned Go writer settings (informative for reproducibility, see §8):

| Setting | Value |
|---|---|
| ingest level | 3 → klauspost `SpeedDefault` (`EncoderLevelFromZstd(3)`) |
| GC recompress level | 19 (configurable) → klauspost `SpeedBestCompression` |
| window | 2^23 (8 MiB) |
| frame checksum | on (XXH64 content checksum) |
| single segment | on (no window descriptor; frame content size present) |
| encoder concurrency | 1 |
| dictionary | none |

## 3. Packfile "FFPK"

```text
offset  size  field
0       4     magic "FFPK" (46 46 50 4B)
4       2     version = 1
6       2     flags = 0
8       8     created_unix_ns (i64; 0 when the writer has no clock)
16      ...   records, back to back
end-8   4     record_count u32
end-4   4     crc32c of bytes [0, end-4)
```

Record (16-byte header, then payload):

```text
+0   4   raw_len     u32, 1 ..= 67108864 (MaxRecordRaw, 64 MiB)
+4   4   stored_len  u32, payload length, >= 1; == raw_len when codec = 0
+8   1   codec       0 = raw, 1 = zstd
+9   3   reserved    zero
+12  4   crc32c      over header bytes [+0, +12) followed by the payload
+16  stored_len payload
```

Validation order (decoders): length `< 24` → `PACK_TRUNCATED`; magic →
`PACK_BAD_MAGIC`; footer CRC → `PACK_CHECKSUM`; version/flags →
`PACK_UNSUPPORTED`; then the record walk: a partial header, a record
overrunning the body, a bad codec, non-zero reserved bytes, a raw
record whose lengths differ, or a count different from the footer →
`PACK_MALFORMED`; a record CRC mismatch → `RECORD_CHECKSUM`.

**Pack id** = SHA-256 of the whole pack file. Object names:
`t/<ns>/packs/<hex pack id>.pack` and `.idx`.

## 4. Pack index "FFPI"

```text
offset            size     field
0                 4        magic "FFPI"
4                 2        version = 1
6                 2        flags = 0
8                 4        count N (<= 2^26)
12                4        pack_crc32c (the pack's footer CRC)
16                8        pack_size
24                32       pack_id
56                1024     fanout[256] u32: number of ids whose first byte <= b
1080              32*N     ids, strictly ascending (bytewise)
1080+32N          8*N      offsets u64 (of the record header in the pack)
1080+40N          4*N      comp_lens u32 (= stored_len)
1080+44N          4*N      raw_lens u32
1080+48N          4        crc32c of bytes [0, 1080+48N)
```

Size is exactly `1084 + 48N`. Lookup: `lo = b == 0 ? 0 : fanout[b-1]`,
`hi = fanout[b]` for `b = id[0]`, then binary search in `ids[lo:hi]`.
Validation after magic/CRC/version (`INDEX_TRUNCATED`, `INDEX_BAD_MAGIC`,
`INDEX_CHECKSUM`, `INDEX_UNSUPPORTED`): exact size, ids strictly
ascending, fan-out equal to the cumulative first-byte counts, raw_len in
`[1, MaxRecordRaw]`, comp_len ≥ 1, records inside `[16, pack_size-8)` and
non-overlapping → else `INDEX_MALFORMED`. An index is a pure function of
`(pack_id, pack_size, pack_crc32c, entry set)`.

## 5. Manifest "FFMF"

Version 2 (written by every Put):

```text
0    4      magic "FFMF"
4    2      version = 2
6    2      flags = 0
8    8      total_len u64 (sum of the chunk lengths)
16   4      chunk_count N
20   1      algo (§1: 1 FastCDC, 2 Rabin, 3 fixed-size, 4 whole blob)
21   3      reserved = 0
24   4      min_size u32
28   4      avg_size u32
32   4      max_size u32
36   32*N   chunk ids, in blob order (repeats allowed)
36+32N 4    crc32c of bytes [0, 36+32N)
```

Version 1 (written before chunkers were recorded; still read, and
written only for a manifest whose chunker is unset): the same without
bytes 20-35, so ids start at 20 and the CRC is at `20+32N`.

Constraints: size exactly `40 + 32N` (version 2) or `24 + 32N`
(version 1); `N <= (MaxRecordRaw-40)/32` (version 2) or
`(MaxRecordRaw-24)/32` (version 1); `N <= total_len <= N * MaxRecordRaw`;
`N == 0` iff `total_len == 0`; in version 2 the descriptor is canonical
(§1 table) and valid. **ManifestID** = SHA-256 of these bytes, so the
same content cut by two chunkers has two manifests (both readable; their
chunks are deduplicated wherever the cuts agree). The recorded chunker
re-cuts the blob into exactly its chunk list (CHUNK-INV-24), and fsck
checks the chunk lengths against its bounds (`MANIFEST_CHUNKING_MISMATCH`).
The manifest is stored as an ordinary record (in the same pack as the
blob's last new chunks), so it is deduplicated like any chunk. Codes:
`MANIFEST_TRUNCATED`, `_BAD_MAGIC`, `_CHECKSUM`, `_UNSUPPORTED` (also an
unknown algorithm byte), `_MALFORMED` (also non-zero reserved bytes or a
non-canonical descriptor); a reader also checks that the chunks' raw
lengths sum to `total_len` (`MANIFEST_LENGTH_MISMATCH`). Golden files:
`golden_manifest.json` (version 1), `golden_manifest_v2.json`.

## 6. Ref "FFRF"

```text
0    4      magic "FFRF"
4    2      version = 1
6    2      flags = 0
8    8      generation u64 (incremented on every replace)
16   4      pack count N (<= 2^24)
20   32*N   pack ids, strictly ascending
20+32N 4    crc32c of bytes [0, 20+32N)
```

Object name `t/<ns>/ref`. The ref is the root: a pack exists for readers
only once the ref names it. Codes: `REF_TRUNCATED`, `_BAD_MAGIC`,
`_CHECKSUM`, `_UNSUPPORTED`, `_MALFORMED`.

## 7. Namespaces and write ordering

Every object of a store lives under `t/<ns>/`, where `ns` is 1–64 bytes
of `[a-z0-9_-]` (one tenant). Nothing is shared across namespaces.

* **Flush (seal):** write the pack, then its index, then the new ref.
* **GC:** write new packs and indexes; replace the ref; delete the
  replaced packs and indexes and any other object under `t/<ns>/packs/`
  the ref does not name.

A crash at any point leaves a ref naming only complete packs with
complete indexes; leftovers are orphans that fsck reports as
`ORPHAN_OBJECT` warnings and the next GC deletes.

## 8. Reading and verification

To read chunk `id`: find `(pack, offset, comp_len, raw_len)` in the
indexes; read `16 + comp_len` bytes at `offset`; require the header
lengths to equal the index (`RECORD_MALFORMED`), the record CRC
(`RECORD_CHECKSUM`), a known codec and zero reserved bytes
(`RECORD_MALFORMED`), decoding to exactly `raw_len`
(`CHUNK_DECOMPRESS`, `CHUNK_LENGTH_MISMATCH`), and SHA-256 equal to `id`
(`CHUNK_HASH_MISMATCH`).

**zstd determinism.** For fixed settings the klauspost encoder's
`EncodeAll` is byte-deterministic (verified across encoder instances,
repeated calls and encoder concurrency 1 vs 8 by CHUNK-INV-12), so the Go
writer reproduces packs, indexes and refs exactly from the same inputs
and clock. The C zstd library used by the Rust crate produces different
frames; that is allowed. Manifest bytes and ManifestIDs are identical
across implementations; pack, index and ref bytes are identical only for
the same compressor (the golden files pin a frame so a port can
round-trip them byte for byte).
