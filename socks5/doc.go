// Package socks5 is the SOCKS protocol version 5 codec (RFC 1928) with
// username/password authentication (RFC 1929): every message of the
// handshake as a parser over bytes and a canonical encoder, plus server and
// client handshakes over an io.ReadWriter. The egress proxy serves it and
// the tool runner speaks it (spec 29.10, 35.1; V2_DESIGN §9).
//
// Each Parse function takes the bytes received so far and returns the
// message and the number of octets it used, or an *Error. An *Error with
// CodeShort carries Need, the total length known to be required, so a
// reader can fetch exactly that many octets and never reads past the
// message into tunnelled data. Parsing is total and strict: the reserved
// octet must be zero, versions must match and lengths must be non-zero
// where the RFCs require it. Encoding is canonical: Append(Parse(b)) is
// b[:n].
//
// Only CONNECT is served. BIND and UDP ASSOCIATE parse, but ServerHandshake
// always refuses them with reply 7 (command not supported); unknown
// commands also get 7 and unknown address types 8 (RFC 1928 §6).
//
// PORT: Go strings hold arbitrary octets. Usernames, passwords and domain
// names are kept exactly as received (RFC 1928 and 1929 do not require
// UTF-8), so a Rust port stores them as Vec<u8>, not String.
package socks5
