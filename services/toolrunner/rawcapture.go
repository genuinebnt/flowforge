package toolrunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/genuinebnt/flowforge/chunkstore"
	"github.com/genuinebnt/flowforge/chunkstore/fsstore"
	"github.com/genuinebnt/flowforge/content"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/graphstore"
)

// Raw protocol capture (spec 6.1 raw_ref, 10.5; G-SVC-16). The egress
// paths record the protocol bytes a tool received, as received: each HTTP
// response read off the connection (after TLS, before any decoding), each
// DNS message from the proxy's forwarder, and the bytes the server sent
// during a TLS handshake. An execution's records form one transcript (the
// FFRW format below) that the runner stores through a RawStore; every
// observation of the execution names the transcript recorded up to its
// emission in raw_ref.
//
// Redaction (V2 §12.11): an HTTP response can carry credentials (cookies,
// tokens in headers or bodies), so HTTP records are stored redacted:
// Set-Cookie values and credential headers become typed placeholders and
// every credential-shaped string (content.Redact, the content secret
// rules) is replaced. DNS messages and TLS handshake records hold public
// data and are stored as captured.
//
// FFRW v1 (all integers big-endian):
//
//	magic "FFRW" | version u8 = 1 | flags u8 | count u16 | count records
//	record: kind u8 | flags u8 | label_len u16 | label | data_len u32 | data
//
// Transcript flags: bit 0 = a record was cut or dropped by the limit.
// Record flags: bit 0 = truncated, bit 1 = redacted. Kinds are the RawKind
// values. Records are in capture order (the order their first byte was
// read).

// RawKind is the protocol of a raw record.
type RawKind uint8

// Raw record kinds (FFRW kind byte).
const (
	// RawHTTPResponse is one HTTP/1.1 response stream as read from its
	// connection (status line, headers, body; chunked framing kept).
	RawHTTPResponse RawKind = 1
	// RawDNSMessage is one DNS response message in RFC 1035 wire format.
	RawDNSMessage RawKind = 2
	// RawTLSHandshake is what a TLS server sent during the handshake (its
	// records: ServerHello, Certificate, ...; encrypted after the
	// ServerHello in TLS 1.3), as captured.
	RawTLSHandshake RawKind = 3
)

// Raw capture bounds.
const (
	// DefaultRawLimit bounds the bytes one execution captures (the 8 MiB
	// per-response bound of V2 §12.10); later bytes are dropped and the
	// transcript is flagged.
	DefaultRawLimit = 8 << 20
	// MaxRawRecords bounds the records of one transcript.
	MaxRawRecords = 4096
	// maxRawLabel bounds a record label.
	maxRawLabel = 1024
)

const (
	rawMagic          = "FFRW"
	rawVersion        = 1
	rawFlagTruncated  = 1 << 0
	rawFlagRedacted   = 1 << 1
	rawTranscriptHead = 8
)

// ErrRawFormat reports a byte string that is not an FFRW v1 transcript.
var ErrRawFormat = errors.New("toolrunner: malformed raw transcript")

// RawRecord is one decoded FFRW record.
type RawRecord struct {
	Kind      RawKind
	Label     string
	Data      []byte
	Truncated bool
	Redacted  bool
}

// RawTranscript is a decoded FFRW transcript.
type RawTranscript struct {
	// Truncated reports that the capture limit cut or dropped records.
	Truncated bool
	Records   []RawRecord
}

type rawRecord struct {
	kind      RawKind
	label     string
	data      []byte
	truncated bool
}

// RawCapture collects the raw records of one execution, bounded by its
// limit. It is safe for concurrent use. A nil *RawCapture records nothing.
type RawCapture struct {
	mu        sync.Mutex
	limit     int
	used      int
	recs      []*rawRecord
	truncated bool
}

// NewRawCapture returns a capture bounded to limit bytes (<= 0:
// DefaultRawLimit).
func NewRawCapture(limit int) *RawCapture {
	if limit <= 0 {
		limit = DefaultRawLimit
	}
	return &RawCapture{limit: limit}
}

// open starts a record; it returns nil when the record limit is reached.
func (c *RawCapture) open(kind RawKind, label string) *rawRecord {
	if c == nil {
		return nil
	}
	if len(label) > maxRawLabel {
		label = label[:maxRawLabel]
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.recs) >= MaxRawRecords {
		c.truncated = true
		return nil
	}
	r := &rawRecord{kind: kind, label: label}
	c.recs = append(c.recs, r)
	return r
}

// write appends p to r within the limit.
func (c *RawCapture) write(r *rawRecord, p []byte) {
	if c == nil || r == nil || len(p) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.limit - c.used
	if len(p) > room {
		p = p[:max(room, 0)]
		r.truncated, c.truncated = true, true
	}
	r.data = append(r.data, p...)
	c.used += len(p)
}

// Add records one complete message (a DNS response).
func (c *RawCapture) Add(kind RawKind, label string, data []byte) {
	c.write(c.open(kind, label), data)
}

// Conn wraps conn so that every byte read from it is appended to one new
// record. A nil capture returns conn unchanged.
func (c *RawCapture) Conn(conn net.Conn, kind RawKind, label string) net.Conn {
	if c == nil {
		return conn
	}
	return &rawConn{Conn: conn, c: c, r: c.open(kind, label)}
}

// Len returns the number of records.
func (c *RawCapture) Len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.recs)
}

// Transcript encodes the records captured so far as FFRW v1, HTTP
// records redacted. It returns nil when nothing was captured.
func (c *RawCapture) Transcript() []byte {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	recs := make([]rawRecord, len(c.recs))
	for i, r := range c.recs {
		recs[i] = rawRecord{kind: r.kind, label: r.label, data: bytes.Clone(r.data), truncated: r.truncated}
	}
	truncated := c.truncated
	c.mu.Unlock()
	if len(recs) == 0 {
		return nil
	}
	out := []RawRecord{}
	for _, r := range recs {
		rec := RawRecord{Kind: r.kind, Label: r.label, Data: r.data, Truncated: r.truncated}
		if r.kind == RawHTTPResponse {
			rec.Data, rec.Redacted = RedactHTTPResponse(r.data)
		}
		out = append(out, rec)
	}
	return EncodeRawTranscript(RawTranscript{Truncated: truncated, Records: out})
}

type rawConn struct {
	net.Conn
	c *RawCapture
	r *rawRecord
}

func (rc *rawConn) Read(p []byte) (int, error) {
	n, err := rc.Conn.Read(p)
	rc.c.write(rc.r, p[:n])
	return n, err
}

// EncodeRawTranscript writes t as FFRW v1. Labels longer than 1024 bytes
// and data longer than 2^32-1 bytes are cut (and flagged truncated).
func EncodeRawTranscript(t RawTranscript) []byte {
	recs := t.Records
	trunc := t.Truncated
	if len(recs) > MaxRawRecords {
		recs, trunc = recs[:MaxRawRecords], true
	}
	var b bytes.Buffer
	b.WriteString(rawMagic)
	flags := byte(0)
	if trunc {
		flags |= rawFlagTruncated
	}
	b.Write([]byte{rawVersion, flags})
	_ = binary.Write(&b, binary.BigEndian, uint16(len(recs)))
	for _, r := range recs {
		label, data := r.Label, r.Data
		rf := byte(0)
		if len(label) > maxRawLabel {
			label = label[:maxRawLabel]
		}
		if uint64(len(data)) > 1<<32-1 {
			data, r.Truncated = data[:1<<32-1], true
		}
		if r.Truncated {
			rf |= rawFlagTruncated
		}
		if r.Redacted {
			rf |= rawFlagRedacted
		}
		b.Write([]byte{byte(r.Kind), rf})
		_ = binary.Write(&b, binary.BigEndian, uint16(len(label)))
		b.WriteString(label)
		_ = binary.Write(&b, binary.BigEndian, uint32(len(data)))
		b.Write(data)
	}
	return b.Bytes()
}

// DecodeRawTranscript parses an FFRW v1 transcript. Errors wrap
// ErrRawFormat; nothing panics on input.
func DecodeRawTranscript(b []byte) (RawTranscript, error) {
	bad := func(msg string) (RawTranscript, error) {
		return RawTranscript{}, fmt.Errorf("%w: %s", ErrRawFormat, msg)
	}
	if len(b) < rawTranscriptHead || string(b[:4]) != rawMagic {
		return bad("bad magic")
	}
	if b[4] != rawVersion {
		return bad(fmt.Sprintf("version %d", b[4]))
	}
	if b[5]&^rawFlagTruncated != 0 {
		return bad("unknown transcript flags")
	}
	t := RawTranscript{Truncated: b[5]&rawFlagTruncated != 0}
	n := int(binary.BigEndian.Uint16(b[6:8]))
	if n > MaxRawRecords {
		return bad("too many records")
	}
	p := b[rawTranscriptHead:]
	for range n {
		if len(p) < 4 {
			return bad("short record header")
		}
		kind, rf := RawKind(p[0]), p[1]
		if kind < RawHTTPResponse || kind > RawTLSHandshake {
			return bad(fmt.Sprintf("unknown record kind %d", kind))
		}
		if rf&^(rawFlagTruncated|rawFlagRedacted) != 0 {
			return bad("unknown record flags")
		}
		ll := int(binary.BigEndian.Uint16(p[2:4]))
		p = p[4:]
		if ll > maxRawLabel || len(p) < ll+4 {
			return bad("bad label length")
		}
		label := string(p[:ll])
		p = p[ll:]
		dl := uint64(binary.BigEndian.Uint32(p[:4]))
		p = p[4:]
		if uint64(len(p)) < dl {
			return bad("data runs past the end")
		}
		t.Records = append(t.Records, RawRecord{Kind: kind, Label: label, Data: bytes.Clone(p[:dl]),
			Truncated: rf&rawFlagTruncated != 0, Redacted: rf&rawFlagRedacted != 0})
		p = p[dl:]
	}
	if len(p) != 0 {
		return bad("trailing bytes")
	}
	return t, nil
}

// credentialHeaders are response headers whose whole value is a
// credential (RFC 9110 §11.6.2, §11.7.2; common API-key headers).
var credentialHeaders = map[string]bool{"authorization": true, "proxy-authorization": true, "cookie": true,
	"x-api-key": true, "x-auth-token": true, "x-amz-security-token": true}

// RedactHTTPResponse returns an HTTP response byte stream with its
// secrets replaced (V2 §12.11): Set-Cookie values (the name and the
// attributes stay), the values of credential headers, and every
// credential-shaped string in other header values and in the body
// (content.Redact). It reports whether anything changed. Lines keep their
// CRLF framing; a body cut by the capture limit is redacted as far as it
// goes. Chunked framing is redacted as text, so a secret split across two
// chunks is not recognised (the content service, which sees the decoded
// body, keeps the encrypted finding).
func RedactHTTPResponse(b []byte) ([]byte, bool) {
	head, body, hasBody := bytes.Cut(b, []byte("\r\n\r\n"))
	lines := bytes.Split(head, []byte("\r\n"))
	changed := false
	for i := 1; i < len(lines); i++ { // line 0 is the status line
		name, value, ok := bytes.Cut(lines[i], []byte(":"))
		if !ok {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(string(name)))
		v := bytes.TrimLeft(value, " \t")
		var nv []byte
		switch {
		case key == "set-cookie":
			nv = redactSetCookie(v)
		case credentialHeaders[key]:
			if len(v) > 0 {
				nv = []byte(content.RedactionPlaceholder("credential_header", v))
			}
		default:
			if r, fs := content.Redact(v); len(fs) > 0 {
				nv = r
			}
		}
		if nv != nil && !bytes.Equal(nv, v) {
			lines[i] = append(append(bytes.Clone(name), ": "...), nv...)
			changed = true
		}
	}
	out := bytes.Join(lines, []byte("\r\n"))
	if hasBody {
		out = append(out, "\r\n\r\n"...)
		if r, fs := content.Redact(body); len(fs) > 0 {
			body, changed = r, true
		}
		out = append(out, body...)
	}
	return out, changed
}

// redactSetCookie replaces the value of "name=value; attrs" (RFC 6265
// §4.1.1) by a placeholder.
func redactSetCookie(v []byte) []byte {
	pair, attrs, hasAttrs := bytes.Cut(v, []byte(";"))
	name, val, ok := bytes.Cut(pair, []byte("="))
	if !ok || len(bytes.TrimSpace(val)) == 0 {
		return nil
	}
	out := append(bytes.Clone(name), '=')
	out = append(out, content.RedactionPlaceholder("cookie_value", bytes.TrimSpace(val))...)
	if hasAttrs {
		out = append(append(out, ';'), attrs...)
	}
	return out
}

// RawStore keeps raw transcripts; the returned ref becomes the
// observations' raw_ref (spec 10.5 graph.observations.raw_ref).
type RawStore interface {
	// PutRaw stores data for tenant, captured on day (UTC), and returns
	// its ref.
	PutRaw(ctx context.Context, tenant core.UUID, day time.Time, data []byte) (string, error)
	// GetRaw returns the bytes of a ref PutRaw returned.
	GetRaw(ctx context.Context, ref string) ([]byte, error)
}

// ErrRawRef reports a ref the store does not serve.
var ErrRawRef = errors.New("toolrunner: unknown raw ref")

// ChunkRaw is a RawStore in the chunk store (chunkstore, V2 §6.1): one
// namespace per tenant and capture day, "r<yyyymmdd>-<tenant uuid>", so
// a tenant's transcripts dedupe and compress against each other and never
// against another tenant's (V2 §12.12), and every object of a namespace
// is written on its day: an object-age lifecycle rule (spec 10.7,
// observations 90 days) removes whole namespaces, never part of one. Refs
// are "chunk://<namespace>/<manifest id>". The chunk store allows one
// writer per backend (G-CST-03), so a ChunkRaw must be the only writer of
// its backend. Safe for concurrent use.
type ChunkRaw struct {
	backend chunkstore.Backend
	cfg     chunkstore.Config

	mu     sync.Mutex
	stores map[string]*chunkstore.Store
}

// NewChunkRaw returns a ChunkRaw over b.
func NewChunkRaw(b chunkstore.Backend, cfg chunkstore.Config) *ChunkRaw {
	return &ChunkRaw{backend: b, cfg: cfg, stores: map[string]*chunkstore.Store{}}
}

const chunkRawScheme = "chunk://"

func (c *ChunkRaw) store(ctx context.Context, ns string) (*chunkstore.Store, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if s, ok := c.stores[ns]; ok {
		return s, nil
	}
	s, err := chunkstore.Open(ctx, c.backend, ns, c.cfg)
	if err != nil {
		return nil, err
	}
	c.stores[ns] = s
	return s, nil
}

// PutRaw implements RawStore.
func (c *ChunkRaw) PutRaw(ctx context.Context, tenant core.UUID, day time.Time, data []byte) (string, error) {
	ns := "r" + day.UTC().Format("20060102") + "-" + tenant.String()
	s, err := c.store(ctx, ns)
	if err != nil {
		return "", err
	}
	id, _, err := s.Put(ctx, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	return chunkRawScheme + ns + "/" + id.String(), nil
}

// GetRaw implements RawStore.
func (c *ChunkRaw) GetRaw(ctx context.Context, ref string) ([]byte, error) {
	rest, ok := strings.CutPrefix(ref, chunkRawScheme)
	ns, idText, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 {
		return nil, fmt.Errorf("%w: %s", ErrRawRef, ref)
	}
	id, err := chunkstore.ParseManifestID(idText)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrRawRef, ref)
	}
	s, err := c.store(ctx, ns)
	if err != nil {
		return nil, err
	}
	rc, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// Close closes the open namespaces.
func (c *ChunkRaw) Close(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	var errs []error
	for ns, s := range c.stores {
		errs = append(errs, s.Close(ctx))
		delete(c.stores, ns)
	}
	return errors.Join(errs...)
}

// RawStoreFor returns the raw store over the services' object store
// (services/internal/blob): the chunk store in memory for graphstore.MemBlobs,
// the chunk store in <root>/raw-chunks for graphstore.DirBlobs, and BlobRaw
// for any other store (Cloud Storage: chunkstore has no bucket backend
// yet, G-CST-02). nil gives nil (no raw capture).
func RawStoreFor(b graphstore.BlobStore, clock core.Clock) (RawStore, error) {
	cfg := chunkstore.Config{Clock: clock}
	switch b := b.(type) {
	case nil:
		return nil, nil
	case *graphstore.MemBlobs:
		return NewChunkRaw(chunkstore.NewMemBackend(), cfg), nil
	case graphstore.DirBlobs:
		d, err := fsstore.New(filepath.Join(b.Root, "raw-chunks"), fsstore.Options{})
		if err != nil {
			return nil, err
		}
		return NewChunkRaw(d, cfg), nil
	}
	return BlobRaw{Blobs: b}, nil
}

// BlobRaw is a RawStore over an object store without a chunk-store
// backend (a Cloud Storage bucket until chunkstore has one, G-CST-02):
// each transcript is one object named by its SHA-256,
// "raw/<tenant uuid>/<yyyy-mm-dd>/<sha256>.ffrw", so an identical
// transcript is stored once and bucket lifecycle rules expire objects
// individually.
type BlobRaw struct {
	Blobs graphstore.BlobStore
}

// PutRaw implements RawStore.
func (b BlobRaw) PutRaw(ctx context.Context, tenant core.UUID, day time.Time, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	return b.Blobs.Put(ctx, "raw/"+tenant.String()+"/"+day.UTC().Format("2006-01-02")+"/"+hex.EncodeToString(sum[:])+".ffrw", data)
}

// GetRaw implements RawStore.
func (b BlobRaw) GetRaw(ctx context.Context, ref string) ([]byte, error) {
	return b.Blobs.Get(ctx, ref)
}
