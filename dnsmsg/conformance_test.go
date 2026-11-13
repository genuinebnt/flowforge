package dnsmsg

import (
	"encoding/hex"
	"fmt"
	"net/netip"
	"testing"

	"github.com/genuinebnt/flowforge/testkit"
)

// The language-neutral JSON form of a message used by conformance/dnsmsg.
type (
	jsonHeader struct {
		ID     uint16 `json:"id"`
		QR     bool   `json:"qr"`
		Opcode uint8  `json:"opcode"`
		AA     bool   `json:"aa"`
		TC     bool   `json:"tc"`
		RD     bool   `json:"rd"`
		RA     bool   `json:"ra"`
		Z      bool   `json:"z"`
		AD     bool   `json:"ad"`
		CD     bool   `json:"cd"`
		RCode  uint8  `json:"rcode"`
	}
	jsonQuestion struct {
		Name  string `json:"name"`
		Type  uint16 `json:"type"`
		Class uint16 `json:"class"`
	}
	jsonOption struct {
		Code    uint16 `json:"code"`
		DataHex string `json:"data_hex"`
	}
	jsonData struct {
		Kind       string       `json:"kind"`
		IP         string       `json:"ip"`
		Target     string       `json:"target"`
		Preference uint16       `json:"preference"`
		Priority   uint16       `json:"priority"`
		Weight     uint16       `json:"weight"`
		Port       uint16       `json:"port"`
		StringsHex []string     `json:"strings_hex"`
		MName      string       `json:"mname"`
		RName      string       `json:"rname"`
		Serial     uint32       `json:"serial"`
		Refresh    uint32       `json:"refresh"`
		Retry      uint32       `json:"retry"`
		Expire     uint32       `json:"expire"`
		Minimum    uint32       `json:"minimum"`
		Flags      uint8        `json:"flags"`
		Tag        string       `json:"tag"`
		ValueHex   string       `json:"value_hex"`
		Options    []jsonOption `json:"options"`
		RawHex     string       `json:"raw_hex"`
	}
	jsonRR struct {
		Name  string   `json:"name"`
		Type  uint16   `json:"type"`
		Class uint16   `json:"class"`
		TTL   uint32   `json:"ttl"`
		Data  jsonData `json:"data"`
	}
	jsonMessage struct {
		Header     jsonHeader     `json:"header"`
		Questions  []jsonQuestion `json:"questions"`
		Answers    []jsonRR       `json:"answers"`
		Authority  []jsonRR       `json:"authority"`
		Additional []jsonRR       `json:"additional"`
	}
	roundTripFile struct {
		Cases []struct {
			ID      string      `json:"id"`
			Hex     string      `json:"hex"`
			Message jsonMessage `json:"message"`
		} `json:"cases"`
	}
	decodeFile struct {
		Cases []struct {
			ID          string       `json:"id"`
			Hex         string       `json:"hex"`
			Mode        string       `json:"mode"`
			Error       string       `json:"error"`
			Message     *jsonMessage `json:"message"`
			Diagnostics []string     `json:"diagnostics"`
		} `json:"cases"`
	}
)

func unhex(t testing.TB, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

func (j jsonMessage) message(t testing.TB) Message {
	t.Helper()
	h := j.Header
	m := Message{Header: Header{ID: h.ID, Response: h.QR, Opcode: Opcode(h.Opcode), Authoritative: h.AA, Truncated: h.TC,
		RecursionDesired: h.RD, RecursionAvailable: h.RA, Zero: h.Z, AuthenticData: h.AD, CheckingDisabled: h.CD, RCode: RCode(h.RCode)}}
	for _, q := range j.Questions {
		m.Questions = append(m.Questions, Question{Name: q.Name, Type: Type(q.Type), Class: Class(q.Class)})
	}
	conv := func(rs []jsonRR) []RR {
		var out []RR
		for _, r := range rs {
			out = append(out, RR{Name: r.Name, Type: Type(r.Type), Class: Class(r.Class), TTL: r.TTL, Data: r.Data.rdata(t)})
		}
		return out
	}
	m.Answers, m.Authority, m.Additional = conv(j.Answers), conv(j.Authority), conv(j.Additional)
	return m
}

func (d jsonData) rdata(t testing.TB) RData {
	t.Helper()
	ip := func() netip.Addr {
		a, err := netip.ParseAddr(d.IP)
		if err != nil {
			t.Fatalf("bad ip %q", d.IP)
		}
		return a
	}
	switch d.Kind {
	case "a":
		return RData{Kind: KindA, IP: ip()}
	case "aaaa":
		return RData{Kind: KindAAAA, IP: ip()}
	case "name":
		return RData{Kind: KindName, Target: d.Target}
	case "mx":
		return RData{Kind: KindMX, Preference: d.Preference, Target: d.Target}
	case "srv":
		return RData{Kind: KindSRV, Priority: d.Priority, Weight: d.Weight, Port: d.Port, Target: d.Target}
	case "soa":
		return RData{Kind: KindSOA, SOA: SOA{MName: d.MName, RName: d.RName, Serial: d.Serial, Refresh: d.Refresh, Retry: d.Retry, Expire: d.Expire, Minimum: d.Minimum}}
	case "txt":
		var ss []string
		for _, h := range d.StringsHex {
			ss = append(ss, string(unhex(t, h)))
		}
		return RData{Kind: KindTXT, Strings: ss}
	case "caa":
		return RData{Kind: KindCAA, CAAFlags: d.Flags, CAATag: d.Tag, CAAValue: string(unhex(t, d.ValueHex))}
	case "opt":
		var os []Option
		for _, o := range d.Options {
			os = append(os, Option{Code: o.Code, Data: unhex(t, o.DataHex)})
		}
		return RData{Kind: KindOPT, Options: os}
	case "raw":
		return RData{Kind: KindRaw, Raw: unhex(t, d.RawHex)}
	}
	t.Fatalf("unknown data kind %q", d.Kind)
	return RData{}
}

// TestDNSConformance_RoundTrip: every vector decodes (strictly) to its
// message and its message encodes to exactly the vector's bytes.
func TestDNSConformance_RoundTrip(t *testing.T) {
	for _, f := range testkit.LoadCases[roundTripFile](t, "dnsmsg/roundtrip") {
		for _, c := range f.Data.Cases {
			t.Run(c.ID, func(t *testing.T) {
				b := unhex(t, c.Hex)
				want := c.Message.message(t)
				got, diags, err := Decode(b, Strict)
				if err != nil || len(diags) != 0 {
					t.Fatalf("decode: %v %v", err, diags)
				}
				if !got.Equal(want) {
					t.Fatalf("decode:\n got %+v\nwant %+v", got, want)
				}
				enc, err := Encode(want)
				if err != nil {
					t.Fatal(err)
				}
				if hex.EncodeToString(enc) != c.Hex {
					t.Fatalf("encode:\n got %x\nwant %s", enc, c.Hex)
				}
			})
		}
	}
}

// TestDNSConformance_Decode: malformed and edge-case vectors give their
// error code, or their message and lenient diagnostics.
func TestDNSConformance_Decode(t *testing.T) {
	n := 0
	for _, f := range testkit.LoadCases[decodeFile](t, "dnsmsg/decode") {
		for _, c := range f.Data.Cases {
			if c.Mode == "" {
				continue // a round-trip file
			}
			n++
			t.Run(c.ID, func(t *testing.T) {
				mode := Strict
				if c.Mode == "lenient" {
					mode = Lenient
				}
				got, diags, err := Decode(unhex(t, c.Hex), mode)
				if c.Error != "" {
					if CodeOf(err) != Code(c.Error) {
						t.Fatalf("error %v, want %s", err, c.Error)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if want := c.Message.message(t); !got.Equal(want) {
					t.Fatalf("decode:\n got %+v\nwant %+v", got, want)
				}
				var codes []string
				for _, d := range diags {
					codes = append(codes, string(d.Code))
				}
				if fmt.Sprint(codes) != fmt.Sprint(c.Diagnostics) && !(len(codes) == 0 && len(c.Diagnostics) == 0) {
					t.Fatalf("diagnostics %v, want %v", codes, c.Diagnostics)
				}
			})
		}
	}
	if n == 0 {
		t.Fatal("no decode vectors")
	}
}
