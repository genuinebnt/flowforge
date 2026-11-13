// Package dnsmsg is the DNS wire format (RFC 1035 §4, RFC 3596, RFC 2782,
// RFC 6891, RFC 8659): an explicit decoder and a canonical encoder for DNS
// messages, used by the egress proxy's DNS forwarder and by the tools that
// speak DNS through it (spec 29.9, 35.3; V2_DESIGN §9).
//
// Decoding is total: any byte string yields a Message or an *Error with a
// stable Code, never a panic or an unbounded loop. Names are decoded by an
// explicit state machine (RFC 1035 §4.1.4) that only follows compression
// pointers strictly backwards, so crafted cyclic pointers are rejected in
// time linear in the message length. Every decoded name is at most 255
// octets in wire form with labels of at most 63 octets (RFC 1035 §2.3.4).
//
// Names are carried as canonical presentation text: labels joined by '.',
// always absolute (trailing '.'), the root as ".". Inside a label '.' and
// '\' are escaped with a backslash and octets outside 0x21-0x7E as \DDD
// (RFC 1035 §5.1). Case is preserved; EqualNames compares ASCII
// case-insensitively (RFC 4343).
//
// Encoding is canonical: the same Message always yields the same bytes.
// Owner names, question names and the names inside NS, CNAME, PTR, MX and
// SOA data are compressed against the first earlier occurrence of the
// longest matching suffix (exact octets, so case survives a round trip);
// SRV targets are never compressed (RFC 2782, RFC 3597 §4).
//
// Strict mode rejects trailing octets, malformed data of a known type, OPT
// records outside the rules of RFC 6891 §6.1.1 and TXT records without a
// string. Lenient mode ignores trailing octets, keeps malformed data as
// KindRaw and misplaced OPT records as they are, each with a Diagnostic,
// and keeps a TXT record without strings as KindTXT with no strings and no
// diagnostic (conformance case txt_empty_lenient). The package performs no
// I/O except the TCP framing helpers, which read from and write to the io
// interfaces they are given (RFC 1035 §4.2.2). Every function is pure and
// safe for concurrent use.
//
// Wire integers are big-endian (network order), unlike the little-endian
// Flowforge file formats.
package dnsmsg
