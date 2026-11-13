# textindex file formats (version 1)

Three file kinds live in an index `Directory`: immutable segments
(`seg_<n>.fftx`, "FFTX"), tombstone sidecars (`seg_<n>_<g>.fftd`,
"FFTD") and the commit point (`COMMIT`, "FFTC"). `<n>` and `<g>` are 16
lower-case hex digits. All integers are little-endian. `uvarint` is
unsigned LEB128 with the minimal encoding, at most 5 bytes, value at most
2^32-1; readers reject longer, overflowing or non-minimal encodings.
CRC32C is the Castagnoli CRC (`hash/crc32` Castagnoli table; Rust `crc32c`).

A reader must check every rule marked **must**; Go's `OpenSegment` does
and returns `ErrChecksum` (CRC) or `ErrCorrupt` (structure), never reading
outside the file. Writers are deterministic: the same schema, codec and
normalized documents with generations give the same bytes.

## 1. Segment "FFTX"

```
offset 0   header (8 bytes)
offset 8   section 1 .. section 10, contiguous, in id order
           directory (4 + 10 * 24 bytes)
           footer (16 bytes)
```

### 1.1 Header

| Offset | Size | Field | Rule |
|---|---|---|---|
| 0 | 4 | magic | `"FFTX"` |
| 4 | 2 | version | must be 1 |
| 6 | 2 | posting codec | must be 1 (plain), 2 (varint) or 3 (group-varint), §1.6 |

### 1.2 Directory and footer

Directory: `section_count u32` (must be 10), then 10 entries of 24 bytes:
`id u32 | offset u64 | length u64 | crc32c u32`. Entry *i* must have
id *i*+1; section 1 must start at offset 8 and each next section where the
previous one ends; section 10 must end where the directory starts. The
CRC covers the section bytes.

Footer (last 16 bytes): `directory_offset u64 | file_crc u32 | "XTFF"`.
`file_crc` is CRC32C of every byte before it (including
`directory_offset`). `directory_offset` must equal `file_size - 16 - 244`.
Readers check the footer CRC first, then the section CRCs.

Section ids: 1 META, 2 IDS, 3 NORMS, 4 KEYWORDS, 5 NUMERICS, 6 TERMDICT,
7 POSTINGS, 8 POSITIONS, 9 TRIGRAMS, 10 STORED. In the following, *n* is
the document count, *nt* the number of text fields, and local doc ids are
0..*n*-1 in ascending external id order.

### 1.3 META (1)

```
doc_count   u32            n, must be <= 2^28
max_gen     u64            largest generation in IDS
text_count  u8             then per text field:  name_len u8 | name | sum_len u64
kw_count    u8             then per keyword field: name_len u8 | name
num_count   u8             then per numeric field: name_len u8 | name
stored_cnt  u8             then per stored field:  name_len u8 | name
```

The four name lists must equal the opening index's schema, in order
(otherwise `ErrSchemaMismatch`). Field ids used below are positions in
these lists. `sum_len` is the sum of the field's NORMS column.

### 1.4 IDS (2), NORMS (3), KEYWORDS (4), NUMERICS (5)

* IDS: `id u64[n]` (strictly increasing), then `gen u64[n]` (each <=
  `max_gen`). The size must be exactly 16*n*.
* NORMS: for each text field in order, `length u32[n]`: the field length
  in positions (`FieldLength`, used as BM25 *dl*). Size exactly 4·*n*·*nt*.
* KEYWORDS: for each keyword field: `value_count u32` (<= *n*), then
  `value_count` values, each `uvarint len | bytes`, non-empty and strictly
  increasing bytewise, then `ord u32[n]`: 0 = absent, *k* = value *k*-1.
  The section must be consumed exactly.
* NUMERICS: for each numeric field: a presence bitmap of ceil(*n*/8) bytes
  (bit *d* is byte *d*/8, bit *d*%8, LSB first; padding bits must be 0),
  then `value i64[n]` (two's complement; must be 0 where absent).

### 1.5 TERMDICT (6)

```
term_count   u32
block_count  u32                       must be ceil(term_count / 64)
block_offset u32[block_count]          relative to the start of the entries
entries: term_count times
    shared     uvarint                 bytes shared with the previous key in the block
    suffix_len uvarint
    suffix     bytes
    df         uvarint                 1 .. n
    post_off   uvarint                 offset of the term's record in POSTINGS
    pos_off    uvarint                 offset of the term's record in POSITIONS
```

A key is `field_id u8 || term`, where *term* is the analyzer's folded
UTF-8 bytes; keys are strictly increasing bytewise, 2..513 bytes, and the
field id must be < *nt*. Terms are grouped in blocks of 64; the first
entry of a block has `shared = 0` (its key is complete, so a lookup binary
searches the block offsets on first keys, then scans one block), and
`block_offset[b]` must equal the offset of entry 64*b*. Records must be
contiguous: term *i*'s `post_off`/`pos_off` equal the end of term *i*-1's
records (0 for the first), and the last ones end at the section ends.

### 1.6 POSTINGS (7) and posting codecs

The record of a term with document frequency *df*:

```
skip table: nskip = floor((df - 1) / 128) entries of 12 bytes
    last_doc  u32   last local doc id of block k
    next_post u32   offset of block k+1, relative to the first byte after the skip table
    next_pos  u32   offset of block k+1's first position, relative to pos_off
blocks: block k holds postings 128k .. min(128k + 127, df - 1),
        encoded by the segment's codec
```

Doc ids are delta coded across the whole list: the first posting stores
its doc id, every later one `doc - previous_doc` (must be >= 1), and the
first delta of block *k* > 0 is relative to `last_doc` of block *k*-1 —
which is why a reader can start decoding at any block from its skip entry.
Each posting also has a term frequency `tf` (1 .. the doc's field
length). Every skip entry must match the decoded blocks.

A block of *m* postings (deltas *d*, frequencies *f*):

| Codec | Id | Layout |
|---|---|---|
| plain | 1 | `d u32[m]` then `f u32[m]` |
| varint | 2 | `uvarint d_0, uvarint f_0, uvarint d_1, uvarint f_1, ...` |
| group-varint | 3 | `G(d)` then `G(f)` |

`G(v)` encodes *m* values in groups of four: a control byte whose bits
2*k*..2*k*+1 hold (byte length − 1) of lane *k*, then each value in that
many little-endian bytes. A final group of fewer than four values has
only its lanes; the control bits of absent lanes must be 0. Writers use
the minimal length of each value.

### 1.7 POSITIONS (8)

For each term in key order, for each posting in order, `tf` uvarints: the
first position, then `position - previous` (must be >= 1). Positions
must be < the doc's field length. Positions are not codec dependent.

### 1.8 TRIGRAMS (9)

```
count   u32
entries count times, 12 bytes, strictly increasing by trigram:
    trigram u32     b0<<16 | b1<<8 | b2, must be < 2^24
    df      u32     1 .. n
    off     u32     offset of the record in the data area (records contiguous)
data area: per trigram
    skip table: floor((df-1)/128) entries of 8 bytes: last_doc u32 | next_post u32
    blocks of doc deltas only, encoded by the codec without frequencies
        (plain: d u32[m]; varint: uvarint d per posting; group-varint: G(d))
```

A document is in a trigram's list when the trigram occurs in any of its
text fields (bytes, ASCII letters folded to lower case).

### 1.9 STORED (10)

```
block_count u32
entries block_count times, 20 bytes:
    first_doc u32 | doc_count u32 (1..16) | off u32 | comp_len u32 (>0) | raw_len u32 (<= 64 MiB)
data area: zstd frames, contiguous
```

Blocks must cover docs 0..*n*-1 in order and the data area exactly. The
writer closes a block after 16 documents or once its raw size reaches
16 KiB. A raw block holds, per document, `field_count uvarint` then per
present field `field_ref uvarint | len uvarint (>= 1) | bytes`, with
`field_ref` strictly increasing: text field *i* is *i*, stored field *j*
is *nt*+*j*. Keyword and numeric values live only in the doc-values
columns. Each block is one zstd frame (RFC 8878): klauspost/compress
level `SpeedDefault` (zstd level 3), no frame checksum, no dictionary.
The decompressed size must equal `raw_len`.

PORT: two zstd implementations need not produce the same compressed
bytes for the same level, so byte-for-byte segment equality across
languages holds for every section except STORED's data area; compare
decompressed blocks there.

## 2. Tombstones "FFTD"

Segments are immutable, so deletions are kept in a sidecar per (segment,
deletion generation), written whole at each Refresh that deletes from the
segment:

```
"FFTD" | version u16 = 1 | flags u16 = 0
doc_count u32      must equal the segment's n
seg_crc   u32      must equal the segment's footer file_crc
del_gen   u64      must equal the generation named in COMMIT
del_count u32      must equal the bitmap's population count
bitmap    ceil(n/8) bytes, bit d set = local doc d deleted (padding bits 0)
crc32c    u32      of every preceding byte
```

A plain bitmap (not roaring): segments are small enough that a dense
bitmap costs *n*/8 bytes, needs no container logic, and is O(1) to test.

## 3. Commit point "FFTC"

```
"FFTC" | version u16 = 1 | flags u16 = 0
commit_gen u64     increases by one per commit
max_gen    u64     last operation generation assigned
next_seg   u64     number of the next segment file; must exceed every listed one
seg_count  u32
per segment:
    name_len u16 | name           seg_<16 hex>.fftx
    doc_count u32 | seg_crc u32    must match the segment file
    del_gen u64                    0 = no tombstone file
    del_count u32
crc32c u32 of every preceding byte
```

The commit point is the only mutable name. It is replaced atomically
(`Directory.WriteFile`: temporary file, fsync, rename, directory fsync),
after the segment and tombstone files it names are written. A crash
leaves either the old or the new commit; files no commit names are
removed by the next `Open`. A document id must be live in at most one
listed segment.
