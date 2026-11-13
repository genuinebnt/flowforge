package socks5

import (
	"io"
	"slices"
)

// readMsg reads exactly one message with parse: it grows the buffer to the
// length the parser asks for and never reads further, so the octets after
// the message stay in r.
func readMsg[T any](r io.Reader, parse func([]byte) (T, int, error)) (T, error) {
	buf := make([]byte, 0, 32)
	for {
		v, _, err := parse(buf)
		e, ok := err.(*Error)
		if !ok || e.Code != CodeShort {
			return v, err
		}
		need := e.Need
		// Need always grows and the longest message (an RFC 1929 request,
		// 3+255+255 octets) is below this bound; the check keeps a buggy
		// parser from looping or allocating without limit.
		if need <= len(buf) || need > 2+2*(MaxLen+1)+16 {
			var zero T
			return zero, err
		}
		old := len(buf)
		buf = slices.Grow(buf, need-old)[:need]
		if _, err := io.ReadFull(r, buf[old:]); err != nil {
			var zero T
			return zero, err
		}
	}
}

// ReadGreeting reads a greeting from r.
func ReadGreeting(r io.Reader) (Greeting, error) { return readMsg(r, ParseGreeting) }

// ReadMethodSelection reads a method selection from r.
func ReadMethodSelection(r io.Reader) (MethodSelection, error) {
	return readMsg(r, ParseMethodSelection)
}

// ReadUserPass reads an RFC 1929 request from r.
func ReadUserPass(r io.Reader) (UserPass, error) { return readMsg(r, ParseUserPass) }

// ReadAuthReply reads an RFC 1929 status from r.
func ReadAuthReply(r io.Reader) (AuthReply, error) { return readMsg(r, ParseAuthReply) }

// ReadRequest reads a request from r.
func ReadRequest(r io.Reader) (Request, error) { return readMsg(r, ParseRequest) }

// ReadReply reads a reply from r.
func ReadReply(r io.Reader) (Reply, error) { return readMsg(r, ParseReply) }

// WriteReply encodes r and writes it to w in one Write call.
func WriteReply(w io.Writer, r Reply) error {
	b, err := AppendReply(nil, r)
	if err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ServerHandshake runs the server side up to the request (RFC 1928 §3-§4,
// RFC 1929): it requires username/password authentication (a client that
// does not offer it gets 0xFF), checks the credentials with auth, and reads
// the request. It refuses BIND, UDP ASSOCIATE and unknown commands with
// reply 7 (returning an *Error with CodeCommandRefused), unknown address
// types with reply 8 and other malformed requests with reply 1 (returning
// the parse error); a short or failed read sends nothing. Failed credentials get
// status 1 and CodeAuthFailed. On success the caller decides the CONNECT
// and must write the Reply itself. Octets after the request stay unread
// in rw.
func ServerHandshake(rw io.ReadWriter, auth func(UserPass) bool) (Request, UserPass, error) {
	g, err := ReadGreeting(rw)
	if err != nil {
		return Request{}, UserPass{}, err
	}
	if !slices.Contains(g.Methods, MethodUserPass) {
		_, _ = rw.Write(AppendMethodSelection(nil, MethodSelection{Method: MethodNoAcceptable}))
		return Request{}, UserPass{}, fail(CodeNoAcceptableMethod)
	}
	if _, err := rw.Write(AppendMethodSelection(nil, MethodSelection{Method: MethodUserPass})); err != nil {
		return Request{}, UserPass{}, err
	}
	up, err := ReadUserPass(rw)
	if err != nil {
		_, _ = rw.Write(AppendAuthReply(nil, AuthReply{Status: 1}))
		return Request{}, UserPass{}, err
	}
	if !auth(up) {
		_, _ = rw.Write(AppendAuthReply(nil, AuthReply{Status: 1}))
		return Request{}, up, fail(CodeAuthFailed)
	}
	if _, err := rw.Write(AppendAuthReply(nil, AuthReply{Status: 0})); err != nil {
		return Request{}, up, err
	}
	req, err := ReadRequest(rw)
	if err != nil {
		switch CodeOf(err) {
		case CodeBadCommand:
			_ = WriteReply(rw, Reply{Code: CommandNotSupported})
			return Request{}, up, &Error{Code: CodeCommandRefused, Reply: CommandNotSupported}
		case CodeBadAddressType:
			_ = WriteReply(rw, Reply{Code: AddressTypeNotSupported})
		case CodeShort, "":
		default:
			_ = WriteReply(rw, Reply{Code: GeneralFailure})
		}
		return Request{}, up, err
	}
	if req.Command != CmdConnect {
		_ = WriteReply(rw, Reply{Code: CommandNotSupported})
		return req, up, &Error{Code: CodeCommandRefused, Reply: CommandNotSupported}
	}
	return req, up, nil
}

// ClientConnect runs the client side of a CONNECT to addr with
// username/password authentication (or no authentication when user is
// empty) and returns the server's success reply. A failure reply is
// returned together with an *Error with CodeRequestFailed and Reply set; a
// server that picks another method gives CodeNoAcceptableMethod and a
// non-zero auth status gives CodeAuthFailed.
func ClientConnect(rw io.ReadWriter, user, pass string, addr Addr) (Reply, error) {
	method := MethodUserPass
	if user == "" {
		method = MethodNoAuth
	}
	g, err := AppendGreeting(nil, Greeting{Methods: []Method{method}})
	if err != nil {
		return Reply{}, err
	}
	if _, err := rw.Write(g); err != nil {
		return Reply{}, err
	}
	sel, err := ReadMethodSelection(rw)
	if err != nil {
		return Reply{}, err
	}
	if sel.Method != method {
		return Reply{}, fail(CodeNoAcceptableMethod)
	}
	if method == MethodUserPass {
		b, err := AppendUserPass(nil, UserPass{Username: user, Password: pass})
		if err != nil {
			return Reply{}, err
		}
		if _, err := rw.Write(b); err != nil {
			return Reply{}, err
		}
		ar, err := ReadAuthReply(rw)
		if err != nil {
			return Reply{}, err
		}
		if ar.Status != 0 {
			return Reply{}, fail(CodeAuthFailed)
		}
	}
	b, err := AppendRequest(nil, Request{Command: CmdConnect, Addr: addr})
	if err != nil {
		return Reply{}, err
	}
	if _, err := rw.Write(b); err != nil {
		return Reply{}, err
	}
	rep, err := ReadReply(rw)
	if err != nil {
		return Reply{}, err
	}
	if rep.Code != Succeeded {
		return rep, &Error{Code: CodeRequestFailed, Reply: rep.Code}
	}
	return rep, nil
}
