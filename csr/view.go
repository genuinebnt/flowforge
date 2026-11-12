package csr

// This file is the only place the package uses unsafe. It reinterprets
// the little-endian sections of a snapshot file as typed slices so that a
// memory-mapped snapshot is used in place, without copying 0.2 GB of
// arrays on open (spec 21.1, "mmap-friendly"). Every view falls back to a
// portable copy when the host is big endian or the bytes are misaligned.

import (
	"encoding/binary"
	"math"
	"unsafe"

	"github.com/genuinebnt/flowforge/core"
)

func hostLittleEndian() bool {
	var b [2]byte
	binary.NativeEndian.PutUint16(b[:], 1)
	return b[0] == 1
}

func aligned(b []byte, n uintptr) bool {
	return uintptr(unsafe.Pointer(unsafe.SliceData(b)))%n == 0
}

func viewU64(b []byte) []uint64 {
	n := len(b) / 8
	if n == 0 {
		return []uint64{}
	}
	if hostLittleEndian() && aligned(b, unsafe.Alignof(uint64(0))) {
		// SAFETY: b holds exactly 8n bytes (len(b) is checked against the
		// header before this is called), its start is aligned for uint64,
		// the host byte order equals the file byte order, and every bit
		// pattern is a valid uint64. The view shares b's lifetime: b is a
		// Go slice (kept alive by the view) or a mapping released only by
		// Graph.Close.
		return unsafe.Slice((*uint64)(unsafe.Pointer(unsafe.SliceData(b))), n)
	}
	out := make([]uint64, n)
	for i := range out {
		out[i] = binary.LittleEndian.Uint64(b[8*i:])
	}
	return out
}

func viewU32(b []byte) []uint32 {
	n := len(b) / 4
	if n == 0 {
		return []uint32{}
	}
	if hostLittleEndian() && aligned(b, unsafe.Alignof(uint32(0))) {
		// SAFETY: as in viewU64, for 4-byte elements.
		return unsafe.Slice((*uint32)(unsafe.Pointer(unsafe.SliceData(b))), n)
	}
	out := make([]uint32, n)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(b[4*i:])
	}
	return out
}

func viewF32(b []byte) []float32 {
	n := len(b) / 4
	if n == 0 {
		return []float32{}
	}
	if hostLittleEndian() && aligned(b, unsafe.Alignof(float32(0))) {
		// SAFETY: as in viewU64; every bit pattern is a valid float32 and
		// NaN or out-of-range values are rejected by Graph.validate.
		return unsafe.Slice((*float32)(unsafe.Pointer(unsafe.SliceData(b))), n)
	}
	out := make([]float32, n)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return out
}

func viewUUIDs(b []byte) []core.UUID {
	n := len(b) / 16
	if n == 0 {
		return []core.UUID{}
	}
	// SAFETY: core.UUID is [16]byte, so it has alignment 1, no padding and
	// no byte-order dependence; b holds exactly 16n bytes.
	return unsafe.Slice((*core.UUID)(unsafe.Pointer(unsafe.SliceData(b))), n)
}
