# chunkstore byte formats (version 1)

Normative. The Go code in this directory and the Rust port must produce
and accept exactly these bytes; `conformance/chunkstore/` holds the test
vectors. All integers are **little-endian**, unsigned unless noted.
**CRC32C** is CRC-32 with the Castagnoli polynomial (0x1EDC6F41,
reflected 0x82F63B78, init and xorout 0xFFFFFFFF; `crc32c("123456789") =
0xE3069283`). **SHA-256** is FIPS 180-4.

## 1. Chunking (FastCDC, normalized chunking)

Parameters `(min, avg, max)`; defaults **2048 / 8192 / 65536**, which
are part of the format (stores dedupe only against identical
parameters). Constraints: `avg` is a power of two `2^b` with
`8 <= b <= 20`; `64 <= min < avg < max <= 16 MiB`.

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

**Chunk id** = SHA-256 of the chunk's uncompressed bytes. It never depends
on compression.

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

```text
0    4      magic "FFMF"
4    2      version = 1
6    2      flags = 0
8    8      total_len u64 (sum of the chunk lengths)
16   4      chunk_count N
20   32*N   chunk ids, in blob order (repeats allowed)
20+32N 4    crc32c of bytes [0, 20+32N)
```

Constraints: size exactly `24 + 32N`; `N <= (MaxRecordRaw-24)/32`;
`N <= total_len <= N * MaxRecordRaw`; `N == 0` iff `total_len == 0`
(the empty blob has a 24-byte manifest). **ManifestID** = SHA-256 of
these bytes. The manifest is stored as an ordinary record (in the same
pack as the blob's last new chunks), so it is deduplicated like any chunk.
Codes: `MANIFEST_TRUNCATED`, `_BAD_MAGIC`, `_CHECKSUM`, `_UNSUPPORTED`,
`_MALFORMED`; a reader also checks that the chunks' raw lengths sum to
`total_len` (`MANIFEST_LENGTH_MISMATCH`).

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
