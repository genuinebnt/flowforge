# services/internal/blob

Object storage for raw tool output (`raw_ref`), graph snapshot files and
run archives (G-OPS-14). The port is `graphstore.BlobStore`
(`Put(name) -> uri`, `Get(uri)`); `Open` selects:

| Config | Adapter | URIs |
|---|---|---|
| `FF_ARTIFACT_BUCKET` | `GCS` (cloud.google.com/go/storage, Application Default Credentials) | `gs://bucket/name` |
| `FF_BLOB_DIR` | `graphstore.DirBlobs` (atomic write: temp file, fsync, rename) | `file:///abs/path` |
| neither | `graphstore.MemBlobs` | `mem://name` |

Names are slash-separated relative paths; `..`, empty segments and
absolute names are refused. GCS uploads are single-shot multipart requests
(every blob is built in memory first) and the client library verifies the
CRC32C the server returns.

`ZstdStored` writes RFC 8878 zstd frames of raw blocks with a content
checksum (no encoder is in go.mod; any decoder reads them, verified against
klauspost/compress outside the module); `ZstdDecodeStored` reads them back.

Tests: `TestRegress_GOPS14_BlobAdapters` runs the same round trip on all
three adapters; the GCS one talks to `chunkstore/gcsstore/gcsfake`, the
in-process fake of the JSON API subset shared with the chunk store's GCS
adapter (multipart upload, `alt=media` download with
`storage.WithJSONReads`, list, delete, generation preconditions, fault
injection), and also checks replace, idempotent delete and that a refused
read is not reported as "not found". Not covered: resumable uploads, the
XML read path and authentication.

Unlike `chunkstore/gcsstore`, this adapter writes without generation
preconditions: the BlobStore contract replaces objects (graph snapshot
files are rewritten under the same name), and raw output names are unique
per observation. The content service's chunk packs and text indexes live
in their own bucket through `chunkstore/gcsstore` (`FF_CONTENT_BUCKET`).
