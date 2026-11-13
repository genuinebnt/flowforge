package dnsmsg

import (
	"encoding/binary"
	"net/netip"
)

// Mode selects strict or lenient decoding.
type Mode uint8

// Decoding modes.
const (
	// Strict rejects trailing octets, malformed data of a known type, OPT
	// records that break RFC 6891 §6.1.1 and TXT records without strings.
	Strict Mode = iota
	// Lenient keeps such records as KindRaw (or ignores trailing octets)
	// and reports each problem as a Diagnostic. Structural errors (short
	// input, bad names) fail in both modes.
	Lenient
)

// HeaderLen is the length of the fixed header.
const HeaderLen = 12

// minRRLen is the smallest possible resource record: root owner, type,
// class, TTL and RDLENGTH.
const minRRLen = 11

// section names the part of the message the decoder is reading.
type section uint8

const (
	sectionHeader section = iota
	sectionQuestion
	sectionAnswer
	sectionAuthority
	sectionAdditional
	sectionDone
)

type decoder struct {
	msg   []byte
	mode  Mode
	diags []Diagnostic
	opts  int // OPT records seen
}

func (d *decoder) diag(c Code, off int) { d.diags = append(d.diags, Diagnostic{Code: c, Offset: off}) }

// Decode parses one DNS message. It never panics and runs in time linear
// in len(b). In Lenient mode the diagnostics list what was recovered from.
func Decode(b []byte, mode Mode) (Message, []Diagnostic, error) {
	d := &decoder{msg: b, mode: mode}
	var (
		m      Message
		counts [4]int
		pos    int
		sec    = sectionHeader
	)
	for sec != sectionDone {
		switch sec {
		case sectionHeader:
			if len(b) < HeaderLen {
				return Message{}, nil, fail(CodeShortMessage, len(b))
			}
			m.Header = decodeHeader(b)
			for i := range counts {
				counts[i] = int(binary.BigEndian.Uint16(b[4+2*i:]))
			}
			pos = HeaderLen
			sec = sectionQuestion
		case sectionQuestion:
			// Pre-size by what the input can hold, never by the claimed count.
			m.Questions = make([]Question, 0, min(counts[0], (len(b)-pos)/5))
			for range counts[0] {
				q, next, err := d.question(pos)
				if err != nil {
					return Message{}, nil, err
				}
				m.Questions = append(m.Questions, q)
				pos = next
			}
			sec = sectionAnswer
		case sectionAnswer, sectionAuthority, sectionAdditional:
			n := counts[int(sec-sectionQuestion)]
			rrs := make([]RR, 0, min(n, (len(b)-pos)/minRRLen))
			for range n {
				rr, next, err := d.rr(pos, sec)
				if err != nil {
					return Message{}, nil, err
				}
				rrs = append(rrs, rr)
				pos = next
			}
			switch sec {
			case sectionAnswer:
				m.Answers = rrs
			case sectionAuthority:
				m.Authority = rrs
			default:
				m.Additional = rrs
			}
			sec++
			if sec == sectionAdditional+1 {
				sec = sectionDone
			}
		}
	}
	if pos != len(b) {
		if d.mode == Strict {
			return Message{}, nil, fail(CodeTrailingData, pos)
		}
		d.diag(CodeTrailingData, pos)
	}
	return m, d.diags, nil
}

func decodeHeader(b []byte) Header {
	f := binary.BigEndian.Uint16(b[2:])
	return Header{
		ID:                 binary.BigEndian.Uint16(b),
		Response:           f&(1<<15) != 0,
		Opcode:             Opcode(f >> 11 & 0xF),
		Authoritative:      f&(1<<10) != 0,
		Truncated:          f&(1<<9) != 0,
		RecursionDesired:   f&(1<<8) != 0,
		RecursionAvailable: f&(1<<7) != 0,
		Zero:               f&(1<<6) != 0,
		AuthenticData:      f&(1<<5) != 0,
		CheckingDisabled:   f&(1<<4) != 0,
		RCode:              RCode(f & 0xF),
	}
}

// DecodeHeader parses only the fixed header (for example to answer FORMERR
// with the right ID when the rest does not parse).
func DecodeHeader(b []byte) (Header, bool) {
	if len(b) < HeaderLen {
		return Header{}, false
	}
	return decodeHeader(b), true
}

func (d *decoder) question(pos int) (Question, int, error) {
	name, pos, err := readName(d.msg, pos, len(d.msg))
	if err != nil {
		return Question{}, 0, err
	}
	if pos+4 > len(d.msg) {
		return Question{}, 0, fail(CodeShortMessage, len(d.msg))
	}
	return Question{Name: name, Type: Type(binary.BigEndian.Uint16(d.msg[pos:])), Class: Class(binary.BigEndian.Uint16(d.msg[pos+2:]))}, pos + 4, nil
}

func (d *decoder) rr(pos int, sec section) (RR, int, error) {
	start := pos
	name, pos, err := readName(d.msg, pos, len(d.msg))
	if err != nil {
		return RR{}, 0, err
	}
	if pos+10 > len(d.msg) {
		return RR{}, 0, fail(CodeShortMessage, len(d.msg))
	}
	rr := RR{Name: name, Type: Type(binary.BigEndian.Uint16(d.msg[pos:])), Class: Class(binary.BigEndian.Uint16(d.msg[pos+2:])),
		TTL: binary.BigEndian.Uint32(d.msg[pos+4:])}
	rdlen := int(binary.BigEndian.Uint16(d.msg[pos+8:]))
	pos += 10
	end := pos + rdlen
	if end > len(d.msg) {
		return RR{}, 0, fail(CodeShortMessage, len(d.msg))
	}
	if rr.Type == TypeOPT {
		d.opts++
		if sec != sectionAdditional || name != "." || d.opts > 1 {
			if d.mode == Strict {
				return RR{}, 0, fail(CodeBadOPT, start)
			}
			d.diag(CodeBadOPT, start)
		}
	}
	data, err := d.rdata(rr.Type, pos, end)
	if err != nil {
		if d.mode == Strict {
			return RR{}, 0, err
		}
		d.diag(CodeOf(err), pos)
		data = RData{Kind: KindRaw, Raw: append([]byte(nil), d.msg[pos:end]...)}
	}
	rr.Data = data
	return rr, end, nil
}

// rdata decodes msg[pos:end] as the data of type t. Names inside may use
// compression pointers to earlier octets of the message.
func (d *decoder) rdata(t Type, pos, end int) (RData, error) {
	msg := d.msg
	raw := msg[pos:end]
	u16 := func(o int) uint16 { return binary.BigEndian.Uint16(msg[o:]) }
	switch KindFor(t) {
	case KindA:
		if len(raw) != 4 {
			return RData{}, fail(CodeBadRData, pos)
		}
		return RData{Kind: KindA, IP: netip.AddrFrom4([4]byte(raw))}, nil
	case KindAAAA:
		if len(raw) != 16 {
			return RData{}, fail(CodeBadRData, pos)
		}
		return RData{Kind: KindAAAA, IP: netip.AddrFrom16([16]byte(raw))}, nil
	case KindName:
		n, next, err := readName(msg, pos, end)
		if err != nil {
			return RData{}, rdataErr(err)
		}
		if next != end {
			return RData{}, fail(CodeRDLengthMismatch, next)
		}
		return RData{Kind: KindName, Target: n}, nil
	case KindMX:
		if len(raw) < 3 {
			return RData{}, fail(CodeBadRData, pos)
		}
		n, next, err := readName(msg, pos+2, end)
		if err != nil {
			return RData{}, rdataErr(err)
		}
		if next != end {
			return RData{}, fail(CodeRDLengthMismatch, next)
		}
		return RData{Kind: KindMX, Preference: u16(pos), Target: n}, nil
	case KindSRV:
		if len(raw) < 7 {
			return RData{}, fail(CodeBadRData, pos)
		}
		n, next, err := readName(msg, pos+6, end)
		if err != nil {
			return RData{}, rdataErr(err)
		}
		if next != end {
			return RData{}, fail(CodeRDLengthMismatch, next)
		}
		return RData{Kind: KindSRV, Priority: u16(pos), Weight: u16(pos + 2), Port: u16(pos + 4), Target: n}, nil
	case KindSOA:
		mn, next, err := readName(msg, pos, end)
		if err != nil {
			return RData{}, rdataErr(err)
		}
		rn, next, err := readName(msg, next, end)
		if err != nil {
			return RData{}, rdataErr(err)
		}
		if next+20 != end {
			return RData{}, fail(CodeRDLengthMismatch, next)
		}
		u32 := func(i int) uint32 { return binary.BigEndian.Uint32(msg[next+4*i:]) }
		return RData{Kind: KindSOA, SOA: SOA{MName: mn, RName: rn, Serial: u32(0), Refresh: u32(1), Retry: u32(2), Expire: u32(3), Minimum: u32(4)}}, nil
	case KindTXT:
		var ss []string
		for i := 0; i < len(raw); {
			l := int(raw[i])
			if i+1+l > len(raw) {
				return RData{}, fail(CodeBadRData, pos+i)
			}
			ss = append(ss, string(raw[i+1:i+1+l]))
			i += 1 + l
		}
		if len(ss) == 0 && d.mode == Strict {
			return RData{}, fail(CodeBadRData, pos)
		}
		return RData{Kind: KindTXT, Strings: ss}, nil
	case KindCAA:
		if len(raw) < 2 {
			return RData{}, fail(CodeBadRData, pos)
		}
		tl := int(raw[1])
		if tl == 0 || 2+tl > len(raw) {
			return RData{}, fail(CodeBadRData, pos)
		}
		tag := raw[2 : 2+tl]
		for _, c := range tag {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
				return RData{}, fail(CodeBadRData, pos+2)
			}
		}
		return RData{Kind: KindCAA, CAAFlags: raw[0], CAATag: string(tag), CAAValue: string(raw[2+tl:])}, nil
	case KindOPT:
		var opts []Option
		for i := 0; i < len(raw); {
			if i+4 > len(raw) {
				return RData{}, fail(CodeBadRData, pos+i)
			}
			code, l := binary.BigEndian.Uint16(raw[i:]), int(binary.BigEndian.Uint16(raw[i+2:]))
			if i+4+l > len(raw) {
				return RData{}, fail(CodeBadRData, pos+i)
			}
			opts = append(opts, Option{Code: code, Data: append([]byte(nil), raw[i+4:i+4+l]...)})
			i += 4 + l
		}
		return RData{Kind: KindOPT, Options: opts}, nil
	}
	return RData{Kind: KindRaw, Raw: append([]byte(nil), raw...)}, nil
}

// rdataErr maps a name error inside record data: running past the data
// window is malformed data, everything else keeps its code.
func rdataErr(err error) error {
	if e, ok := err.(*Error); ok && e.Code == CodeShortMessage {
		return &Error{Code: CodeBadRData, Offset: e.Offset}
	}
	return err
}
