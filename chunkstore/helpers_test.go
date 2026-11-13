package chunkstore

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"maps"
	"slices"
	"sync"
	"testing"

	"github.com/genuinebnt/flowforge/core"
)

// splitmixBytes is the documented conformance generator "splitmix64":
// draw u64 values from core.NewSplitMix64(seed), append each as 8
// little-endian bytes, truncate to n.
func splitmixBytes(seed uint64, n int) []byte {
	r := core.NewSplitMix64(seed)
	b := make([]byte, 0, n+8)
	for len(b) < n {
		b = binary.LittleEndian.AppendUint64(b, r.Uint64())
	}
	return b[:n]
}

// asciiBytes is the conformance generator "splitmix64_ascii": one u64 per
// byte, byte = 'a' + (u64 % 16). Low entropy, compressible.
func asciiBytes(seed uint64, n int) []byte {
	r := core.NewSplitMix64(seed)
	b := make([]byte, n)
	for i := range b {
		b[i] = 'a' + byte(r.Uint64()%16)
	}
	return b
}

// textish returns compressible pseudo-text from r.
func textish(r core.Rng, n int) []byte {
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "fox", "golf", "hotel", "india", "juliet", "kilo", "lima", " ", "\n", "{", "}", "=", ";"}
	var b bytes.Buffer
	for b.Len() < n {
		b.WriteString(words[core.IntN(r, len(words))])
		if core.IntN(r, 8) == 0 {
			b.WriteByte(byte(core.IntN(r, 256)))
		}
	}
	return b.Bytes()[:n]
}

func randBytes(r core.Rng, n int) []byte {
	b := make([]byte, n)
	for i := 0; i < n; i += 8 {
		var w [8]byte
		binary.LittleEndian.PutUint64(w[:], r.Uint64())
		copy(b[i:], w[:])
	}
	return b
}

// smallChunking keeps tests fast while exercising every code path.
var smallChunking = ChunkerConfig{MinSize: 256, AvgSize: 1024, MaxSize: 8192}

const testNS = "tenant-a"

func openStore(t testing.TB, b Backend, cfg Config) *Store {
	t.Helper()
	s, err := Open(context.Background(), b, testNS, cfg)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func mustPut(t testing.TB, s *Store, data []byte) (ManifestID, Stats) {
	t.Helper()
	id, st, err := s.Put(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Put(%d bytes): %v", len(data), err)
	}
	return id, st
}

func getAll(s *Store, id ManifestID) ([]byte, error) {
	rc, err := s.Get(context.Background(), id)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

func mustGet(t testing.TB, s *Store, id ManifestID) []byte {
	t.Helper()
	b, err := getAll(s, id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return b
}

// snapshot returns a copy of every object of a MemBackend.
func snapshot(m *MemBackend) map[string][]byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string][]byte, len(m.objs))
	for k, v := range m.objs {
		out[k] = slices.Clone(v)
	}
	return out
}

func memFrom(objs map[string][]byte) *MemBackend {
	m := NewMemBackend()
	for k, v := range objs {
		m.objs[k] = slices.Clone(v)
	}
	return m
}

func sameObjects(a, b map[string][]byte) bool {
	return maps.EqualFunc(a, b, bytes.Equal)
}

var errInjected = errors.New("injected crash")

// faultBackend wraps a backend and simulates a crash at the k-th mutating
// call (Put or Delete, counted from 0): that call and every later
// mutating call fail. With ackLost the k-th call reaches the inner
// backend before failing (the write happened, the acknowledgement was
// lost). k < 0 never fails; ops counts mutating calls.
type faultBackend struct {
	Backend
	mu      sync.Mutex
	k       int
	ackLost bool
	ops     int
}

func (f *faultBackend) step() (doInner, fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := f.ops
	f.ops++
	switch {
	case f.k < 0 || n < f.k:
		return true, false
	case n == f.k:
		return f.ackLost, true
	default:
		return false, true
	}
}

func (f *faultBackend) Put(ctx context.Context, name string, data []byte) error {
	inner, fail := f.step()
	if inner {
		if err := f.Backend.Put(ctx, name, data); err != nil {
			return err
		}
	}
	if fail {
		return errInjected
	}
	return nil
}

func (f *faultBackend) Delete(ctx context.Context, name string) error {
	inner, fail := f.step()
	if inner {
		if err := f.Backend.Delete(ctx, name); err != nil {
			return err
		}
	}
	if fail {
		return errInjected
	}
	return nil
}

// mutate applies a few small random edits (insert, delete, overwrite).
func mutate(r core.Rng, data []byte, edits int) []byte {
	out := slices.Clone(data)
	for range edits {
		if len(out) == 0 {
			out = append(out, randBytes(r, 1+core.IntN(r, 64))...)
			continue
		}
		pos := core.IntN(r, len(out))
		n := 1 + core.IntN(r, 64)
		switch core.IntN(r, 3) {
		case 0:
			out = slices.Insert(out, pos, randBytes(r, n)...)
		case 1:
			out = slices.Delete(out, pos, min(len(out), pos+n))
		default:
			copy(out[pos:], randBytes(r, n))
		}
	}
	return out
}
