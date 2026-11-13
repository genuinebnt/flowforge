package socks5

import (
	"encoding/binary"
	"net/netip"
	"strconv"
)

// Protocol versions: the VER octet of every RFC 1928 message and of the
// RFC 1929 sub-negotiation.
const (
	Version     = 5 // RFC 1928
	AuthVersion = 1 // RFC 1929 sub-negotiation
)

// Method is an authentication method (RFC 1928 §3).
type Method uint8

// Methods (RFC 1928 §3). MethodGSSAPI is listed for parsing only; the
// handshakes never select it.
const (
	MethodNoAuth       Method = 0x00
	MethodGSSAPI       Method = 0x01
	MethodUserPass     Method = 0x02
	MethodNoAcceptable Method = 0xFF
)

// Command is a request command (RFC 1928 §4).
type Command uint8

// Commands.
const (
	CmdConnect      Command = 1
	CmdBind         Command = 2
	CmdUDPAssociate Command = 3
)

// AddrType is an address type (RFC 1928 §5).
type AddrType uint8

// Address types.
const (
	AddrIPv4   AddrType = 1
	AddrDomain AddrType = 3
	AddrIPv6   AddrType = 4
)

// ReplyCode is the REP field (RFC 1928 §6).
type ReplyCode uint8

// Reply codes.
const (
	Succeeded               ReplyCode = 0
	GeneralFailure          ReplyCode = 1
	NotAllowed              ReplyCode = 2 // connection not allowed by ruleset
	NetworkUnreachable      ReplyCode = 3
	HostUnreachable         ReplyCode = 4
	ConnectionRefused       ReplyCode = 5
	TTLExpired              ReplyCode = 6
	CommandNotSupported     ReplyCode = 7
	AddressTypeNotSupported ReplyCode = 8
)

// String returns the RFC 1928 text of c.
func (c ReplyCode) String() string {
	switch c {
	case Succeeded:
		return "succeeded"
	case GeneralFailure:
		return "general SOCKS server failure"
	case NotAllowed:
		return "connection not allowed by ruleset"
	case NetworkUnreachable:
		return "network unreachable"
	case HostUnreachable:
		return "host unreachable"
	case ConnectionRefused:
		return "connection refused"
	case TTLExpired:
		return "TTL expired"
	case CommandNotSupported:
		return "command not supported"
	case AddressTypeNotSupported:
		return "address type not supported"
	}
	return "reply " + strconv.Itoa(int(c))
}

// MaxLen is the longest username, password and domain name (one length
// octet).
const MaxLen = 255

// Code is a stable error code; the list is closed.
type Code string

// Error codes.
const (
	CodeShort              Code = "short"                 // more octets are needed (see Error.Need)
	CodeBadVersion         Code = "bad_version"           // VER is not 5 (or 1 for RFC 1929)
	CodeNoMethods          Code = "no_methods"            // NMETHODS is 0
	CodeEmptyUsername      Code = "empty_username"        // ULEN is 0
	CodeEmptyPassword      Code = "empty_password"        // PLEN is 0
	CodeBadReserved        Code = "bad_reserved"          // RSV is not 0
	CodeBadCommand         Code = "bad_command"           // CMD is not 1, 2 or 3
	CodeBadAddressType     Code = "bad_address_type"      // ATYP is not 1, 3 or 4
	CodeEmptyDomain        Code = "empty_domain"          // a domain address of length 0
	CodeBadReplyCode       Code = "bad_reply_code"        // REP above 8
	CodeTooLong            Code = "too_long"              // encoding a field over 255 octets
	CodeBadAddress         Code = "bad_address"           // encoding an address whose fields do not match its type
	CodeNoAcceptableMethod Code = "no_acceptable_method"  // handshake: no common method
	CodeAuthFailed         Code = "auth_failed"           // handshake: credentials refused
	CodeCommandRefused     Code = "command_not_supported" // handshake: BIND, UDP ASSOCIATE or unknown command
	CodeRequestFailed      Code = "request_failed"        // handshake: the server replied with a failure
)

// Error is a codec or handshake failure.
type Error struct {
	Code Code
	// Need is set with CodeShort: the total number of octets known to be
	// required (more may be needed once they arrive).
	Need int
	// Reply is set with CodeRequestFailed and CodeCommandRefused.
	Reply ReplyCode
}

// Error returns "socks5: <code>", with the Need or the reply text where
// set.
func (e *Error) Error() string {
	switch e.Code {
	case CodeShort:
		return "socks5: short message (need " + strconv.Itoa(e.Need) + " octets)"
	case CodeRequestFailed, CodeCommandRefused:
		return "socks5: " + string(e.Code) + ": " + e.Reply.String()
	}
	return "socks5: " + string(e.Code)
}

// Is matches *Error values by Code.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

// CodeOf returns the Code of err, or "".
func CodeOf(err error) Code {
	if e, ok := err.(*Error); ok {
		return e.Code
	}
	return ""
}

func short(need int) error { return &Error{Code: CodeShort, Need: need} }
func fail(c Code) error    { return &Error{Code: c} }

// Addr is a destination or bound address.
type Addr struct {
	Type   AddrType
	IP     netip.Addr // AddrIPv4, AddrIPv6
	Domain string     // AddrDomain, raw octets (1..255)
	Port   uint16
}

// Host returns the domain octets as given, or the IP in netip text form.
// A zero Addr returns "invalid IP".
func (a Addr) Host() string {
	if a.Type == AddrDomain {
		return a.Domain
	}
	return a.IP.String()
}

// String returns host:port, with an IPv6 host in brackets.
func (a Addr) String() string {
	h := a.Host()
	if a.Type == AddrIPv6 {
		h = "[" + h + "]"
	}
	return h + ":" + strconv.Itoa(int(a.Port))
}

// AddrFrom returns the Addr for host and port: AddrIPv4 or AddrIPv6 for an
// IP literal (an IPv4-mapped IPv6 literal stays IPv6), AddrDomain for
// anything else. A zoned IPv6 literal has no RFC 1928 encoding, so it is
// sent as a domain name and the server resolves (or refuses) it. host is
// not validated; AppendRequest rejects an empty or over-long domain.
func AddrFrom(host string, port uint16) Addr {
	if ip, err := netip.ParseAddr(host); err == nil && ip.Zone() == "" {
		if ip.Is4() {
			return Addr{Type: AddrIPv4, IP: ip, Port: port}
		}
		return Addr{Type: AddrIPv6, IP: ip, Port: port}
	}
	return Addr{Type: AddrDomain, Domain: host, Port: port}
}

// Greeting is the client's version identifier/method selection message
// (RFC 1928 §3): the methods it offers, 1..255 of them.
type Greeting struct{ Methods []Method }

// MethodSelection is the server's METHOD choice (RFC 1928 §3); 0xFF means
// none of the offered methods is acceptable.
type MethodSelection struct{ Method Method }

// UserPass is the RFC 1929 §2 username/password request; each field is
// 1..255 raw octets.
type UserPass struct{ Username, Password string }

// AuthReply is the RFC 1929 §2 status: 0 is success, anything else is
// failure and the server closes the connection.
type AuthReply struct{ Status uint8 }

// Request is a client request (RFC 1928 §4): the command and DST.ADDR,
// DST.PORT.
type Request struct {
	Command Command
	Addr    Addr
}

// Reply is a server reply (RFC 1928 §6): REP and BND.ADDR, BND.PORT.
type Reply struct {
	Code ReplyCode
	Addr Addr
}

// ParseGreeting parses VER NMETHODS METHODS (RFC 1928 §3).
func ParseGreeting(b []byte) (Greeting, int, error) {
	if len(b) < 2 {
		return Greeting{}, 0, short(2)
	}
	if b[0] != Version {
		return Greeting{}, 0, fail(CodeBadVersion)
	}
	n := int(b[1])
	if n == 0 {
		return Greeting{}, 0, fail(CodeNoMethods)
	}
	if len(b) < 2+n {
		return Greeting{}, 0, short(2 + n)
	}
	ms := make([]Method, n)
	for i := range ms {
		ms[i] = Method(b[2+i])
	}
	return Greeting{Methods: ms}, 2 + n, nil
}

// AppendGreeting appends the encoding of g to dst. It fails with
// CodeNoMethods or CodeTooLong (and leaves dst unchanged) unless g has
// 1..255 methods.
func AppendGreeting(dst []byte, g Greeting) ([]byte, error) {
	if len(g.Methods) == 0 {
		return dst, fail(CodeNoMethods)
	}
	if len(g.Methods) > MaxLen {
		return dst, fail(CodeTooLong)
	}
	dst = append(dst, Version, byte(len(g.Methods)))
	for _, m := range g.Methods {
		dst = append(dst, byte(m))
	}
	return dst, nil
}

// ParseMethodSelection parses VER METHOD (RFC 1928 §3).
func ParseMethodSelection(b []byte) (MethodSelection, int, error) {
	if len(b) < 2 {
		return MethodSelection{}, 0, short(2)
	}
	if b[0] != Version {
		return MethodSelection{}, 0, fail(CodeBadVersion)
	}
	return MethodSelection{Method: Method(b[1])}, 2, nil
}

// AppendMethodSelection appends the encoding of s to dst; every method
// octet encodes.
func AppendMethodSelection(dst []byte, s MethodSelection) []byte {
	return append(dst, Version, byte(s.Method))
}

// ParseUserPass parses VER ULEN UNAME PLEN PASSWD (RFC 1929 §2).
func ParseUserPass(b []byte) (UserPass, int, error) {
	if len(b) < 2 {
		return UserPass{}, 0, short(2)
	}
	if b[0] != AuthVersion {
		return UserPass{}, 0, fail(CodeBadVersion)
	}
	ul := int(b[1])
	if ul == 0 {
		return UserPass{}, 0, fail(CodeEmptyUsername)
	}
	if len(b) < 3+ul {
		return UserPass{}, 0, short(3 + ul)
	}
	pl := int(b[2+ul])
	if pl == 0 {
		return UserPass{}, 0, fail(CodeEmptyPassword)
	}
	n := 3 + ul + pl
	if len(b) < n {
		return UserPass{}, 0, short(n)
	}
	return UserPass{Username: string(b[2 : 2+ul]), Password: string(b[3+ul : n])}, n, nil
}

// AppendUserPass appends the encoding of u to dst. Each field must be
// 1..255 octets (CodeEmptyUsername, CodeEmptyPassword, CodeTooLong); on
// error dst is returned unchanged.
func AppendUserPass(dst []byte, u UserPass) ([]byte, error) {
	switch {
	case u.Username == "":
		return dst, fail(CodeEmptyUsername)
	case u.Password == "":
		return dst, fail(CodeEmptyPassword)
	case len(u.Username) > MaxLen || len(u.Password) > MaxLen:
		return dst, fail(CodeTooLong)
	}
	dst = append(dst, AuthVersion, byte(len(u.Username)))
	dst = append(dst, u.Username...)
	dst = append(dst, byte(len(u.Password)))
	return append(dst, u.Password...), nil
}

// ParseAuthReply parses VER STATUS (RFC 1929 §2; 0 is success).
func ParseAuthReply(b []byte) (AuthReply, int, error) {
	if len(b) < 2 {
		return AuthReply{}, 0, short(2)
	}
	if b[0] != AuthVersion {
		return AuthReply{}, 0, fail(CodeBadVersion)
	}
	return AuthReply{Status: b[1]}, 2, nil
}

// AppendAuthReply appends the encoding of r to dst.
func AppendAuthReply(dst []byte, r AuthReply) []byte { return append(dst, AuthVersion, r.Status) }

// parseAddr parses ATYP ADDR PORT at b[off:] (RFC 1928 §5; the domain is
// length-prefixed, the port big-endian).
func parseAddr(b []byte, off int) (Addr, int, error) {
	if len(b) < off+1 {
		return Addr{}, 0, short(off + 1)
	}
	var a Addr
	var n int
	switch AddrType(b[off]) {
	case AddrIPv4:
		n = off + 1 + 4 + 2
		if len(b) < n {
			return Addr{}, 0, short(n)
		}
		a = Addr{Type: AddrIPv4, IP: netip.AddrFrom4([4]byte(b[off+1 : off+5]))}
	case AddrIPv6:
		n = off + 1 + 16 + 2
		if len(b) < n {
			return Addr{}, 0, short(n)
		}
		a = Addr{Type: AddrIPv6, IP: netip.AddrFrom16([16]byte(b[off+1 : off+17]))}
	case AddrDomain:
		if len(b) < off+2 {
			return Addr{}, 0, short(off + 2)
		}
		l := int(b[off+1])
		if l == 0 {
			return Addr{}, 0, fail(CodeEmptyDomain)
		}
		n = off + 2 + l + 2
		if len(b) < n {
			return Addr{}, 0, short(n)
		}
		a = Addr{Type: AddrDomain, Domain: string(b[off+2 : off+2+l])}
	default:
		return Addr{}, 0, fail(CodeBadAddressType)
	}
	a.Port = binary.BigEndian.Uint16(b[n-2:])
	return a, n, nil
}

// appendAddr appends ATYP ADDR PORT. Fields that do not belong to a.Type
// must be zero (CodeBadAddress), so Parse(Append(a)) == a holds
// (SOCKS-INV-01).
func appendAddr(dst []byte, a Addr) ([]byte, error) {
	switch a.Type {
	case AddrIPv4:
		if !a.IP.Is4() || a.Domain != "" {
			return dst, fail(CodeBadAddress)
		}
		v := a.IP.As4()
		dst = append(append(dst, byte(AddrIPv4)), v[:]...)
	case AddrIPv6:
		if !a.IP.Is6() || a.IP.Zone() != "" || a.Domain != "" {
			return dst, fail(CodeBadAddress)
		}
		v := a.IP.As16()
		dst = append(append(dst, byte(AddrIPv6)), v[:]...)
	case AddrDomain:
		switch {
		case a.Domain == "":
			return dst, fail(CodeEmptyDomain)
		case len(a.Domain) > MaxLen:
			return dst, fail(CodeTooLong)
		case a.IP.IsValid():
			return dst, fail(CodeBadAddress)
		}
		dst = append(dst, byte(AddrDomain), byte(len(a.Domain)))
		dst = append(dst, a.Domain...)
	default:
		return dst, fail(CodeBadAddressType)
	}
	return binary.BigEndian.AppendUint16(dst, a.Port), nil
}

// ParseRequest parses VER CMD RSV ATYP DST.ADDR DST.PORT (RFC 1928 §4).
// Commands 1-3 parse (ServerHandshake refuses 2 and 3); others fail with
// CodeBadCommand. Fields are checked in wire order, so the first bad octet
// decides the code.
func ParseRequest(b []byte) (Request, int, error) {
	if len(b) < 3 {
		return Request{}, 0, short(3)
	}
	if b[0] != Version {
		return Request{}, 0, fail(CodeBadVersion)
	}
	cmd := Command(b[1])
	if cmd < CmdConnect || cmd > CmdUDPAssociate {
		return Request{}, 0, fail(CodeBadCommand)
	}
	if b[2] != 0 {
		return Request{}, 0, fail(CodeBadReserved)
	}
	a, n, err := parseAddr(b, 3)
	if err != nil {
		return Request{}, 0, err
	}
	return Request{Command: cmd, Addr: a}, n, nil
}

// AppendRequest appends the encoding of r to dst; it fails with
// CodeBadCommand or an address error from the rules of appendAddr.
func AppendRequest(dst []byte, r Request) ([]byte, error) {
	if r.Command < CmdConnect || r.Command > CmdUDPAssociate {
		return dst, fail(CodeBadCommand)
	}
	return appendAddr(append(dst, Version, byte(r.Command), 0), r.Addr)
}

// ParseReply parses VER REP RSV ATYP BND.ADDR BND.PORT (RFC 1928 §6). A
// REP above 8 (not assigned by RFC 1928) fails with CodeBadReplyCode.
func ParseReply(b []byte) (Reply, int, error) {
	if len(b) < 3 {
		return Reply{}, 0, short(3)
	}
	if b[0] != Version {
		return Reply{}, 0, fail(CodeBadVersion)
	}
	code := ReplyCode(b[1])
	if code > AddressTypeNotSupported {
		return Reply{}, 0, fail(CodeBadReplyCode)
	}
	if b[2] != 0 {
		return Reply{}, 0, fail(CodeBadReserved)
	}
	a, n, err := parseAddr(b, 3)
	if err != nil {
		return Reply{}, 0, err
	}
	return Reply{Code: code, Addr: a}, n, nil
}

// AppendReply appends the encoding of r to dst. A reply whose Addr is the
// zero value carries 0.0.0.0:0, the conventional "no bound address".
func AppendReply(dst []byte, r Reply) ([]byte, error) {
	if r.Code > AddressTypeNotSupported {
		return dst, fail(CodeBadReplyCode)
	}
	a := r.Addr
	if a.Type == 0 {
		a = Addr{Type: AddrIPv4, IP: netip.IPv4Unspecified()}
	}
	return appendAddr(append(dst, Version, byte(r.Code), 0), a)
}
