# dnsmsg

Layer-1 DNS wire format (RFC 1035 §4, RFC 3596, RFC 2782, RFC 6891, RFC
8659): `Decode` (strict and lenient), canonical `Encode`, `EncodeTruncated`
(TC handling, RFC 2181 §9), TCP framing (`AppendTCP`, `WriteTCP`,
`ReadTCP`), EDNS(0) helpers and presentation-form names (`CanonicalName`,
`EqualNames`, `ReverseName`, `ParseReverseName`). Used by the egress
proxy's DNS forwarder and the tools that query through it (V2_DESIGN §9).

Records with structured data: A, AAAA, NS, CNAME, PTR, MX, TXT, SOA, SRV,
CAA, OPT. Every other type (and, in lenient mode, malformed data of a
known type) is `KindRaw`. `RData` is a tagged struct (`Kind` + fields), so
it maps to a Rust enum.

* **Names** are canonical presentation text: absolute, `\.` and `\\`
  escaped, octets outside 0x21-0x7E as `\DDD`, case kept. The decoder is an
  explicit state machine (`stateLength`, `stateLabel`, `statePointer`).
  Compression pointers must point strictly before the last pointer
  followed, and after a jump the name may not reach that pointer again, so
  every cycle is `pointer_loop`; at most 127 hops per name.
* **Encoding** compresses owner, question, NS/CNAME/PTR/MX/SOA names
  against the first earlier occurrence of the longest suffix (exact octets);
  SRV targets are never compressed.
* **Strict vs lenient:** strict rejects trailing octets, malformed known
  data (`bad_rdata`, `rdlength_mismatch`), OPT outside the additional
  section / non-root / duplicated (`bad_opt`) and TXT without strings;
  lenient keeps them (raw data or as-is) and returns `Diagnostic`s.

Error codes (closed list): `short_message`, `label_too_long`,
`name_too_long`, `bad_label_type`, `pointer_out_of_bounds`,
`pointer_loop`, `bad_rdata`, `rdlength_mismatch`, `trailing_data`,
`bad_opt`, `bad_name_text`, `string_too_long`, `rdata_kind_mismatch`,
`message_too_large`, `too_many_records`.

## Invariants

| Id | Statement | Test |
|----|-----------|------|
| DNS-INV-01 | `Decode(Encode(m)) == m` for generated canonical messages, both modes, no diagnostics | `TestDNSInv01_RoundTrip` |
| DNS-INV-02 | Compression pointers never loop: self, forward, backward-cyclic and over-long chains end in `pointer_loop`; pointer-dense random input terminates | `TestDNSInv02_PointersTerminate` |
| DNS-INV-03 | Every decoded name is at most 255 octets in wire form with labels of at most 63, and is canonical | `TestDNSInv03_NameBounds` |
| DNS-INV-04 | Decoding is total: any bytes give a message or an error / diagnostics from the closed list | `TestDNSInv04_Totality` |
| DNS-INV-05 | Encoding is canonical: equal messages give equal bytes, and decode-then-encode reproduces them | `TestDNSInv05_CanonicalEncode` |
| DNS-INV-06 | Strict idempotence: whatever decodes strictly re-encodes to bytes that decode to the same message | `TestDNSInv06_StrictIdempotence` |
| DNS-INV-07 | `EncodeTruncated` fits the limit, sets TC exactly when records were dropped, keeps a prefix of the answers and the OPT record, and decodes strictly | `TestDNSInv07_Truncation` |
| DNS-INV-08 | TCP framing round-trips and refuses frames above the limit | `TestDNSInv08_TCPFraming` |
| DNS-INV-09 | Strict success implies the same lenient result with no diagnostics | `TestDNSInv09_LenientSuperset` |
| DNS-INV-10 | Presentation names: `CanonicalName` is idempotent, keeps case, escapes round-trip; `ParseReverseName(ReverseName(a)) == a` | `TestDNSInv10_NameText` |

Conformance data: `conformance/dnsmsg/roundtrip/` (hex ↔ message JSON,
encoded by an independent reference encoder, so `Encode` must match it
byte for byte) and `conformance/dnsmsg/decode/` (malformed and edge cases
with their codes and lenient diagnostics).

```sh
go test -race ./dnsmsg/
go test -run '^$' -fuzz FuzzDNSDecode -fuzztime 60s ./dnsmsg/
go test -run '^$' -bench . ./dnsmsg/
```

Limitations: no DNSSEC record types beyond raw data, no name
decompression for unknown types (RFC 3597 forbids it), no message
signing (TSIG).
