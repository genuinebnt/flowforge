package bytecode

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf8"
)

// Decoding and encoding errors. Decode wraps one of these with %w.
var (
	ErrTooLarge       = errors.New("bytecode: module exceeds 4 MiB")
	ErrBadMagic       = errors.New("bytecode: bad magic")
	ErrVersion        = errors.New("bytecode: unsupported format version")
	ErrFlags          = errors.New("bytecode: unknown flags")
	ErrTruncated      = errors.New("bytecode: truncated module")
	ErrTrailing       = errors.New("bytecode: trailing bytes after entry")
	ErrBadConst       = errors.New("bytecode: malformed constant")
	ErrBadTypeDef     = errors.New("bytecode: malformed type layout")
	ErrBadSpans       = errors.New("bytecode: malformed span table")
	ErrBadMetaField   = errors.New("bytecode: malformed META string")
	ErrBadAnnotations = errors.New("bytecode: malformed ANNOTS section")
	ErrEncode         = errors.New("bytecode: module cannot be encoded")
)

type reader struct {
	b   []byte
	off int
	err error
}

func (r *reader) fail(err error) {
	if r.err == nil {
		r.err = fmt.Errorf("%w at offset %d", err, r.off)
	}
}

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.b)-r.off {
		r.fail(ErrTruncated)
		return nil
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s
}

func (r *reader) u8() uint8 {
	if s := r.take(1); s != nil {
		return s[0]
	}
	return 0
}

func (r *reader) u16() uint16 {
	if s := r.take(2); s != nil {
		return binary.LittleEndian.Uint16(s)
	}
	return 0
}

func (r *reader) u32() uint32 {
	if s := r.take(4); s != nil {
		return binary.LittleEndian.Uint32(s)
	}
	return 0
}

func (r *reader) u64() uint64 {
	if s := r.take(8); s != nil {
		return binary.LittleEndian.Uint64(s)
	}
	return 0
}

func (r *reader) hash() (h [32]byte) {
	if s := r.take(32); s != nil {
		copy(h[:], s)
	}
	return h
}

func (r *reader) remaining() int { return len(r.b) - r.off }

// count reads a u32 count and checks that count entries of at least
// minSize bytes each can still fit, so that a forged count can never force
// a large allocation.
func (r *reader) count(n uint64, minSize int) int {
	if r.err != nil {
		return 0
	}
	if n > uint64(r.remaining()/minSize) {
		r.fail(ErrTruncated)
		return 0
	}
	return int(n)
}

func (r *reader) str(limit int) string {
	n := r.u32()
	if r.err != nil {
		return ""
	}
	if uint64(n) > uint64(limit) {
		r.fail(ErrBadMetaField)
		return ""
	}
	s := r.take(int(n))
	if s == nil {
		return ""
	}
	if !utf8.Valid(s) {
		r.fail(ErrBadMetaField)
		return ""
	}
	return string(s)
}

// Decode parses an encoded module with strict bounds checks. It rejects
// modules over MaxModuleSize, unknown versions, flags, tags and kinds,
// non-finite float constants, invalid UTF-8, unsorted span tables and
// trailing bytes. Decode checks structure only; Verify checks semantics.
func Decode(b []byte) (*Module, error) {
	if len(b) > MaxModuleSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(b))
	}
	r := &reader{b: b}
	if string(r.take(4)) != Magic {
		if r.err != nil {
			return nil, r.err
		}
		return nil, ErrBadMagic
	}
	m := &Module{}
	m.FormatVersion = r.u16()
	m.Flags = r.u16()
	if r.err == nil && m.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("%w: %d", ErrVersion, m.FormatVersion)
	}
	if r.err == nil && m.Flags&^FlagAnnotations != 0 {
		return nil, fmt.Errorf("%w: 0x%04x", ErrFlags, m.Flags)
	}

	n := r.count(uint64(r.u32()), 5)
	if n > 0 {
		m.Consts = make([]Const, 0, n)
	}
	for i := 0; i < n && r.err == nil; i++ {
		m.Consts = append(m.Consts, decodeConst(r))
	}

	n = r.count(uint64(r.u32()), 7)
	if n > 0 {
		m.Types = make([]TypeDef, 0, n)
	}
	for i := 0; i < n && r.err == nil; i++ {
		m.Types = append(m.Types, decodeType(r))
	}

	n = r.count(uint64(r.u32()), 16)
	if n > 0 {
		m.Funcs = make([]Func, 0, n)
	}
	for i := 0; i < n && r.err == nil; i++ {
		m.Funcs = append(m.Funcs, decodeFunc(r))
	}

	m.Effects = EffectMask(r.u16())
	n = r.count(uint64(r.u16()), 36)
	if n > 0 {
		m.Tools = make([]Tool, 0, n)
	}
	for i := 0; i < n && r.err == nil; i++ {
		var t Tool
		t.Name = r.u32()
		t.SpecSHA256 = r.hash()
		m.Tools = append(m.Tools, t)
	}
	m.Meta.CompilerVersion = r.str(MaxStringBytes)
	m.Meta.SourceSHA256 = r.hash()
	m.Meta.ProgramPolicySHA256 = r.hash()
	m.Meta.WorkflowName = r.str(MaxStringBytes)
	if r.err == nil && m.Flags&FlagAnnotations != 0 {
		m.Annotations = decodeAnnotations(r)
	}
	m.Entry = r.u16()
	if r.err != nil {
		return nil, r.err
	}
	if r.remaining() != 0 {
		return nil, fmt.Errorf("%w: %d bytes", ErrTrailing, r.remaining())
	}
	return m, nil
}

func decodeConst(r *reader) Const {
	c := Const{Tag: ConstTag(r.u8())}
	size := r.u32()
	if r.err != nil {
		return c
	}
	switch c.Tag {
	case ConstInt, ConstDuration, ConstTime, ConstFloat:
		if size != 8 {
			r.fail(fmt.Errorf("%w: %s constant of %d bytes", ErrBadConst, c.Tag, size))
			return c
		}
		bits := r.u64()
		if c.Tag == ConstFloat {
			c.Float = math.Float64frombits(bits)
			if math.IsNaN(c.Float) || math.IsInf(c.Float, 0) {
				r.fail(fmt.Errorf("%w: non-finite float", ErrBadConst))
			}
		} else {
			c.Int = int64(bits)
		}
	case ConstString:
		if size > MaxStringBytes {
			r.fail(fmt.Errorf("%w: string constant over 1 MiB", ErrBadConst))
			return c
		}
		s := r.take(int(size))
		if s != nil && !utf8.Valid(s) {
			r.fail(fmt.Errorf("%w: invalid UTF-8", ErrBadConst))
		}
		c.Str = string(s)
	default:
		r.fail(fmt.Errorf("%w: unknown tag %d", ErrBadConst, c.Tag))
	}
	return c
}

func decodeType(r *reader) TypeDef {
	t := TypeDef{Kind: TypeKind(r.u8()), Name: r.u32()}
	if r.err != nil {
		return t
	}
	switch t.Kind {
	case KindRecord, KindSignature:
		if t.Kind == KindSignature {
			t.Func = r.u16()
		}
		n := r.count(uint64(r.u16()), 8)
		if t.Kind == KindRecord && n > math.MaxUint8 {
			r.fail(fmt.Errorf("%w: record with %d fields (max 255)", ErrBadTypeDef, n))
			return t
		}
		if n > 0 {
			t.Fields = make([]FieldDef, n)
		}
		for i := 0; i < n; i++ {
			t.Fields[i] = FieldDef{Name: r.u32(), Type: r.u32()}
		}
		if t.Kind == KindSignature {
			t.Result = r.u32()
		}
	case KindEnum:
		n := r.count(uint64(r.u16()), 5)
		if n > math.MaxUint8+1 {
			r.fail(fmt.Errorf("%w: enum with %d variants (max 256)", ErrBadTypeDef, n))
			return t
		}
		if n > 0 {
			t.Variants = make([]VariantDef, n)
		}
		for i := 0; i < n && r.err == nil; i++ {
			vd := VariantDef{Name: r.u32()}
			k := r.count(uint64(r.u8()), 4)
			if k > 0 {
				vd.Payload = make([]uint32, k)
			}
			for j := 0; j < k; j++ {
				vd.Payload[j] = r.u32()
			}
			t.Variants[i] = vd
		}
	default:
		r.fail(fmt.Errorf("%w: unknown kind %d", ErrBadTypeDef, t.Kind))
	}
	return t
}

func decodeFunc(r *reader) Func {
	f := Func{Name: r.u32(), Arity: r.u8(), NLocals: r.u16(), NCaps: r.u8()}
	n := r.count(uint64(r.u32()), 1)
	if code := r.take(n); code != nil {
		f.Code = append([]byte(nil), code...)
	}
	ns := r.count(uint64(r.u32()), 12)
	if ns > 0 {
		f.Spans = make([]Span, ns)
	}
	for i := 0; i < ns && r.err == nil; i++ {
		s := Span{CodeOffset: r.u32(), SourceOffset: r.u32(), Len: r.u32()}
		if r.err == nil {
			if uint64(s.CodeOffset) >= uint64(len(f.Code)) || (i > 0 && s.CodeOffset <= f.Spans[i-1].CodeOffset) {
				r.fail(fmt.Errorf("%w: entry %d", ErrBadSpans, i))
			}
		}
		f.Spans[i] = s
	}
	return f
}
