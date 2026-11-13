package dnsmsg

import (
	"bytes"
	"net/netip"
	"slices"
)

// Type is an RR TYPE or QTYPE (RFC 1035 §3.2.2, §3.2.3).
type Type uint16

// Record types with structured data, plus ANY.
const (
	TypeA     Type = 1
	TypeNS    Type = 2
	TypeCNAME Type = 5
	TypeSOA   Type = 6
	TypePTR   Type = 12
	TypeMX    Type = 15
	TypeTXT   Type = 16
	TypeAAAA  Type = 28
	TypeSRV   Type = 33
	TypeOPT   Type = 41
	TypeANY   Type = 255
	TypeCAA   Type = 257
)

// Class is an RR CLASS (RFC 1035 §3.2.4). For OPT it is the requestor's
// UDP payload size (RFC 6891 §6.1.2).
type Class uint16

// Classes.
const (
	ClassINET  Class = 1
	ClassCHAOS Class = 3
	ClassANY   Class = 255
)

// Opcode is the header OPCODE (4 bits).
type Opcode uint8

// OpcodeQuery is a standard query.
const OpcodeQuery Opcode = 0

// RCode is the header RCODE (4 bits; RFC 1035 §4.1.1).
type RCode uint8

// Response codes.
const (
	RCodeSuccess  RCode = 0
	RCodeFormErr  RCode = 1
	RCodeServFail RCode = 2
	RCodeNXDomain RCode = 3
	RCodeNotImp   RCode = 4
	RCodeRefused  RCode = 5
)

// Header is the 12-octet message header (RFC 1035 §4.1.1) without the
// section counts, which the encoder derives from the sections.
type Header struct {
	ID                 uint16
	Response           bool // QR
	Opcode             Opcode
	Authoritative      bool // AA
	Truncated          bool // TC
	RecursionDesired   bool // RD
	RecursionAvailable bool // RA
	Zero               bool // Z, reserved; kept so messages round-trip
	AuthenticData      bool // AD (RFC 4035)
	CheckingDisabled   bool // CD (RFC 4035)
	RCode              RCode
}

// Question is one entry of the question section (RFC 1035 §4.1.2).
type Question struct {
	Name  string // canonical presentation form
	Type  Type
	Class Class
}

// RR is a resource record (RFC 1035 §4.1.3).
type RR struct {
	Name  string // canonical presentation form
	Type  Type
	Class Class
	TTL   uint32
	Data  RData
}

// RDataKind says which RData fields are meaningful.
type RDataKind uint8

// RData kinds. Each known Type has exactly one kind; any type may carry
// KindRaw (unknown types always do, RFC 3597).
const (
	KindRaw  RDataKind = iota // Raw
	KindA                     // IP (IPv4)
	KindAAAA                  // IP (IPv6)
	KindName                  // Target (NS, CNAME, PTR)
	KindMX                    // Preference, Target
	KindTXT                   // Strings
	KindSOA                   // SOA
	KindSRV                   // Priority, Weight, Port, Target
	KindCAA                   // CAAFlags, CAATag, CAAValue
	KindOPT                   // Options
)

// RData is the data of a record as a tagged struct: Kind names the fields
// that are set; the others are zero.
type RData struct {
	Kind       RDataKind
	IP         netip.Addr
	Target     string // canonical presentation form
	Preference uint16
	Priority   uint16
	Weight     uint16
	Port       uint16
	Strings    []string // TXT character-strings, raw octets
	SOA        SOA
	CAAFlags   uint8
	CAATag     string
	CAAValue   string
	Options    []Option
	Raw        []byte
}

// SOA is start-of-authority data (RFC 1035 §3.3.13).
type SOA struct {
	MName, RName                             string
	Serial, Refresh, Retry, Expire, Minimum uint32
}

// Option is one EDNS(0) option (RFC 6891 §6.1.2).
type Option struct {
	Code uint16
	Data []byte
}

// Message is a DNS message.
type Message struct {
	Header     Header
	Questions  []Question
	Answers    []RR
	Authority  []RR
	Additional []RR
}

// KindFor returns the structured kind of t, or KindRaw for types without
// structured data.
func KindFor(t Type) RDataKind {
	switch t {
	case TypeA:
		return KindA
	case TypeAAAA:
		return KindAAAA
	case TypeNS, TypeCNAME, TypePTR:
		return KindName
	case TypeMX:
		return KindMX
	case TypeTXT:
		return KindTXT
	case TypeSOA:
		return KindSOA
	case TypeSRV:
		return KindSRV
	case TypeCAA:
		return KindCAA
	case TypeOPT:
		return KindOPT
	}
	return KindRaw
}

// Equal reports whether m and o are the same message. Nil and empty
// slices are equal.
func (m Message) Equal(o Message) bool {
	if m.Header != o.Header || !slices.Equal(m.Questions, o.Questions) {
		return false
	}
	return rrsEqual(m.Answers, o.Answers) && rrsEqual(m.Authority, o.Authority) && rrsEqual(m.Additional, o.Additional)
}

func rrsEqual(a, b []RR) bool {
	return slices.EqualFunc(a, b, func(x, y RR) bool { return x.Equal(y) })
}

// Equal reports whether r and o are the same record.
func (r RR) Equal(o RR) bool {
	return r.Name == o.Name && r.Type == o.Type && r.Class == o.Class && r.TTL == o.TTL && r.Data.Equal(o.Data)
}

// Equal reports whether d and o are the same data (all fields compared;
// nil and empty slices are equal).
func (d RData) Equal(o RData) bool {
	return d.Kind == o.Kind && d.IP == o.IP && d.Target == o.Target && d.Preference == o.Preference &&
		d.Priority == o.Priority && d.Weight == o.Weight && d.Port == o.Port && slices.Equal(d.Strings, o.Strings) &&
		d.SOA == o.SOA && d.CAAFlags == o.CAAFlags && d.CAATag == o.CAATag && d.CAAValue == o.CAAValue &&
		slices.EqualFunc(d.Options, o.Options, func(x, y Option) bool { return x.Code == y.Code && bytes.Equal(x.Data, y.Data) }) &&
		bytes.Equal(d.Raw, o.Raw)
}

// EDNS is the interpreted content of an OPT record (RFC 6891 §6.1.3).
type EDNS struct {
	UDPSize       uint16
	ExtendedRCode uint8 // upper 8 bits of the 12-bit RCODE
	Version       uint8
	DNSSECOK      bool // DO
	Options       []Option
}

// EDNS returns the first OPT record of the additional section.
func (m Message) EDNS() (EDNS, bool) {
	for _, rr := range m.Additional {
		if rr.Type == TypeOPT && rr.Data.Kind == KindOPT {
			return EDNS{UDPSize: uint16(rr.Class), ExtendedRCode: uint8(rr.TTL >> 24), Version: uint8(rr.TTL >> 16),
				DNSSECOK: rr.TTL&0x8000 != 0, Options: rr.Data.Options}, true
		}
	}
	return EDNS{}, false
}

// OPT returns the OPT record carrying e.
func (e EDNS) OPT() RR {
	ttl := uint32(e.ExtendedRCode)<<24 | uint32(e.Version)<<16
	if e.DNSSECOK {
		ttl |= 0x8000
	}
	return RR{Name: ".", Type: TypeOPT, Class: Class(e.UDPSize), TTL: ttl, Data: RData{Kind: KindOPT, Options: e.Options}}
}

// SetEDNS replaces every OPT record of the additional section with one
// carrying e (appended last).
func (m *Message) SetEDNS(e EDNS) {
	m.Additional = slices.DeleteFunc(m.Additional, func(rr RR) bool { return rr.Type == TypeOPT })
	m.Additional = append(m.Additional, e.OPT())
}

// Option returns the data of the first option with code.
func (e EDNS) Option(code uint16) ([]byte, bool) {
	for _, o := range e.Options {
		if o.Code == code {
			return o.Data, true
		}
	}
	return nil, false
}

// NewQuery returns a recursive query for (name, t) in class IN. name is
// given in presentation form; it is not validated until encoding.
func NewQuery(id uint16, name string, t Type) Message {
	return Message{Header: Header{ID: id, RecursionDesired: true}, Questions: []Question{{Name: name, Type: t, Class: ClassINET}}}
}
