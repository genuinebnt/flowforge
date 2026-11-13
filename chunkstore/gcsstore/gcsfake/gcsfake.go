// Package gcsfake is an in-process fake of the subset of the Cloud
// Storage JSON API that Flowforge's GCS adapters use (chunkstore/gcsstore,
// services/internal/blob), so their tests run without a network or an
// emulator. Test support only: imported from _test.go files.
//
// Served (paths under the httptest server's URL):
//
//	POST   /upload/storage/v1/b/{bucket}/o?uploadType=multipart  single-request upload
//	GET    /storage/v1/b/{bucket}/o?prefix=&pageToken=           list, ascending by name, paged
//	GET    /storage/v1/b/{bucket}/o/{object}                     object metadata
//	GET    /storage/v1/b/{bucket}/o/{object}?alt=media           download, with Range
//	DELETE /storage/v1/b/{bucket}/o/{object}                     delete
//
// Like the real service, every write assigns a new generation, uploads and
// deletes honour ifGenerationMatch (0: the object must not exist) with 412
// conditionNotMet, an upload whose crc32c does not match its bytes is
// refused, full downloads carry X-Goog-Hash so the client verifies them,
// and a range starting at or past the end is 416. Not modelled: resumable
// uploads, metageneration preconditions, versioning, soft delete, ACLs,
// the XML API (clients must use storage.WithJSONReads) and authentication.
package gcsfake

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"cloud.google.com/go/storage"
	"google.golang.org/api/option"
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Op names one request for fault injection.
type Op struct {
	Method string // "upload", "get" (media), "attrs", "list", "delete"
	Bucket string
	Object string // "" for list; for uploads, the name query parameter
}

// Fault is what Server.Fault returns for an Op: Status 0 serves the
// request normally; otherwise the response is Status with an error body.
// With AfterCommit the request takes effect first and only the response
// is replaced (a lost acknowledgement).
type Fault struct {
	Status      int
	AfterCommit bool
}

type object struct {
	data        []byte
	gen         int64
	contentType string
}

// Server is the fake. Create it with New; it is safe for concurrent use.
type Server struct {
	// PageSize bounds list pages (default 1000, as the service).
	PageSize int
	// Fault, if set, is consulted before every request.
	Fault func(Op) Fault

	URL string

	mu      sync.Mutex
	gen     int64
	objects map[string]*object // bucket + "/" + name
	log     []string
}

// New starts a fake server that stops when t ends.
func New(t testing.TB) *Server {
	s := &Server{objects: map[string]*object{}}
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// ClientOptions point a storage client at the fake: its endpoint, no
// authentication and the JSON read path.
func (s *Server) ClientOptions() []option.ClientOption {
	return []option.ClientOption{option.WithEndpoint(s.URL + "/storage/v1/"), option.WithoutAuthentication(), storage.WithJSONReads()}
}

// Client returns a storage client on the fake, closed when t ends.
func (s *Server) Client(t testing.TB) *storage.Client {
	t.Helper()
	c, err := storage.NewClient(t.Context(), s.ClientOptions()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// Names returns the object names in bucket, sorted.
func (s *Server) Names(bucket string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		if b, n, _ := strings.Cut(k, "/"); b == bucket {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// Object returns a copy of an object's bytes and its generation.
func (s *Server) Object(bucket, name string) ([]byte, int64, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[bucket+"/"+name]
	if !ok {
		return nil, 0, false
	}
	return slices.Clone(o.data), o.gen, true
}

// Write stores data as a new generation of bucket/name, as another client
// would (no precondition), and returns the generation.
func (s *Server) Write(bucket, name string, data []byte) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gen++
	s.objects[bucket+"/"+name] = &object{data: slices.Clone(data), gen: s.gen, contentType: "application/octet-stream"}
	return s.gen
}

// Remove deletes bucket/name as another client would.
func (s *Server) Remove(bucket, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, bucket+"/"+name)
}

// Requests returns "<op> <object>" for every request served so far.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.log)
}

func writeErr(w http.ResponseWriter, code int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": reason,
		"errors": []map[string]string{{"reason": reason, "message": reason}}}})
}

func crcString(data []byte) string {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], crc32.Checksum(data, castagnoli))
	return base64.StdEncoding.EncodeToString(b[:])
}

func (o *object) resource(bucket, name string) map[string]any {
	return map[string]any{"kind": "storage#object", "bucket": bucket, "name": name, "id": bucket + "/" + name + "/" + strconv.FormatInt(o.gen, 10),
		"generation": strconv.FormatInt(o.gen, 10), "metageneration": "1", "size": strconv.Itoa(len(o.data)),
		"contentType": o.contentType, "crc32c": crcString(o.data), "storageClass": "STANDARD",
		"timeCreated": "2026-01-01T00:00:00Z", "updated": "2026-01-01T00:00:00Z"}
}

// precondition checks ifGenerationMatch against the current object
// (nil: absent). The caller holds s.mu.
func precondition(q url.Values, cur *object) bool {
	v := q.Get("ifGenerationMatch")
	if v == "" {
		return true
	}
	g, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return false
	}
	if g == 0 {
		return cur == nil
	}
	return cur != nil && cur.gen == g
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.EscapedPath()
	var op Op
	switch {
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/upload/storage/v1/b/"):
		// The client names the object in the query as well as in the
		// metadata part; faults can match on it before the body is read.
		op = Op{Method: "upload", Bucket: strings.TrimSuffix(strings.TrimPrefix(path, "/upload/storage/v1/b/"), "/o"), Object: r.URL.Query().Get("name")}
	case strings.HasPrefix(path, "/storage/v1/b/"):
		rest := strings.TrimPrefix(path, "/storage/v1/b/")
		bucket, obj, hasObj := strings.Cut(rest, "/o/")
		if !hasObj {
			bucket = strings.TrimSuffix(rest, "/o")
		}
		name, err := url.PathUnescape(obj)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "invalid")
			return
		}
		op = Op{Bucket: bucket, Object: name}
		switch {
		case !hasObj && r.Method == http.MethodGet:
			op.Method = "list"
		case hasObj && r.Method == http.MethodGet && r.URL.Query().Get("alt") == "media":
			op.Method = "get"
		case hasObj && r.Method == http.MethodGet:
			op.Method = "attrs"
		case hasObj && r.Method == http.MethodDelete:
			op.Method = "delete"
		}
	}
	if op.Method == "" {
		writeErr(w, http.StatusNotImplemented, "unsupported")
		return
	}
	var f Fault
	if s.Fault != nil {
		f = s.Fault(op)
	}
	if f.Status != 0 && !f.AfterCommit {
		s.record(op, f.Status)
		writeErr(w, f.Status, "injected")
		return
	}
	var cw http.ResponseWriter = w
	if f.Status != 0 {
		cw = &discard{h: http.Header{}}
	}
	switch op.Method {
	case "upload":
		s.upload(cw, r, op)
	case "list":
		s.list(cw, r, op)
	case "get":
		s.media(cw, r, op)
	case "attrs":
		s.attrs(cw, op)
	case "delete":
		s.delete(cw, r, op)
	}
	if f.Status != 0 {
		writeErr(w, f.Status, "injected after commit")
	}
}

func (s *Server) record(op Op, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := op.Method + " " + op.Object
	if status != 0 {
		e += " -> " + strconv.Itoa(status)
	}
	s.log = append(s.log, e)
}

// discard swallows a response whose acknowledgement is "lost".
type discard struct{ h http.Header }

func (d *discard) Header() http.Header         { return d.h }
func (d *discard) Write(b []byte) (int, error) { return len(b), nil }
func (d *discard) WriteHeader(int)             {}

func (s *Server) upload(w http.ResponseWriter, r *http.Request, op Op) {
	q := r.URL.Query()
	if q.Get("uploadType") != "multipart" {
		writeErr(w, http.StatusNotImplemented, "only multipart uploads")
		return
	}
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	mr := multipart.NewReader(r.Body, params["boundary"])
	meta, err := mr.NextPart()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var m struct {
		Name        string `json:"name"`
		ContentType string `json:"contentType"`
		CRC32C      string `json:"crc32c"`
	}
	if err := json.NewDecoder(meta).Decode(&m); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	media, err := mr.NextPart()
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	data, err := io.ReadAll(media)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if m.CRC32C != "" && m.CRC32C != crcString(data) {
		s.record(Op{Method: "upload", Object: m.Name}, http.StatusBadRequest)
		writeErr(w, http.StatusBadRequest, "crc32c mismatch")
		return
	}
	s.mu.Lock()
	key := op.Bucket + "/" + m.Name
	if !precondition(q, s.objects[key]) {
		s.mu.Unlock()
		s.record(Op{Method: "upload", Object: m.Name}, http.StatusPreconditionFailed)
		writeErr(w, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	s.gen++
	ct := m.ContentType
	if ct == "" {
		ct = "application/octet-stream"
	}
	o := &object{data: data, gen: s.gen, contentType: ct}
	s.objects[key] = o
	s.log = append(s.log, "upload "+m.Name)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(o.resource(op.Bucket, m.Name))
}

func (s *Server) list(w http.ResponseWriter, r *http.Request, op Op) {
	q := r.URL.Query()
	prefix, token := q.Get("prefix"), q.Get("pageToken")
	size := s.PageSize
	if size <= 0 {
		size = 1000
	}
	if v, err := strconv.Atoi(q.Get("maxResults")); err == nil && v > 0 && v < size {
		size = v
	}
	s.mu.Lock()
	var names []string
	for k := range s.objects {
		if b, n, _ := strings.Cut(k, "/"); b == op.Bucket && strings.HasPrefix(n, prefix) && n > token {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	next := ""
	if len(names) > size {
		names, next = names[:size], names[size-1]
	}
	items := make([]map[string]any, 0, len(names))
	for _, n := range names {
		items = append(items, s.objects[op.Bucket+"/"+n].resource(op.Bucket, n))
	}
	s.log = append(s.log, "list "+prefix)
	s.mu.Unlock()
	out := map[string]any{"kind": "storage#objects", "items": items}
	if next != "" {
		out["nextPageToken"] = next
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) lookup(op Op) (*object, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.objects[op.Bucket+"/"+op.Object]
	e := op.Method + " " + op.Object
	if !ok {
		e += " -> 404"
	}
	s.log = append(s.log, e)
	if !ok {
		return nil, false
	}
	c := *o
	return &c, true
}

func (s *Server) attrs(w http.ResponseWriter, op Op) {
	o, ok := s.lookup(op)
	if !ok {
		writeErr(w, http.StatusNotFound, "notFound")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(o.resource(op.Bucket, op.Object))
}

// media serves a download; "bytes=a-b" and "bytes=a-" ranges give 206.
func (s *Server) media(w http.ResponseWriter, r *http.Request, op Op) {
	o, ok := s.lookup(op)
	if !ok {
		writeErr(w, http.StatusNotFound, "notFound")
		return
	}
	h := w.Header()
	h.Set("Content-Type", o.contentType)
	h.Set("X-Goog-Generation", strconv.FormatInt(o.gen, 10))
	h.Set("X-Goog-Metageneration", "1")
	size := int64(len(o.data))
	rng := r.Header.Get("Range")
	if rng == "" {
		h.Set("X-Goog-Hash", "crc32c="+crcString(o.data))
		h.Set("Content-Length", strconv.FormatInt(size, 10))
		_, _ = w.Write(o.data)
		return
	}
	spec, ok := strings.CutPrefix(rng, "bytes=")
	a, b, _ := strings.Cut(spec, "-")
	start, err := strconv.ParseInt(a, 10, 64)
	if !ok || err != nil || start < 0 {
		writeErr(w, http.StatusBadRequest, "bad range")
		return
	}
	end := size - 1
	if b != "" {
		if end, err = strconv.ParseInt(b, 10, 64); err != nil || end < start {
			writeErr(w, http.StatusBadRequest, "bad range")
			return
		}
		end = min(end, size-1)
	}
	if start >= size {
		writeErr(w, http.StatusRequestedRangeNotSatisfiable, "requestedRangeNotSatisfiable")
		return
	}
	h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	h.Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = w.Write(o.data[start : end+1])
}

func (s *Server) delete(w http.ResponseWriter, r *http.Request, op Op) {
	s.mu.Lock()
	key := op.Bucket + "/" + op.Object
	cur, ok := s.objects[key]
	switch {
	case !ok:
		s.log = append(s.log, "delete "+op.Object+" -> 404")
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "notFound")
		return
	case !precondition(r.URL.Query(), cur):
		s.log = append(s.log, "delete "+op.Object+" -> 412")
		s.mu.Unlock()
		writeErr(w, http.StatusPreconditionFailed, "conditionNotMet")
		return
	}
	delete(s.objects, key)
	s.log = append(s.log, "delete "+op.Object)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}
