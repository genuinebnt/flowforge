package chunkstore

import (
	"bytes"
	"encoding/binary"
	"slices"
)

// Ref file "FFRF" layout (FORMAT.md §6). The ref names the live packs;
// replacing it is the single commit point of every flush and GC.
const (
	RefMagic      = "FFRF"
	RefVersion    = 1
	RefHeaderSize = 20 // magic[4] version u16 flags u16 generation u64 count u32
	// RefObject is the name of the ref file inside a namespace.
	RefObject = "ref"
	// NamespacePrefix starts every object name: "t/<namespace>/...".
	NamespacePrefix = "t/"
	// MaxRefPacks bounds the pack count of a ref.
	MaxRefPacks = 1 << 24
)

// Ref is a decoded ref file: the generation counter and the live packs,
// sorted and unique.
type Ref struct {
	Generation uint64
	Packs      []PackID
}

// Encode serializes the ref, sorting a copy of the pack ids. It fails on
// duplicates.
func (r *Ref) Encode() ([]byte, error) {
	ps := slices.Clone(r.Packs)
	slices.SortFunc(ps, func(a, b PackID) int { return bytes.Compare(a[:], b[:]) })
	for i := 1; i < len(ps); i++ {
		if ps[i] == ps[i-1] {
			return nil, errf(CodeRefMalformed, RefObject, "duplicate pack %s", ps[i])
		}
	}
	b := make([]byte, 0, RefHeaderSize+32*len(ps)+4)
	b = append(b, RefMagic...)
	b = binary.LittleEndian.AppendUint16(b, RefVersion)
	b = binary.LittleEndian.AppendUint16(b, 0)
	b = binary.LittleEndian.AppendUint64(b, r.Generation)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(ps)))
	for _, p := range ps {
		b = append(b, p[:]...)
	}
	return binary.LittleEndian.AppendUint32(b, CRC32C(b)), nil
}

// DecodeRef validates and decodes a ref file: size, magic, CRC32C,
// version and flags, exact length, pack ids strictly ascending.
func DecodeRef(data []byte) (*Ref, error) {
	if len(data) < RefHeaderSize+4 {
		return nil, errf(CodeRefTruncated, RefObject, "%d bytes", len(data))
	}
	if string(data[:4]) != RefMagic {
		return nil, errf(CodeRefBadMagic, RefObject, "magic %q", data[:4])
	}
	end := len(data) - 4
	if CRC32C(data[:end]) != binary.LittleEndian.Uint32(data[end:]) {
		return nil, errf(CodeRefChecksum, RefObject, "CRC32C mismatch")
	}
	version := binary.LittleEndian.Uint16(data[4:])
	flags := binary.LittleEndian.Uint16(data[6:])
	if version != RefVersion || flags != 0 {
		return nil, errf(CodeRefUnsupported, RefObject, "version %d flags %#x", version, flags)
	}
	n := uint64(binary.LittleEndian.Uint32(data[16:]))
	if n > MaxRefPacks || uint64(len(data)) != RefHeaderSize+32*n+4 {
		return nil, errf(CodeRefMalformed, RefObject, "length %d does not match %d packs", len(data), n)
	}
	r := &Ref{Generation: binary.LittleEndian.Uint64(data[8:]), Packs: make([]PackID, n)}
	for i := range r.Packs {
		copy(r.Packs[i][:], data[RefHeaderSize+32*i:])
		if i > 0 && bytes.Compare(r.Packs[i-1][:], r.Packs[i][:]) >= 0 {
			return nil, errf(CodeRefMalformed, RefObject, "pack ids not strictly ascending at %d", i)
		}
	}
	return r, nil
}

// namespace is a validated tenant namespace; it builds object names.
type namespace string

// ValidNamespace reports whether ns is 1 to 64 bytes of [a-z0-9_-].
func ValidNamespace(ns string) bool {
	if ns == "" || len(ns) > 64 {
		return false
	}
	for i := 0; i < len(ns); i++ {
		c := ns[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// newNamespace validates ns (ValidNamespace) as a CodeInvalidConfig error.
func newNamespace(ns string) (namespace, error) {
	if !ValidNamespace(ns) {
		return "", errf(CodeInvalidConfig, ns, "namespace must be 1-64 bytes of [a-z0-9_-]")
	}
	return namespace(ns), nil
}

// Object names of a namespace (FORMAT.md §7): "t/<ns>/ref" and
// "t/<ns>/packs/<pack id hex>.pack|.idx".
func (n namespace) root() string           { return NamespacePrefix + string(n) + "/" }
func (n namespace) ref() string            { return n.root() + RefObject }
func (n namespace) packs() string          { return n.root() + "packs/" }
func (n namespace) pack(id PackID) string  { return n.packs() + id.String() + ".pack" }
func (n namespace) index(id PackID) string { return n.packs() + id.String() + ".idx" }
