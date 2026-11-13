# socks5

Layer-1 SOCKS 5 codec (RFC 1928) with username/password authentication
(RFC 1929): `Parse*` / `Append*` for the greeting, method selection,
username/password request and status, request and reply (IPv4, IPv6 and
domain addresses, every reply code), `Read*` helpers that read exactly one
message (never into tunnelled data), `ServerHandshake` and
`ClientConnect`. The egress proxy serves it; the tool runner dials through
it (V2_DESIGN §9).

Only CONNECT is served: BIND and UDP ASSOCIATE parse, but
`ServerHandshake` always refuses them with reply 7 (command not
supported); unknown commands get 7, unknown address types 8, clients that
do not offer username/password get method 0xFF, rejected credentials get
status 1.

Parsers are strict and total; short input returns `CodeShort` with
`Need`, the total length known to be required. Error codes (closed list):
`short`, `bad_version`, `no_methods`, `empty_username`, `empty_password`,
`bad_reserved`, `bad_command`, `bad_address_type`, `empty_domain`,
`bad_reply_code`, `too_long`, `bad_address`, and for handshakes
`no_acceptable_method`, `auth_failed`, `command_not_supported`,
`request_failed`.

## Invariants

| Id | Statement | Test |
|----|-----------|------|
| SOCKS-INV-01 | Every message round-trips: parse(append(m)) uses exactly its octets and re-encodes identically | `TestSOCKSInv01_RoundTrip` |
| SOCKS-INV-02 | Parsing is total: a value whose canonical encoding is the consumed prefix, or a closed-list error | `TestSOCKSInv02_Totality` |
| SOCKS-INV-03 | Every strict prefix of a valid message is `short` with `len(prefix) < Need <= len(message)` | `TestSOCKSInv03_Prefix` |
| SOCKS-INV-04 | Unsupported commands, address types, methods and credentials are refused with the RFC reply; nothing but CONNECT reaches the caller | `TestSOCKSInv04_UnsupportedRefused` |
| SOCKS-INV-05 | Usernames, passwords and domains are 1..255 octets; longer or empty fields do not encode | `TestSOCKSInv05_MaxLengths` |
| SOCKS-INV-06 | Client and server handshakes agree, and bytes after the request stay in the stream | `TestSOCKSInv06_Handshake` |

Conformance data: `conformance/socks5/messages.json` (built byte by byte by
a reference script). Fuzz: `go test -run '^$' -fuzz FuzzSOCKS5 ./socks5/`.
