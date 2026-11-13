package socks5

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

type jsonAddr struct {
	Type uint8  `json:"type"`
	Host string `json:"host"`
	Port uint16 `json:"port"`
}

type vectorFile struct {
	Cases []struct {
		ID    string `json:"id"`
		Kind  string `json:"kind"`
		Hex   string `json:"hex"`
		Error string `json:"error"`
		Need  int    `json:"need"`
		Value struct {
			Methods  []uint8  `json:"methods"`
			Method   uint8    `json:"method"`
			Username string   `json:"username"`
			Password string   `json:"password"`
			Status   uint8    `json:"status"`
			Command  uint8    `json:"command"`
			Code     uint8    `json:"code"`
			Addr     jsonAddr `json:"addr"`
		} `json:"value"`
	} `json:"cases"`
}

func (a jsonAddr) addr() Addr {
	x := Addr{Type: AddrType(a.Type), Port: a.Port}
	if x.Type == AddrDomain {
		x.Domain = a.Host
	} else {
		x.IP = netip.MustParseAddr(a.Host)
	}
	return x
}

// parseAny parses b as kind and re-encodes the value; it returns the octets
// used, the canonical encoding and the error.
func parseAny(kind string, b []byte) (string, int, []byte, error) {
	var (
		n   int
		enc []byte
		err error
		v   string
	)
	switch kind {
	case "greeting":
		var g Greeting
		if g, n, err = ParseGreeting(b); err == nil {
			enc, err = AppendGreeting(nil, g)
			v = strings.Trim(strings.ReplaceAll(strings.Trim(stringOf(g.Methods), "[]"), " ", ","), ",")
		}
	case "method_selection":
		var s MethodSelection
		if s, n, err = ParseMethodSelection(b); err == nil {
			enc = AppendMethodSelection(nil, s)
			v = stringOf(s.Method)
		}
	case "userpass":
		var u UserPass
		if u, n, err = ParseUserPass(b); err == nil {
			enc, err = AppendUserPass(nil, u)
			v = u.Username + "|" + u.Password
		}
	case "auth_reply":
		var r AuthReply
		if r, n, err = ParseAuthReply(b); err == nil {
			enc = AppendAuthReply(nil, r)
			v = stringOf(r.Status)
		}
	case "request":
		var r Request
		if r, n, err = ParseRequest(b); err == nil {
			enc, err = AppendRequest(nil, r)
			v = stringOf(r.Command) + " " + addrKey(r.Addr)
		}
	case "reply":
		var r Reply
		if r, n, err = ParseReply(b); err == nil {
			enc, err = AppendReply(nil, r)
			v = stringOf(r.Code) + " " + addrKey(r.Addr)
		}
	}
	return v, n, enc, err
}

func stringOf(x any) string {
	var b strings.Builder
	switch v := x.(type) {
	case []Method:
		b.WriteByte('[')
		for i, m := range v {
			if i > 0 {
				b.WriteByte(' ')
			}
			b.WriteString(itoa(int(m)))
		}
		b.WriteByte(']')
	case Method:
		b.WriteString(itoa(int(v)))
	case uint8:
		b.WriteString(itoa(int(v)))
	case Command:
		b.WriteString(itoa(int(v)))
	case ReplyCode:
		b.WriteString(itoa(int(v)))
	}
	return b.String()
}

func itoa(i int) string { return strconv.Itoa(i) }

func addrKey(a Addr) string { return itoa(int(a.Type)) + " " + a.String() }

// TestSOCKSConformance: each vector parses to its value (re-encoding to
// the same octets) or fails with its code and Need.
func TestSOCKSConformance(t *testing.T) {
	for _, f := range testkit.LoadCases[vectorFile](t, "socks5") {
		for _, c := range f.Data.Cases {
			t.Run(c.ID, func(t *testing.T) {
				b, _ := hex.DecodeString(c.Hex)
				got, n, enc, err := parseAny(c.Kind, b)
				if c.Error != "" {
					var e *Error
					if !errors.As(err, &e) || string(e.Code) != c.Error || (c.Need != 0 && e.Need != c.Need) {
						t.Fatalf("error %v, want %s (need %d)", err, c.Error, c.Need)
					}
					return
				}
				if err != nil || n != len(b) || !bytes.Equal(enc, b) {
					t.Fatalf("parse %v n=%d enc=%x", err, n, enc)
				}
				v := c.Value
				var want string
				switch c.Kind {
				case "greeting":
					ms := make([]Method, len(v.Methods))
					for i, m := range v.Methods {
						ms[i] = Method(m)
					}
					want = strings.Trim(strings.ReplaceAll(strings.Trim(stringOf(ms), "[]"), " ", ","), ",")
				case "method_selection":
					want = stringOf(Method(v.Method))
				case "userpass":
					want = v.Username + "|" + v.Password
				case "auth_reply":
					want = stringOf(v.Status)
				case "request":
					want = stringOf(Command(v.Command)) + " " + addrKey(v.Addr.addr())
				case "reply":
					want = stringOf(ReplyCode(v.Code)) + " " + addrKey(v.Addr.addr())
				}
				if got != want {
					t.Fatalf("value %q, want %q", got, want)
				}
			})
		}
	}
}

var kinds = []string{"greeting", "method_selection", "userpass", "auth_reply", "request", "reply"}

var closedCodes = []Code{CodeShort, CodeBadVersion, CodeNoMethods, CodeEmptyUsername, CodeEmptyPassword, CodeBadReserved,
	CodeBadCommand, CodeBadAddressType, CodeEmptyDomain, CodeBadReplyCode, CodeTooLong, CodeBadAddress}

type gen struct{ r core.Rng }

func (g gen) n(k int) int { return int(g.r.Uint64() % uint64(k)) }

func (g gen) str(min, max int) string {
	b := make([]byte, min+g.n(max-min+1))
	for i := range b {
		b[i] = byte(g.r.Uint64())
	}
	return string(b)
}

func (g gen) addr() Addr {
	switch g.n(3) {
	case 0:
		return Addr{Type: AddrIPv4, IP: netip.AddrFrom4([4]byte{byte(g.n(256)), byte(g.n(256)), byte(g.n(256)), byte(g.n(256))}), Port: uint16(g.n(65536))}
	case 1:
		var v [16]byte
		for i := range v {
			v[i] = byte(g.n(256))
		}
		return Addr{Type: AddrIPv6, IP: netip.AddrFrom16(v), Port: uint16(g.n(65536))}
	}
	return Addr{Type: AddrDomain, Domain: g.str(1, 255), Port: uint16(g.n(65536))}
}

// encoded returns a valid encoding of a random message of kind.
func (g gen) encoded(kind string) []byte {
	var b []byte
	var err error
	switch kind {
	case "greeting":
		ms := make([]Method, 1+g.n(255))
		for i := range ms {
			ms[i] = Method(g.n(256))
		}
		b, err = AppendGreeting(nil, Greeting{Methods: ms})
	case "method_selection":
		b = AppendMethodSelection(nil, MethodSelection{Method: Method(g.n(256))})
	case "userpass":
		b, err = AppendUserPass(nil, UserPass{Username: g.str(1, 255), Password: g.str(1, 255)})
	case "auth_reply":
		b = AppendAuthReply(nil, AuthReply{Status: uint8(g.n(256))})
	case "request":
		b, err = AppendRequest(nil, Request{Command: Command(1 + g.n(3)), Addr: g.addr()})
	case "reply":
		b, err = AppendReply(nil, Reply{Code: ReplyCode(g.n(9)), Addr: g.addr()})
	}
	if err != nil {
		panic(err)
	}
	return b
}

// TestSOCKSInv01_RoundTrip: every generated message parses back from its
// encoding, using exactly its octets, and re-encodes identically.
func TestSOCKSInv01_RoundTrip(t *testing.T) {
	g := gen{testkit.Rng(t)}
	for range testkit.Cases() {
		for _, k := range kinds {
			b := g.encoded(k)
			_, n, enc, err := parseAny(k, append(slices.Clone(b), 0xAA, 0xBB))
			if err != nil || n != len(b) || !bytes.Equal(enc, b) {
				t.Fatalf("%s %x: %v n=%d enc=%x", k, b, err, n, enc)
			}
		}
	}
}

// checkParse asserts totality on arbitrary bytes: a value whose canonical
// encoding is the consumed prefix, or a closed-list error; Need is always
// beyond the input and bounded.
func checkParse(t testing.TB, b []byte) {
	t.Helper()
	for _, k := range kinds {
		_, n, enc, err := parseAny(k, b)
		if err != nil {
			var e *Error
			if !errors.As(err, &e) || !slices.Contains(closedCodes, e.Code) {
				t.Fatalf("SOCKS-INV-02: %s: error outside the closed list: %v", k, err)
			}
			if e.Code == CodeShort && (e.Need <= len(b) || e.Need > 2*(MaxLen+2)+3) {
				t.Fatalf("SOCKS-INV-03: %s: need %d with %d octets", k, e.Need, len(b))
			}
			continue
		}
		if n > len(b) || !bytes.Equal(enc, b[:n]) {
			t.Fatalf("SOCKS-INV-02: %s: consumed %d of %d, re-encoding %x != %x", k, n, len(b), enc, b[:n])
		}
	}
}

// TestSOCKSInv02_Totality: arbitrary bytes and mutated valid messages
// parse or fail with closed-list codes.
func TestSOCKSInv02_Totality(t *testing.T) {
	g := gen{testkit.Rng(t)}
	for range testkit.Cases() {
		checkParse(t, []byte(g.str(0, 40)))
		b := []byte(g.encoded(kinds[g.n(len(kinds))]))
		b[g.n(len(b))] ^= byte(1 + g.n(255))
		checkParse(t, b)
	}
}

// TestSOCKSInv03_Prefix: every strict prefix of a valid encoding is short,
// asks for more than it has and no more than the full message.
func TestSOCKSInv03_Prefix(t *testing.T) {
	g := gen{testkit.Rng(t)}
	for range testkit.Cases() / 10 {
		for _, k := range kinds {
			b := g.encoded(k)
			for i := range len(b) {
				_, _, _, err := parseAny(k, b[:i])
				var e *Error
				if !errors.As(err, &e) || e.Code != CodeShort || e.Need <= i || e.Need > len(b) {
					t.Fatalf("%s prefix %d/%d: %v", k, i, len(b), err)
				}
			}
		}
	}
}

// pipe runs the server handshake against raw client bytes and returns what
// the server wrote and the handshake result.
func serverWith(t *testing.T, client []byte, auth func(UserPass) bool) ([]byte, Request, error) {
	t.Helper()
	var out bytes.Buffer
	rw := struct {
		io.Reader
		io.Writer
	}{bytes.NewReader(client), &out}
	req, _, err := ServerHandshake(rw, auth)
	return out.Bytes(), req, err
}

func clientBytes(t *testing.T, methods []Method, up UserPass, req []byte) []byte {
	t.Helper()
	g, _ := AppendGreeting(nil, Greeting{Methods: methods})
	u, _ := AppendUserPass(nil, up)
	return append(append(g, u...), req...)
}

// TestSOCKSInv04_UnsupportedRefused: BIND, UDP ASSOCIATE and unknown
// commands get reply 7, unknown address types reply 8, a client without
// username/password gets 0xFF, bad credentials status 1; none reach the
// caller as a CONNECT.
func TestSOCKSInv04_UnsupportedRefused(t *testing.T) {
	ok := func(UserPass) bool { return true }
	up := UserPass{Username: "dec_1", Password: "tk"}
	for _, cmd := range []byte{2, 3, 0, 4, 0xFF} {
		req := append([]byte{5, cmd, 0}, 1, 1, 2, 3, 4, 0, 80)
		out, _, err := serverWith(t, clientBytes(t, []Method{MethodUserPass}, up, req), ok)
		rep, _, perr := ParseReply(out[4:])
		if CodeOf(err) != CodeCommandRefused || perr != nil || rep.Code != CommandNotSupported {
			t.Fatalf("cmd %d: %v reply %+v %v", cmd, err, rep, perr)
		}
	}
	out, _, err := serverWith(t, clientBytes(t, []Method{MethodUserPass}, up, []byte{5, 1, 0, 9, 1, 2, 3, 4, 0, 80}), ok)
	if rep, _, _ := ParseReply(out[4:]); CodeOf(err) != CodeBadAddressType || rep.Code != AddressTypeNotSupported {
		t.Fatalf("atyp: %v %+v", err, rep)
	}
	out, _, err = serverWith(t, []byte{5, 1, 0}, ok)
	if CodeOf(err) != CodeNoAcceptableMethod || !bytes.Equal(out, []byte{5, 0xFF}) {
		t.Fatalf("no-auth client: %v %x", err, out)
	}
	connect, _ := AppendRequest(nil, Request{Command: CmdConnect, Addr: AddrFrom("app.test", 443)})
	out, _, err = serverWith(t, clientBytes(t, []Method{MethodNoAuth, MethodUserPass}, up, connect), func(UserPass) bool { return false })
	if CodeOf(err) != CodeAuthFailed || !bytes.Equal(out, []byte{5, 2, 1, 1}) {
		t.Fatalf("bad credentials: %v %x", err, out)
	}
	_, req, err := serverWith(t, clientBytes(t, []Method{MethodUserPass}, up, connect), ok)
	if err != nil || req.Command != CmdConnect || req.Addr.String() != "app.test:443" {
		t.Fatalf("connect: %v %+v", err, req)
	}
}

// TestSOCKSInv05_MaxLengths: 255-octet fields encode, 256 octets and empty
// required fields do not.
func TestSOCKSInv05_MaxLengths(t *testing.T) {
	l255, l256 := strings.Repeat("x", 255), strings.Repeat("x", 256)
	if _, err := AppendUserPass(nil, UserPass{Username: l255, Password: l255}); err != nil {
		t.Fatal(err)
	}
	for _, u := range []UserPass{{l256, "p"}, {"u", l256}} {
		if _, err := AppendUserPass(nil, u); CodeOf(err) != CodeTooLong {
			t.Fatalf("%d/%d: %v", len(u.Username), len(u.Password), err)
		}
	}
	if _, err := AppendUserPass(nil, UserPass{Password: "p"}); CodeOf(err) != CodeEmptyUsername {
		t.Fatal(err)
	}
	if _, err := AppendRequest(nil, Request{Command: CmdConnect, Addr: Addr{Type: AddrDomain, Domain: l255}}); err != nil {
		t.Fatal(err)
	}
	if _, err := AppendRequest(nil, Request{Command: CmdConnect, Addr: Addr{Type: AddrDomain, Domain: l256}}); CodeOf(err) != CodeTooLong {
		t.Fatal(err)
	}
	if _, err := AppendRequest(nil, Request{Command: CmdConnect, Addr: Addr{Type: AddrIPv4, IP: netip.MustParseAddr("::1")}}); CodeOf(err) != CodeBadAddress {
		t.Fatal(err)
	}
	if _, err := AppendGreeting(nil, Greeting{Methods: make([]Method, 256)}); CodeOf(err) != CodeTooLong {
		t.Fatal(err)
	}
}

// TestSOCKSInv06_Handshake: client and server handshakes agree over a
// connection, and tunnelled bytes after the request are not consumed by
// the codec.
func TestSOCKSInv06_Handshake(t *testing.T) {
	g := gen{testkit.Rng(t)}
	for range 50 {
		up := UserPass{Username: g.str(1, 255), Password: g.str(1, 255)}
		addr := g.addr()
		c, s := net.Pipe()
		done := make(chan error, 1)
		go func() {
			defer s.Close()
			req, got, err := ServerHandshake(s, func(u UserPass) bool { return u == up })
			if err == nil && (req.Addr != addr || got != up) {
				err = errors.New("server saw a different request")
			}
			if err == nil {
				err = WriteReply(s, Reply{Code: Succeeded, Addr: AddrFrom("192.0.2.1", 9)})
			}
			if err == nil {
				b := make([]byte, 4)
				_, err = io.ReadFull(s, b)
				if string(b) != "ping" {
					err = errors.New("tunnel bytes lost")
				}
			}
			done <- err
		}()
		rep, err := ClientConnect(c, up.Username, up.Password, addr)
		if err != nil || rep.Addr.String() != "192.0.2.1:9" {
			t.Fatalf("client: %v %+v", err, rep)
		}
		_, _ = c.Write([]byte("ping"))
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	// A failure reply surfaces as CodeRequestFailed with the code.
	c, s := net.Pipe()
	go func() {
		defer s.Close()
		if _, _, err := ServerHandshake(s, func(UserPass) bool { return true }); err == nil {
			_ = WriteReply(s, Reply{Code: NotAllowed})
		}
	}()
	_, err := ClientConnect(c, "u", "p", AddrFrom("evil.test", 80))
	var e *Error
	if !errors.As(err, &e) || e.Code != CodeRequestFailed || e.Reply != NotAllowed {
		t.Fatalf("refusal: %v", err)
	}
	c.Close()
}

// FuzzSOCKS5 asserts totality, canonical re-encoding of the consumed
// prefix and bounded Need (SOCKS-INV-02, 03) for every message kind, and
// that the server handshake never accepts a non-CONNECT request.
func FuzzSOCKS5(f *testing.F) {
	for _, file := range testkit.LoadCases[vectorFile](f, "socks5") {
		for _, c := range file.Data.Cases {
			b, _ := hex.DecodeString(c.Hex)
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		checkParse(t, b)
		var out bytes.Buffer
		req, _, err := ServerHandshake(struct {
			io.Reader
			io.Writer
		}{bytes.NewReader(b), &out}, func(UserPass) bool { return true })
		if err == nil && req.Command != CmdConnect {
			t.Fatalf("SOCKS-INV-04: handshake accepted command %d", req.Command)
		}
	})
}
