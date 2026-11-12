package bytecode

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Section layout (all integers little-endian; spec 8.3 with the details the
// spec leaves open filled in, see OPEN_QUESTIONS.md):
//
//	magic "FFBC" | format_version u16 | flags u16
//	CONSTS  u32 count, then (tag u8, len u32, bytes)*
//	TYPES   u32 count, then per entry: kind u8, name u32, and
//	          record:    nfields u16, (name u32, type u32)*
//	          enum:      nvariants u16, (name u32, arity u8, type u32 * arity)*
//	          signature: func u16, nparams u16, (name u32, type u32)*, result u32
//	FUNCS   u32 count, then per function: name u32, arity u8, nlocals u16,
//	          ncaps u8, code_len u32, code, nspans u32,
//	          (code_offset u32, source_offset u32, len u32)*
//	EFFECTS u16 mask
//	TOOLS   u16 count, then (name u32, spec_sha256 [32]byte)*
//	META    compiler_version (u32 len, bytes), source_sha256 [32]byte,
//	          program_policy_sha256 [32]byte, workflow_name (u32 len, bytes)
//	ANNOTS  only when flags has FlagAnnotations (see Annotations)
//	entry   u16

type writer struct{ b []byte }

func (w *writer) u8(v uint8)   { w.b = append(w.b, v) }
func (w *writer) u16(v uint16) { w.b = binary.LittleEndian.AppendUint16(w.b, v) }
func (w *writer) u32(v uint32) { w.b = binary.LittleEndian.AppendUint32(w.b, v) }
func (w *writer) u64(v uint64) { w.b = binary.LittleEndian.AppendUint64(w.b, v) }
func (w *writer) bytes(b []byte) {
	w.b = append(w.b, b...)
}
func (w *writer) str(s string) {
	w.u32(uint32(len(s)))
	w.b = append(w.b, s...)
}

func fits16(n int) bool { return n >= 0 && n <= math.MaxUint16 }
func fits32(n int) bool { return n >= 0 && uint64(n) <= math.MaxUint32 }

// Encode serializes m. Decode(Encode(m)) == m for every module Decode
// accepts, and Encode(Decode(b)) == b for every b Decode accepts.
func (m *Module) Encode() ([]byte, error) {
	w := &writer{b: make([]byte, 0, 1024)}
	w.bytes([]byte(Magic))
	w.u16(m.FormatVersion)
	w.u16(m.Flags)

	if !fits32(len(m.Consts)) {
		return nil, fmt.Errorf("%w: too many constants", ErrEncode)
	}
	w.u32(uint32(len(m.Consts)))
	for i, c := range m.Consts {
		w.u8(uint8(c.Tag))
		switch c.Tag {
		case ConstInt, ConstDuration, ConstTime:
			w.u32(8)
			w.u64(uint64(c.Int))
		case ConstFloat:
			w.u32(8)
			w.u64(math.Float64bits(c.Float))
		case ConstString:
			if !fits32(len(c.Str)) {
				return nil, fmt.Errorf("%w: constant %d too long", ErrEncode, i)
			}
			w.str(c.Str)
		default:
			return nil, fmt.Errorf("%w: constant %d has unknown tag %d", ErrEncode, i, c.Tag)
		}
	}

	if !fits32(len(m.Types)) {
		return nil, fmt.Errorf("%w: too many types", ErrEncode)
	}
	w.u32(uint32(len(m.Types)))
	for i, t := range m.Types {
		w.u8(uint8(t.Kind))
		w.u32(t.Name)
		switch t.Kind {
		case KindRecord, KindSignature:
			if t.Kind == KindSignature {
				w.u16(t.Func)
			}
			if !fits16(len(t.Fields)) {
				return nil, fmt.Errorf("%w: type %d has too many fields", ErrEncode, i)
			}
			w.u16(uint16(len(t.Fields)))
			for _, f := range t.Fields {
				w.u32(f.Name)
				w.u32(f.Type)
			}
			if t.Kind == KindSignature {
				w.u32(t.Result)
			}
		case KindEnum:
			if !fits16(len(t.Variants)) {
				return nil, fmt.Errorf("%w: type %d has too many variants", ErrEncode, i)
			}
			w.u16(uint16(len(t.Variants)))
			for _, vd := range t.Variants {
				if len(vd.Payload) > math.MaxUint8 {
					return nil, fmt.Errorf("%w: type %d variant payload too long", ErrEncode, i)
				}
				w.u32(vd.Name)
				w.u8(uint8(len(vd.Payload)))
				for _, p := range vd.Payload {
					w.u32(p)
				}
			}
		default:
			return nil, fmt.Errorf("%w: type %d has unknown kind %d", ErrEncode, i, t.Kind)
		}
	}

	if !fits32(len(m.Funcs)) {
		return nil, fmt.Errorf("%w: too many functions", ErrEncode)
	}
	w.u32(uint32(len(m.Funcs)))
	for i, f := range m.Funcs {
		if !fits32(len(f.Code)) || !fits32(len(f.Spans)) {
			return nil, fmt.Errorf("%w: function %d too large", ErrEncode, i)
		}
		w.u32(f.Name)
		w.u8(f.Arity)
		w.u16(f.NLocals)
		w.u8(f.NCaps)
		w.u32(uint32(len(f.Code)))
		w.bytes(f.Code)
		w.u32(uint32(len(f.Spans)))
		for _, s := range f.Spans {
			w.u32(s.CodeOffset)
			w.u32(s.SourceOffset)
			w.u32(s.Len)
		}
	}

	w.u16(uint16(m.Effects))
	if !fits16(len(m.Tools)) {
		return nil, fmt.Errorf("%w: too many tools", ErrEncode)
	}
	w.u16(uint16(len(m.Tools)))
	for _, t := range m.Tools {
		w.u32(t.Name)
		w.bytes(t.SpecSHA256[:])
	}
	w.str(m.Meta.CompilerVersion)
	w.bytes(m.Meta.SourceSHA256[:])
	w.bytes(m.Meta.ProgramPolicySHA256[:])
	w.str(m.Meta.WorkflowName)
	if (m.Flags&FlagAnnotations != 0) != (m.Annotations != nil) {
		return nil, fmt.Errorf("%w: FlagAnnotations and the ANNOTS section disagree", ErrEncode)
	}
	if m.Annotations != nil {
		if err := encodeAnnotations(w, m.Annotations); err != nil {
			return nil, err
		}
	}
	w.u16(m.Entry)

	if len(w.b) > MaxModuleSize {
		return nil, fmt.Errorf("%w: %d bytes exceeds the %d byte module limit", ErrTooLarge, len(w.b), MaxModuleSize)
	}
	return w.b, nil
}
