package dnsmsg

import (
	"encoding/binary"
	"io"
)

// MaxMessageLen is the largest DNS message in octets (the TCP length
// prefix is 16 bits, RFC 1035 §4.2.2).
const MaxMessageLen = 65535

// encoder appends a message with canonical name compression. comp maps the
// uncompressed wire form of every name suffix written so far (exact
// octets) to its first offset; added lists the keys in insertion order so
// a record can be rolled back (EncodeTruncated).
type encoder struct {
	buf   []byte
	comp  map[string]int
	added []string
}

func newEncoder() *encoder {
	return &encoder{buf: make([]byte, HeaderLen, 512), comp: map[string]int{}}
}

// mark is a rollback point: the buffer length and the number of
// compression keys added.
type mark struct{ n, added int }

func (e *encoder) mark() mark { return mark{len(e.buf), len(e.added)} }

func (e *encoder) rollback(m mark) {
	e.buf = e.buf[:m.n]
	for _, k := range e.added[m.added:] {
		delete(e.comp, k)
	}
	e.added = e.added[:m.added]
}

// name appends s, pointing at the longest previously written suffix when
// compress is set, and records its own suffixes for later names.
func (e *encoder) name(s string, compress bool) error {
	w, err := appendNameWire(nil, s)
	if err != nil {
		return err
	}
	// Label start offsets within w (the root is not a label).
	var starts []int
	for i := 0; w[i] != 0; i += 1 + int(w[i]) {
		starts = append(starts, i)
	}
	cut, ptr := len(w)-1, -1 // write w[:cut], then the pointer or the root
	if compress {
		for _, i := range starts {
			if off, ok := e.comp[string(w[i:])]; ok {
				cut, ptr = i, off
				break
			}
		}
	}
	base := len(e.buf)
	if compress {
		for _, i := range starts {
			if i >= cut {
				break
			}
			k := string(w[i:])
			// A pointer has 14 offset bits (RFC 1035 §4.1.4), so a suffix
			// at 0x4000 or later cannot be pointed at.
			if _, ok := e.comp[k]; !ok && base+i < 0x4000 {
				e.comp[k] = base + i
				e.added = append(e.added, k)
			}
		}
	}
	e.buf = append(e.buf, w[:cut]...)
	if ptr >= 0 {
		e.buf = append(e.buf, 0xC0|byte(ptr>>8), byte(ptr))
	} else {
		e.buf = append(e.buf, 0)
	}
	return nil
}

func (e *encoder) u16(v uint16) { e.buf = binary.BigEndian.AppendUint16(e.buf, v) }
func (e *encoder) u32(v uint32) { e.buf = binary.BigEndian.AppendUint32(e.buf, v) }

func (e *encoder) question(q Question) error {
	if err := e.name(q.Name, true); err != nil {
		return err
	}
	e.u16(uint16(q.Type))
	e.u16(uint16(q.Class))
	return nil
}

func (e *encoder) rr(r RR) error {
	if err := e.name(r.Name, true); err != nil {
		return err
	}
	e.u16(uint16(r.Type))
	e.u16(uint16(r.Class))
	e.u32(r.TTL)
	lenAt := len(e.buf)
	e.u16(0)
	if err := e.rdata(r.Type, r.Data); err != nil {
		return err
	}
	n := len(e.buf) - lenAt - 2
	if n > 0xFFFF {
		return fail(CodeMessageTooLarge, -1)
	}
	binary.BigEndian.PutUint16(e.buf[lenAt:], uint16(n))
	return nil
}

func (e *encoder) rdata(t Type, d RData) error {
	if d.Kind != KindRaw && d.Kind != KindFor(t) {
		return fail(CodeKindMismatch, -1)
	}
	switch d.Kind {
	case KindRaw:
		e.buf = append(e.buf, d.Raw...)
	case KindA:
		if !d.IP.Is4() {
			return fail(CodeKindMismatch, -1)
		}
		v := d.IP.As4()
		e.buf = append(e.buf, v[:]...)
	case KindAAAA:
		if !d.IP.Is6() || d.IP.Zone() != "" {
			return fail(CodeKindMismatch, -1)
		}
		v := d.IP.As16()
		e.buf = append(e.buf, v[:]...)
	case KindName:
		return e.name(d.Target, true)
	case KindMX:
		e.u16(d.Preference)
		return e.name(d.Target, true)
	case KindSRV:
		e.u16(d.Priority)
		e.u16(d.Weight)
		e.u16(d.Port)
		return e.name(d.Target, false)
	case KindSOA:
		if err := e.name(d.SOA.MName, true); err != nil {
			return err
		}
		if err := e.name(d.SOA.RName, true); err != nil {
			return err
		}
		for _, v := range []uint32{d.SOA.Serial, d.SOA.Refresh, d.SOA.Retry, d.SOA.Expire, d.SOA.Minimum} {
			e.u32(v)
		}
	case KindTXT:
		for _, s := range d.Strings {
			if len(s) > 255 {
				return fail(CodeStringTooLong, -1)
			}
			e.buf = append(e.buf, byte(len(s)))
			e.buf = append(e.buf, s...)
		}
	case KindCAA:
		if len(d.CAATag) == 0 || len(d.CAATag) > 255 {
			return fail(CodeStringTooLong, -1)
		}
		e.buf = append(e.buf, d.CAAFlags, byte(len(d.CAATag)))
		e.buf = append(e.buf, d.CAATag...)
		e.buf = append(e.buf, d.CAAValue...)
	case KindOPT:
		for _, o := range d.Options {
			if len(o.Data) > 0xFFFF {
				return fail(CodeMessageTooLarge, -1)
			}
			e.u16(o.Code)
			e.u16(uint16(len(o.Data)))
			e.buf = append(e.buf, o.Data...)
		}
	default:
		return fail(CodeKindMismatch, -1)
	}
	return nil
}

func (e *encoder) header(h Header, counts [4]int) error {
	for _, c := range counts {
		if c > 0xFFFF {
			return fail(CodeTooManyRecords, -1)
		}
	}
	f := uint16(h.Opcode&0xF)<<11 | uint16(h.RCode&0xF)
	for _, b := range []struct {
		on  bool
		bit uint
	}{{h.Response, 15}, {h.Authoritative, 10}, {h.Truncated, 9}, {h.RecursionDesired, 8}, {h.RecursionAvailable, 7},
		{h.Zero, 6}, {h.AuthenticData, 5}, {h.CheckingDisabled, 4}} {
		if b.on {
			f |= 1 << b.bit
		}
	}
	binary.BigEndian.PutUint16(e.buf[0:], h.ID)
	binary.BigEndian.PutUint16(e.buf[2:], f)
	for i, c := range counts {
		binary.BigEndian.PutUint16(e.buf[4+2*i:], uint16(c))
	}
	return nil
}

// Encode returns the canonical wire form of m. The section counts come
// from the slices; Header.Opcode and Header.RCode keep their low 4 bits.
func Encode(m Message) ([]byte, error) {
	e := newEncoder()
	for _, q := range m.Questions {
		if err := e.question(q); err != nil {
			return nil, err
		}
	}
	for _, s := range [][]RR{m.Answers, m.Authority, m.Additional} {
		for _, r := range s {
			if err := e.rr(r); err != nil {
				return nil, err
			}
		}
	}
	if len(e.buf) > MaxMessageLen {
		return nil, fail(CodeMessageTooLarge, -1)
	}
	if err := e.header(m.Header, [4]int{len(m.Questions), len(m.Answers), len(m.Authority), len(m.Additional)}); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// EncodeTruncated encodes m in at most max octets (RFC 1035 §4.2.1, RFC
// 2181 §9). When m does not fit, the result keeps the header, the
// questions, as many whole answer records as fit (in order) and the OPT
// record, drops the authority and other additional records, and has TC
// set; truncated reports that. When the header and questions fit but the
// OPT does not, the OPT is dropped too; when not even the header and
// questions fit, it fails with CodeMessageTooLarge.
func EncodeTruncated(m Message, max int) (b []byte, truncated bool, err error) {
	full, err := Encode(m)
	if err != nil {
		return nil, false, err
	}
	if len(full) <= max {
		return full, false, nil
	}
	var opt []RR
	for _, r := range m.Additional {
		if r.Type == TypeOPT {
			opt = append(opt, r)
			break
		}
	}
	for {
		b, err := encodePrefix(m, opt, max)
		if err == nil {
			return b, true, nil
		}
		if len(opt) == 0 {
			return nil, false, err
		}
		opt = nil
	}
}

// encodePrefix encodes the questions, the longest prefix of the answers
// that fits next to opt, and opt, with TC set.
func encodePrefix(m Message, opt []RR, max int) ([]byte, error) {
	optLen := 0
	if len(opt) > 0 {
		t := newEncoder()
		if err := t.rr(opt[0]); err != nil {
			return nil, err
		}
		optLen = len(t.buf) - HeaderLen // the OPT owner is the root: never compressed
	}
	e := newEncoder()
	for _, q := range m.Questions {
		if err := e.question(q); err != nil {
			return nil, err
		}
	}
	if len(e.buf)+optLen > max {
		return nil, fail(CodeMessageTooLarge, -1)
	}
	n := 0
	for _, r := range m.Answers {
		mk := e.mark()
		if err := e.rr(r); err != nil {
			return nil, err
		}
		if len(e.buf)+optLen > max {
			e.rollback(mk)
			break
		}
		n++
	}
	for _, r := range opt {
		if err := e.rr(r); err != nil {
			return nil, err
		}
	}
	h := m.Header
	h.Truncated = true
	if err := e.header(h, [4]int{len(m.Questions), n, 0, len(opt)}); err != nil {
		return nil, err
	}
	return e.buf, nil
}

// AppendTCP appends msg with its two-octet length prefix (RFC 1035
// §4.2.2).
func AppendTCP(dst, msg []byte) ([]byte, error) {
	if len(msg) > MaxMessageLen {
		return dst, fail(CodeMessageTooLarge, -1)
	}
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(msg)))
	return append(dst, msg...), nil
}

// WriteTCP writes msg with its length prefix in one Write call.
func WriteTCP(w io.Writer, msg []byte) error {
	b, err := AppendTCP(make([]byte, 0, len(msg)+2), msg)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadTCP reads one length-prefixed message. A prefix above max fails with
// CodeMessageTooLarge before anything else is read.
func ReadTCP(r io.Reader, max int) ([]byte, error) {
	var p [2]byte
	if _, err := io.ReadFull(r, p[:]); err != nil {
		return nil, err
	}
	n := int(binary.BigEndian.Uint16(p[:]))
	if n > max {
		return nil, fail(CodeMessageTooLarge, 0)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return b, nil
}
