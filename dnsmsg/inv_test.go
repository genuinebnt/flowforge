package dnsmsg

import (
	"bytes"
	"encoding/hex"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// gen draws canonical, strictly valid messages: names from a small pool of
// suffixes (so compression is exercised), labels with escapes, every
// structured kind, unknown types, and at most one OPT in the additional
// section.
type gen struct {
	r    core.Rng
	pool []string
}

func newGen(r core.Rng) *gen {
	return &gen{r: r, pool: []string{".", "com.", "example.com.", "Example.COM.", "a\\.b.example.org.", "in-addr.arpa."}}
}

func (g *gen) n(k int) int { return int(g.r.Uint64() % uint64(k)) }

func (g *gen) label() string {
	const alpha = "abcxyzABC019-_*"
	l := 1 + g.n(12)
	if g.n(20) == 0 {
		l = 63
	}
	var b strings.Builder
	raw := make([]byte, l)
	for i := range raw {
		switch g.n(10) {
		case 0:
			raw[i] = byte(g.n(256))
		case 1:
			raw[i] = '.'
		default:
			raw[i] = alpha[g.n(len(alpha))]
		}
	}
	appendLabelText(&b, raw)
	return b.String()
}

func (g *gen) name() string {
	for {
		base := g.pool[g.n(len(g.pool))]
		var s string
		switch g.n(3) {
		case 0:
			s = base
		default:
			s = g.label() + "." + base
			if base == "." {
				s = g.label() + "."
			}
		}
		if w, err := appendNameWire(nil, s); err == nil && len(w) <= MaxNameLen {
			if g.n(4) == 0 {
				g.pool = append(g.pool, s)
			}
			return s
		}
	}
}

func (g *gen) bytes(max int) []byte {
	b := make([]byte, g.n(max+1))
	for i := range b {
		b[i] = byte(g.r.Uint64())
	}
	return b
}

func (g *gen) rr() RR {
	types := []Type{TypeA, TypeAAAA, TypeNS, TypeCNAME, TypePTR, TypeMX, TypeTXT, TypeSOA, TypeSRV, TypeCAA, 99, 65280}
	t := types[g.n(len(types))]
	r := RR{Name: g.name(), Type: t, Class: Class(1 + g.n(3)), TTL: uint32(g.r.Uint64())}
	switch KindFor(t) {
	case KindA:
		r.Data = RData{Kind: KindA, IP: netip.AddrFrom4([4]byte(g.bytes4()))}
	case KindAAAA:
		var v [16]byte
		copy(v[:], append(g.bytes4(), append(g.bytes4(), append(g.bytes4(), g.bytes4()...)...)...))
		r.Data = RData{Kind: KindAAAA, IP: netip.AddrFrom16(v)}
	case KindName:
		r.Data = RData{Kind: KindName, Target: g.name()}
	case KindMX:
		r.Data = RData{Kind: KindMX, Preference: uint16(g.n(65536)), Target: g.name()}
	case KindSRV:
		r.Data = RData{Kind: KindSRV, Priority: uint16(g.n(65536)), Weight: uint16(g.n(65536)), Port: uint16(g.n(65536)), Target: g.name()}
	case KindSOA:
		r.Data = RData{Kind: KindSOA, SOA: SOA{MName: g.name(), RName: g.name(), Serial: uint32(g.r.Uint64()), Refresh: 1, Retry: 2, Expire: 3, Minimum: uint32(g.n(100))}}
	case KindTXT:
		ss := make([]string, 1+g.n(3))
		for i := range ss {
			ss[i] = string(g.bytes(40))
		}
		r.Data = RData{Kind: KindTXT, Strings: ss}
	case KindCAA:
		r.Data = RData{Kind: KindCAA, CAAFlags: byte(g.n(256)), CAATag: []string{"issue", "issuewild", "iodef", "X1"}[g.n(4)], CAAValue: string(g.bytes(20))}
	default:
		r.Data = RData{Kind: KindRaw, Raw: g.bytes(30)}
	}
	return r
}

func (g *gen) bytes4() []byte {
	return []byte{byte(g.r.Uint64()), byte(g.r.Uint64()), byte(g.r.Uint64()), byte(g.r.Uint64())}
}

func (g *gen) message() Message {
	m := Message{Header: Header{ID: uint16(g.r.Uint64()), Response: g.n(2) == 0, Opcode: Opcode(g.n(16)), Authoritative: g.n(2) == 0,
		Truncated: g.n(4) == 0, RecursionDesired: g.n(2) == 0, RecursionAvailable: g.n(2) == 0, Zero: g.n(8) == 0,
		AuthenticData: g.n(2) == 0, CheckingDisabled: g.n(2) == 0, RCode: RCode(g.n(16))}}
	for range g.n(3) {
		m.Questions = append(m.Questions, Question{Name: g.name(), Type: Type(g.n(300)), Class: Class(g.n(5))})
	}
	sec := func() []RR {
		var rs []RR
		for range g.n(5) {
			rs = append(rs, g.rr())
		}
		return rs
	}
	m.Answers, m.Authority, m.Additional = sec(), sec(), sec()
	if g.n(2) == 0 {
		var opts []Option
		for range g.n(3) {
			opts = append(opts, Option{Code: uint16(g.n(65536)), Data: g.bytes(12)})
		}
		m.SetEDNS(EDNS{UDPSize: uint16(512 + g.n(4096)), Version: uint8(g.n(2)), DNSSECOK: g.n(2) == 0, Options: opts})
	}
	return m
}

// closedCodes is the complete list of codes (README).
var closedCodes = []Code{CodeShortMessage, CodeLabelTooLong, CodeNameTooLong, CodeBadLabelType, CodePointerOutOfBounds,
	CodePointerLoop, CodeBadRData, CodeRDLengthMismatch, CodeTrailingData, CodeBadOPT, CodeBadNameText, CodeStringTooLong,
	CodeKindMismatch, CodeMessageTooLarge, CodeTooManyRecords}

// names returns every name a message carries.
func names(m Message) []string {
	var out []string
	for _, q := range m.Questions {
		out = append(out, q.Name)
	}
	for _, s := range [][]RR{m.Answers, m.Authority, m.Additional} {
		for _, r := range s {
			out = append(out, r.Name)
			switch r.Data.Kind {
			case KindName, KindMX, KindSRV:
				out = append(out, r.Data.Target)
			case KindSOA:
				out = append(out, r.Data.SOA.MName, r.Data.SOA.RName)
			}
		}
	}
	return out
}

// checkDecoded asserts the invariants every decode result must satisfy
// (used by the property tests and the fuzz target).
func checkDecoded(t testing.TB, b []byte) {
	t.Helper()
	for _, mode := range []Mode{Strict, Lenient} {
		m, diags, err := Decode(b, mode)
		if err != nil {
			var e *Error
			if !errors.As(err, &e) || !slices.Contains(closedCodes, e.Code) {
				t.Fatalf("DNS-INV-04: error outside the closed list: %v", err)
			}
			continue
		}
		for _, d := range diags {
			if !slices.Contains(closedCodes, d.Code) {
				t.Fatalf("DNS-INV-04: diagnostic outside the closed list: %v", d)
			}
		}
		// DNS-INV-03: names are within the RFC 1035 limits.
		for _, n := range names(m) {
			w, err := appendNameWire(nil, n)
			if err != nil || len(w) > MaxNameLen {
				t.Fatalf("DNS-INV-03: decoded name %q (%v, %d octets)", n, err, len(w))
			}
			for i := 0; w[i] != 0; i += 1 + int(w[i]) {
				if w[i] > MaxLabelLen {
					t.Fatalf("DNS-INV-03: label over 63 octets in %q", n)
				}
			}
			if c, _ := CanonicalName(n); c != n {
				t.Fatalf("decoded name %q is not canonical (%q)", n, c)
			}
		}
		if mode != Strict {
			continue
		}
		// DNS-INV-09: strict success implies the same lenient result.
		lm, ldiags, lerr := Decode(b, Lenient)
		if lerr != nil || len(ldiags) != 0 || !lm.Equal(m) {
			t.Fatalf("DNS-INV-09: lenient differs: %v %v", lerr, ldiags)
		}
		// DNS-INV-06: re-encoding a strict decode decodes to the same message.
		enc, err := Encode(m)
		if err != nil {
			t.Fatalf("DNS-INV-06: re-encode: %v", err)
		}
		m2, _, err := Decode(enc, Strict)
		if err != nil || !m2.Equal(m) {
			t.Fatalf("DNS-INV-06: idempotence: %v\n%+v\n%+v", err, m, m2)
		}
		// DNS-INV-05: encoding is canonical.
		enc2, _ := Encode(m2)
		if !bytes.Equal(enc, enc2) {
			t.Fatalf("DNS-INV-05: encode not canonical")
		}
	}
}

// TestDNSInv01_RoundTrip: decode(encode(m)) == m for generated messages,
// in both modes, with no diagnostics.
func TestDNSInv01_RoundTrip(t *testing.T) {
	g := newGen(testkit.Rng(t))
	for range testkit.Cases() {
		m := g.message()
		b, err := Encode(m)
		if err != nil {
			t.Fatalf("encode %+v: %v", m, err)
		}
		for _, mode := range []Mode{Strict, Lenient} {
			got, diags, err := Decode(b, mode)
			if err != nil || len(diags) != 0 || !got.Equal(m) {
				t.Fatalf("mode %d: %v %v\n got %+v\nwant %+v\n%x", mode, err, diags, got, m, b)
			}
		}
	}
}

// TestDNSInv02_PointersTerminate: crafted self, forward and backward
// cyclic pointers, and chains of pointers, end in pointer_loop; random
// pointer-dense inputs always terminate.
func TestDNSInv02_PointersTerminate(t *testing.T) {
	hdr := []byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0}
	cases := map[string][]byte{
		"self":     append(slices.Clone(hdr), 0xC0, 12, 0, 1, 0, 1),
		"forward":  append(slices.Clone(hdr), 0xC0, 14, 0xC0, 12, 0, 1, 0, 1),
		"backward": append(slices.Clone(hdr), 1, 'a', 0xC0, 12, 0, 1, 0, 1),
	}
	for name, b := range cases {
		if _, _, err := Decode(b, Lenient); CodeOf(err) != CodePointerLoop {
			t.Errorf("%s: %v, want pointer_loop", name, err)
		}
	}
	// A chain of 200 backward pointers exceeds MaxPointerHops.
	chain := slices.Clone(hdr)
	chain[5] = 0
	chain = append(chain, 0) // offset 12: root
	for i := range 200 {
		target := 12 + 2*i - 1
		if i == 0 {
			target = 12
		}
		chain = append(chain, 0xC0|byte(target>>8), byte(target))
	}
	if _, _, err := readName(chain, len(chain)-2, len(chain)); CodePointerLoop != CodeOf(err) {
		t.Errorf("long chain: %v, want pointer_loop", err)
	}
	r := testkit.Rng(t)
	for range testkit.Cases() {
		b := make([]byte, 12+int(r.Uint64()%200))
		for i := range b {
			if r.Uint64()%3 == 0 {
				b[i] = 0xC0 | byte(r.Uint64()%2)
			} else {
				b[i] = byte(r.Uint64())
			}
		}
		b[5] = byte(1 + r.Uint64()%4)
		checkDecoded(t, b)
	}
}

// TestDNSInv03_NameBounds: decoded names never exceed 255 octets or labels
// 63, at the exact limits and on random input; encoding refuses longer
// names.
func TestDNSInv03_NameBounds(t *testing.T) {
	l63 := strings.Repeat("a", 63)
	ok := strings.Join([]string{l63, l63, l63, strings.Repeat("a", 61)}, ".") + "."
	if _, err := CanonicalName(ok); err != nil {
		t.Fatalf("255-octet name rejected: %v", err)
	}
	if _, err := CanonicalName(strings.Join([]string{l63, l63, l63, strings.Repeat("a", 62)}, ".") + "."); CodeOf(err) != CodeNameTooLong {
		t.Fatalf("256-octet name: %v", err)
	}
	if _, err := CanonicalName(strings.Repeat("a", 64) + ".com."); CodeOf(err) != CodeLabelTooLong {
		t.Fatalf("64-octet label: %v", err)
	}
	if _, err := Encode(NewQuery(1, strings.Repeat("a", 64)+".com.", TypeA)); CodeOf(err) != CodeLabelTooLong {
		t.Fatalf("encode 64-octet label: %v", err)
	}
	for _, bad := range []string{"", "a..b.", ".a.", "a\\", "a\\25", "a\\256.", "a\\2x5."} {
		if _, err := CanonicalName(bad); err == nil {
			t.Errorf("CanonicalName(%q) accepted", bad)
		}
	}
	r := testkit.Rng(t)
	for range testkit.Cases() {
		b := make([]byte, 12+int(r.Uint64()%600))
		for i := range b {
			b[i] = byte(r.Uint64())
			if i > 12 && r.Uint64()%4 == 0 {
				b[i] = byte(r.Uint64() % 64) // plausible label lengths
			}
		}
		b[4], b[5] = 0, 1
		checkDecoded(t, b)
	}
}

// TestDNSInv04_Totality: arbitrary bytes, truncations and single-byte
// mutations of valid messages decode or fail with a closed-list code.
func TestDNSInv04_Totality(t *testing.T) {
	g := newGen(testkit.Rng(t))
	r := testkit.Rng(t)
	for range testkit.Cases() {
		b, err := Encode(g.message())
		if err != nil {
			t.Fatal(err)
		}
		checkDecoded(t, b[:int(r.Uint64()%uint64(len(b)+1))])
		mut := slices.Clone(b)
		mut[int(r.Uint64()%uint64(len(mut)))] ^= byte(1 + r.Uint64()%255)
		checkDecoded(t, mut)
		junk := make([]byte, int(r.Uint64()%100))
		for i := range junk {
			junk[i] = byte(r.Uint64())
		}
		checkDecoded(t, junk)
	}
}

// TestDNSInv05_CanonicalEncode: the same message gives the same bytes, and
// decoding then re-encoding reproduces them exactly.
func TestDNSInv05_CanonicalEncode(t *testing.T) {
	g := newGen(testkit.Rng(t))
	for range testkit.Cases() {
		m := g.message()
		a, err1 := Encode(m)
		b, err2 := Encode(m)
		if err1 != nil || err2 != nil || !bytes.Equal(a, b) {
			t.Fatalf("not deterministic: %v %v", err1, err2)
		}
		d, _, err := Decode(a, Strict)
		if err != nil {
			t.Fatal(err)
		}
		c, _ := Encode(d)
		if !bytes.Equal(a, c) {
			t.Fatalf("re-encode differs:\n%x\n%x", a, c)
		}
	}
}

// TestDNSInv06_StrictIdempotence: for every input that decodes strictly,
// encode then decode gives the same message (checked inside checkDecoded on
// mutated valid messages, where many inputs decode).
func TestDNSInv06_StrictIdempotence(t *testing.T) {
	g := newGen(testkit.Rng(t))
	r := testkit.Rng(t)
	decoded := 0
	for range testkit.Cases() {
		b, _ := Encode(g.message())
		b[int(r.Uint64()%uint64(len(b)))] ^= 1 << (r.Uint64() % 8)
		if _, _, err := Decode(b, Strict); err == nil {
			decoded++
		}
		checkDecoded(t, b)
	}
	if decoded == 0 {
		t.Fatal("no mutated message decoded; the property was not exercised")
	}
}

// TestDNSInv07_Truncation: EncodeTruncated fits max, sets TC exactly when
// records were dropped, keeps a prefix of the answers and the OPT record,
// and its output decodes strictly.
func TestDNSInv07_Truncation(t *testing.T) {
	g := newGen(testkit.Rng(t))
	r := testkit.Rng(t)
	for range testkit.Cases() {
		m := g.message()
		full, _ := Encode(m)
		max := 12 + int(r.Uint64()%uint64(len(full)+20))
		b, truncated, err := EncodeTruncated(m, max)
		if err != nil {
			if CodeOf(err) != CodeMessageTooLarge {
				t.Fatal(err)
			}
			continue
		}
		if len(b) > max {
			t.Fatalf("%d octets > max %d", len(b), max)
		}
		got, _, err := Decode(b, Strict)
		if err != nil {
			t.Fatalf("truncated message does not decode: %v", err)
		}
		if truncated != (len(full) > max) || (truncated && !got.Header.Truncated) {
			t.Fatalf("truncated=%v full=%d max=%d tc=%v", truncated, len(full), max, got.Header.Truncated)
		}
		if !truncated {
			if !got.Equal(m) {
				t.Fatal("untruncated message changed")
			}
			continue
		}
		if !rrsEqual(got.Answers, m.Answers[:len(got.Answers)]) || len(got.Authority) != 0 {
			t.Fatal("answers are not a prefix, or authority kept")
		}
		_, hadOPT := m.EDNS()
		for _, a := range got.Additional {
			if a.Type != TypeOPT || !hadOPT {
				t.Fatal("non-OPT additional record kept")
			}
		}
	}
}

// TestDNSInv08_TCPFraming: ReadTCP returns exactly what AppendTCP framed;
// prefixes above max are refused.
func TestDNSInv08_TCPFraming(t *testing.T) {
	g := newGen(testkit.Rng(t))
	var stream []byte
	var msgs [][]byte
	for range 50 {
		b, _ := Encode(g.message())
		msgs = append(msgs, b)
		stream, _ = AppendTCP(stream, b)
	}
	rd := bytes.NewReader(stream)
	for i, want := range msgs {
		got, err := ReadTCP(rd, MaxMessageLen)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	framed, _ := AppendTCP(nil, msgs[0])
	if _, err := ReadTCP(bytes.NewReader(framed), len(msgs[0])-1); CodeOf(err) != CodeMessageTooLarge {
		t.Fatalf("oversized frame: %v", err)
	}
	if _, err := AppendTCP(nil, make([]byte, MaxMessageLen+1)); CodeOf(err) != CodeMessageTooLarge {
		t.Fatalf("oversized message framed: %v", err)
	}
}

// TestDNSInv09_LenientSuperset: whatever decodes strictly decodes the same
// leniently, without diagnostics (also asserted in checkDecoded).
func TestDNSInv09_LenientSuperset(t *testing.T) {
	g := newGen(testkit.Rng(t))
	for range testkit.Cases() {
		b, _ := Encode(g.message())
		checkDecoded(t, b)
	}
}

// TestDNSInv10_NameText: CanonicalName is idempotent, preserves case and
// round-trips every octet value through escapes; ReverseName and
// ParseReverseName are inverse.
func TestDNSInv10_NameText(t *testing.T) {
	g := newGen(testkit.Rng(t))
	for range testkit.Cases() {
		n := g.name()
		c, err := CanonicalName(n)
		if err != nil || c != n {
			t.Fatalf("CanonicalName(%q) = %q, %v", n, c, err)
		}
	}
	var all []byte
	for i := range 63 {
		all = append(all, byte(i*4+1))
	}
	var b strings.Builder
	appendLabelText(&b, all)
	w, err := appendNameWire(nil, b.String()+".")
	if err != nil || !bytes.Equal(w[1:1+len(all)], all) {
		t.Fatalf("escape round trip: %v %x", err, w)
	}
	if c, _ := CanonicalName("WWW.Example.com"); c != "WWW.Example.com." {
		t.Fatalf("case or absoluteness lost: %q", c)
	}
	if c, _ := CanonicalName("a\\065b."); c != "aAb." {
		t.Fatalf("\\065 not decoded: %q", c)
	}
	if !EqualNames("WWW.example.COM", "www.Example.com.") || EqualNames("a.example.", "b.example.") {
		t.Fatal("EqualNames")
	}
	r := testkit.Rng(t)
	for range testkit.Cases() {
		var a netip.Addr
		if r.Uint64()%2 == 0 {
			a = netip.AddrFrom4([4]byte{byte(r.Uint64()), byte(r.Uint64()), byte(r.Uint64()), byte(r.Uint64())})
		} else {
			var v [16]byte
			for i := range v {
				v[i] = byte(r.Uint64())
			}
			a = netip.AddrFrom16(v)
		}
		got, ok := ParseReverseName(ReverseName(a))
		if !ok || got != a.Unmap() {
			t.Fatalf("reverse %s -> %s -> %s", a, ReverseName(a), got)
		}
	}
	if ReverseName(netip.MustParseAddr("192.0.2.1")) != "1.2.0.192.in-addr.arpa." {
		t.Fatal(ReverseName(netip.MustParseAddr("192.0.2.1")))
	}
	for _, bad := range []string{"1.2.3.in-addr.arpa.", "01.2.3.4.in-addr.arpa.", "256.1.1.1.in-addr.arpa", "x.ip6.arpa."} {
		if _, ok := ParseReverseName(bad); ok {
			t.Errorf("ParseReverseName(%q) accepted", bad)
		}
	}
}

// TestDNSEDNS: EDNS fields round-trip through the OPT record.
func TestDNSEDNS(t *testing.T) {
	m := NewQuery(7, "crt.sh", TypeA)
	e := EDNS{UDPSize: 1232, ExtendedRCode: 1, Version: 0, DNSSECOK: true, Options: []Option{{Code: 65280, Data: []byte("tk")}}}
	m.SetEDNS(e)
	m.SetEDNS(e) // replaces, never duplicates
	b, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := Decode(b, Strict)
	if err != nil {
		t.Fatal(err)
	}
	ge, ok := got.EDNS()
	if !ok || ge.UDPSize != 1232 || ge.ExtendedRCode != 1 || !ge.DNSSECOK || len(got.Additional) != 1 {
		t.Fatalf("%+v", ge)
	}
	if d, ok := ge.Option(65280); !ok || string(d) != "tk" {
		t.Fatal("option lost")
	}
	if _, err := Encode(Message{Answers: []RR{{Name: ".", Type: TypeA, Data: RData{Kind: KindMX}}}}); CodeOf(err) != CodeKindMismatch {
		t.Fatalf("kind mismatch: %v", err)
	}
	if _, err := Encode(Message{Answers: []RR{{Name: ".", Type: TypeTXT, Data: RData{Kind: KindTXT, Strings: []string{strings.Repeat("x", 256)}}}}}); CodeOf(err) != CodeStringTooLong {
		t.Fatalf("long TXT: %v", err)
	}
	if !errors.Is(fail(CodePointerLoop, 3), &Error{Code: CodePointerLoop}) {
		t.Fatal("errors.Is on codes")
	}
	_ = hex.EncodeToString
}

// FuzzDNSDecode asserts the decoder invariants on arbitrary input:
// totality with closed-list codes (DNS-INV-04), name bounds (03), strict
// implies lenient (09), strict idempotence (06) and canonical re-encoding
// (05).
func FuzzDNSDecode(f *testing.F) {
	for _, file := range testkit.LoadCases[roundTripFile](f, "dnsmsg/roundtrip") {
		for _, c := range file.Data.Cases {
			f.Add(unhex(f, c.Hex))
		}
	}
	for _, file := range testkit.LoadCases[decodeFile](f, "dnsmsg/decode") {
		for _, c := range file.Data.Cases {
			f.Add(unhex(f, c.Hex))
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		checkDecoded(t, b)
		if m, _, err := Decode(b, Lenient); err == nil {
			_, _, _ = EncodeTruncated(m, 512)
		}
	})
}

// BenchmarkDNS01_Decode decodes a typical compressed response.
func BenchmarkDNS01_Decode(b *testing.B) {
	g := newGen(core.NewSplitMix64(1))
	msg, _ := Encode(g.message())
	b.ReportAllocs()
	b.SetBytes(int64(len(msg)))
	for b.Loop() {
		_, _, _ = Decode(msg, Strict)
	}
}

// BenchmarkDNS02_Encode encodes the same response.
func BenchmarkDNS02_Encode(b *testing.B) {
	g := newGen(core.NewSplitMix64(1))
	m := g.message()
	b.ReportAllocs()
	for b.Loop() {
		_, _ = Encode(m)
	}
}
