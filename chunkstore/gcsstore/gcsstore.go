// Package gcsstore is the Cloud Storage adapter of the content storage
// ports (V2_DESIGN §6.1, §7; docs/GAPS.md G-CORE-04, G-CST-02):
//
//   - Backend implements chunkstore.Backend: each object is one GCS
//     object "<prefix><name>" ("chunks/t/<ns>/packs/<hex>.pack", ...);
//   - Directory implements textindex.Directory on "<prefix><file>", so
//     the content service's text indexes survive a Cloud Run instance
//     like its chunks do.
//
// Writes are single-request (multipart) uploads, atomic like every GCS
// object write (a reader sees the old generation or the new one in full),
// retried by the client library on transient errors (they are idempotent:
// every write carries a precondition), with the CRC32C sent so the service
// rejects a corrupted upload; full reads are checked against the service's
// CRC32C by the client library.
//
// # One writer, enforced with generation preconditions
//
// chunkstore and textindex assume one writer per namespace (G-CST-03,
// leases are v3). This adapter makes a second writer fail loudly instead
// of corrupting data. It remembers the generation of every object it has
// observed (read, listed, written; 0 for "seen absent") and makes every
// write conditional on it: ifGenerationMatch=<seen>, or ifGenerationMatch=0
// (must not exist) for an object it has not seen. A precondition failure
// is resolved by reading the object: if it already holds exactly the
// bytes being written (an earlier attempt whose acknowledgement was lost,
// or an immutable pack another attempt already stored), the write has
// happened; otherwise the error wraps ErrConflict and the remembered
// generation is kept, so every later write fails too until the store is
// reopened. Since replacing chunkstore's ref is the commit point of every
// flush and GC, a writer whose view is stale cannot commit.
//
// Deletes are conditional on the remembered generation as well. Deleting
// a pack or index (chunkstore's GC sweep) first re-reads the namespace's
// ref generation and refuses with ErrConflict when another writer moved
// it, so a sweep does not remove packs a newer writer has committed. The
// window between that check and the delete remains (closing it needs
// leases, G-CST-03); the content service therefore still runs one replica
// (infra pins max instances to 1).
//
// Deviation from the fsstore contract (by design): Put replaces only
// objects this Backend has observed. chunkstore.Store always reads the
// ref before replacing it and never replaces a pack with different
// bytes, so the Store behaves exactly as on fsstore.
//
// Reads must use the JSON API (storage.WithJSONReads) for parity with the
// tests (gcsfake serves only the JSON API); New does not enforce it.
//
// PORT: the precondition protocol is GCS-specific; an S3 port uses
// If-Match / If-None-Match on the ETag the same way.
package gcsstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"

	"github.com/genuinebnt/flowforge/chunkstore"
)

// ErrConflict reports that an object changed under this writer: another
// process wrote or deleted it since this one observed it.
var ErrConflict = errors.New("gcsstore: object changed by another writer (one writer per namespace, G-CST-03)")

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// objects is the generation-tracking object layer shared by Backend and
// Directory. Names are full object names (prefix included).
type objects struct {
	b      *storage.BucketHandle
	bucket string

	mu   sync.Mutex
	gens map[string]int64 // last observed generation; 0 = observed absent
}

func newObjects(c *storage.Client, bucket string) (*objects, error) {
	if c == nil || bucket == "" {
		return nil, errors.New("gcsstore: a client and a bucket are required")
	}
	return &objects{b: c.Bucket(bucket), bucket: bucket, gens: map[string]int64{}}, nil
}

func (o *objects) seen(name string) (int64, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	g, ok := o.gens[name]
	return g, ok
}

func (o *objects) observe(name string, gen int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.gens[name] = gen
}

// cond is the precondition of a write or delete of name.
func (o *objects) cond(name string) storage.Conditions {
	if g, ok := o.seen(name); ok && g != 0 {
		return storage.Conditions{GenerationMatch: g}
	}
	return storage.Conditions{DoesNotExist: true}
}

func status(err error) int {
	var ge *googleapi.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return 0
}

func conflict(name string, cause error) error {
	return fmt.Errorf("%w: %s: %v", ErrConflict, name, cause)
}

// get reads name in full and records its generation. found is false for
// a missing object.
func (o *objects) get(ctx context.Context, name string) (data []byte, found bool, err error) {
	r, err := o.b.Object(name).NewReader(ctx)
	if errors.Is(err, storage.ErrObjectNotExist) {
		o.observe(name, 0)
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer r.Close()
	data, err = io.ReadAll(r) // the client checks the CRC32C at EOF
	if err != nil {
		return nil, false, err
	}
	o.observe(name, r.Attrs.Generation)
	return data, true, nil
}

// put writes data to name under the remembered precondition (see the
// package comment).
func (o *objects) put(ctx context.Context, name string, data []byte) error {
	w := o.b.Object(name).If(o.cond(name)).NewWriter(ctx)
	// One chunk larger than the object makes the client send a single
	// multipart request from a buffer it can resend; ChunkSize 0 would
	// stream it unbuffered and never retry.
	w.ChunkSize = len(data) + 1
	w.ContentType = "application/octet-stream"
	w.CRC32C = crc32.Checksum(data, castagnoli)
	w.SendCRC32C = true
	_, err := w.Write(data)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		o.observe(name, w.Attrs().Generation)
		return nil
	}
	if status(err) != http.StatusPreconditionFailed {
		return err
	}
	// Lost acknowledgement or identical content: the object already holds
	// these bytes. Otherwise someone else wrote it.
	r, rerr := o.b.Object(name).NewReader(ctx)
	if rerr != nil {
		return conflict(name, err)
	}
	defer r.Close()
	cur, rerr := io.ReadAll(r)
	if rerr != nil || !bytes.Equal(cur, data) {
		return conflict(name, err)
	}
	o.observe(name, r.Attrs.Generation)
	return nil
}

// del deletes name under the remembered generation; a missing object is
// not an error.
func (o *objects) del(ctx context.Context, name string) error {
	h := o.b.Object(name)
	if g, ok := o.seen(name); ok && g != 0 {
		h = h.If(storage.Conditions{GenerationMatch: g})
	}
	err := h.Delete(ctx)
	switch {
	case err == nil, errors.Is(err, storage.ErrObjectNotExist):
		o.observe(name, 0)
		return nil
	case status(err) == http.StatusPreconditionFailed:
		return conflict(name, err)
	}
	return err
}

// list returns the object names under prefix, ascending, recording each
// generation.
func (o *objects) list(ctx context.Context, prefix string) ([]string, error) {
	q := &storage.Query{Prefix: prefix, Projection: storage.ProjectionNoACL}
	if err := q.SetAttrSelection([]string{"Name", "Generation"}); err != nil {
		return nil, err
	}
	it := o.b.Objects(ctx, q)
	var out []string
	for {
		a, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return nil, err
		}
		o.observe(a.Name, a.Generation)
		out = append(out, a.Name)
	}
	slices.Sort(out) // the service lists in UTF-8 binary order already; the contract requires it
	return out, nil
}

// Options configures a Backend.
type Options struct {
	// Prefix is prepended to every object name: "" or a path ending in
	// "/" ("chunks/"), so one bucket can hold other data beside the chunk
	// store.
	Prefix string
}

// Backend is a chunkstore.Backend on one GCS bucket. It is safe for
// concurrent use; it tracks generations only for the objects it has
// observed, so use one Backend per writer process.
type Backend struct {
	o      *objects
	prefix string
}

var _ chunkstore.Backend = (*Backend)(nil)

// New returns a backend on bucket. The client is the caller's (and is
// closed by it); production clients use Application Default Credentials
// and storage.WithJSONReads.
func New(c *storage.Client, bucket string, opts Options) (*Backend, error) {
	if opts.Prefix != "" && (!strings.HasSuffix(opts.Prefix, "/") || strings.HasPrefix(opts.Prefix, "/")) {
		return nil, fmt.Errorf("gcsstore: prefix %q must be relative and end in /", opts.Prefix)
	}
	o, err := newObjects(c, bucket)
	if err != nil {
		return nil, err
	}
	return &Backend{o: o, prefix: opts.Prefix}, nil
}

func (b *Backend) object(name string) (string, error) {
	if !chunkstore.ValidName(name) {
		return "", &chunkstore.Error{Code: chunkstore.CodeInvalidName, Object: name, Detail: "invalid object name"}
	}
	return b.prefix + name, nil
}

func notFound(name string) error {
	return &chunkstore.Error{Code: chunkstore.CodeObjectNotFound, Object: name, Detail: "no such object"}
}

// backendErr wraps a service error. ErrConflict stays reachable through
// errors.Is, under chunkstore's BACKEND code (the closed code list has no
// conflict code; core.KindUnavailable is the nearest kind).
func backendErr(name string, err error) error {
	if err == nil {
		return nil
	}
	return &chunkstore.Error{Code: chunkstore.CodeBackend, Object: name, Err: err}
}

// Put implements chunkstore.Backend.
func (b *Backend) Put(ctx context.Context, name string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := b.object(name)
	if err != nil {
		return err
	}
	return backendErr(name, b.o.put(ctx, full, data))
}

// Get implements chunkstore.Backend.
func (b *Backend) Get(ctx context.Context, name string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	full, err := b.object(name)
	if err != nil {
		return nil, err
	}
	data, found, err := b.o.get(ctx, full)
	if err != nil {
		return nil, backendErr(name, err)
	}
	if !found {
		return nil, notFound(name)
	}
	return data, nil
}

// GetRange implements chunkstore.Backend: exactly n bytes at off, or an
// error. Range reads carry no whole-object CRC; chunkstore checks each
// record's CRC32C itself.
func (b *Backend) GetRange(ctx context.Context, name string, off int64, n int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	full, err := b.object(name)
	if err != nil {
		return nil, err
	}
	if off < 0 || n < 0 {
		return nil, &chunkstore.Error{Code: chunkstore.CodeBackend, Object: name, Detail: "negative range"}
	}
	beyond := &chunkstore.Error{Code: chunkstore.CodeBackend, Object: name, Detail: fmt.Sprintf("range [%d,+%d) beyond end of object", off, n)}
	h := b.o.b.Object(full)
	if n == 0 {
		// A zero-length range request would download the whole object.
		a, err := h.Attrs(ctx)
		switch {
		case errors.Is(err, storage.ErrObjectNotExist):
			return nil, notFound(name)
		case err != nil:
			return nil, backendErr(name, err)
		case off > a.Size:
			return nil, beyond
		}
		return []byte{}, nil
	}
	r, err := h.NewRangeReader(ctx, off, int64(n))
	switch {
	case errors.Is(err, storage.ErrObjectNotExist):
		return nil, notFound(name)
	case status(err) == http.StatusRequestedRangeNotSatisfiable:
		return nil, beyond
	case err != nil:
		return nil, backendErr(name, err)
	}
	defer r.Close()
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, beyond
		}
		return nil, backendErr(name, err)
	}
	b.o.observe(full, r.Attrs.Generation)
	return buf, nil
}

// List implements chunkstore.Backend. Objects under the prefix whose
// names chunkstore would reject are skipped.
func (b *Backend) List(ctx context.Context, prefix string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	names, err := b.o.list(ctx, b.prefix+prefix)
	if err != nil {
		return nil, backendErr(prefix, err)
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if rel := strings.TrimPrefix(n, b.prefix); chunkstore.ValidName(rel) {
			out = append(out, rel)
		}
	}
	return out, nil
}

// Delete implements chunkstore.Backend. Deleting an object under a
// namespace's packs/ first checks that the namespace's ref is still at
// the generation this Backend last observed (see the package comment).
func (b *Backend) Delete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := b.object(name)
	if err != nil {
		return err
	}
	if ref, ok := refOf(name); ok {
		if err := b.checkRef(ctx, ref); err != nil {
			return backendErr(name, err)
		}
	}
	return backendErr(name, b.o.del(ctx, full))
}

// refOf maps "t/<ns>/packs/..." to "t/<ns>/ref".
func refOf(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, chunkstore.NamespacePrefix)
	if !ok {
		return "", false
	}
	ns, tail, ok := strings.Cut(rest, "/")
	if !ok || !strings.HasPrefix(tail, "packs/") {
		return "", false
	}
	return chunkstore.NamespacePrefix + ns + "/" + chunkstore.RefObject, true
}

// checkRef fails with ErrConflict unless the ref is where this Backend
// last saw it. A ref never observed fails too: only the writer that
// loaded the namespace may sweep it.
func (b *Backend) checkRef(ctx context.Context, ref string) error {
	full := b.prefix + ref
	want, ok := b.o.seen(full)
	if !ok {
		return fmt.Errorf("%w: %s not observed by this writer", ErrConflict, ref)
	}
	a, err := b.o.b.Object(full).Attrs(ctx)
	var got int64
	switch {
	case errors.Is(err, storage.ErrObjectNotExist):
	case err != nil:
		return err
	default:
		got = a.Generation
	}
	if got != want {
		return fmt.Errorf("%w: %s is at generation %d, this writer last saw %d", ErrConflict, ref, got, want)
	}
	return nil
}
