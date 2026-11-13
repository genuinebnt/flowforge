package core

import (
	"database/sql/driver"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// UUID is a 128-bit identifier. Flowforge generates version 7 UUIDs
// (time-ordered) and renders them in canonical lowercase form (spec 1.2).
type UUID [16]byte

// Nil is the all-zero UUID.
var Nil UUID

// ErrInvalidID reports a malformed UUID or public identifier.
var ErrInvalidID = errors.New("core: invalid identifier")

// NewUUIDv7 builds a version 7 UUID (RFC 9562 5.7) from the clock's wall
// time in Unix milliseconds and 74 random bits drawn from two rng.Uint64
// calls. UUIDs made in the same millisecond are not ordered among
// themselves (no monotonic counter).
//
// PORT: the exact layout is a cross-language contract (docs/PORTING_RUST.md
// 4.1): bytes 0-5 are the 48-bit big-endian milliseconds, bytes 6-7 the
// low 16 bits of the first draw (big-endian), bytes 8-15 the second draw
// (big-endian), then the version and variant bits are overwritten. A
// pre-1970 time wraps through the uint64 conversion; keep the low 48 bits.
func NewUUIDv7(clock Clock, rng Rng) UUID {
	var u UUID
	ms := uint64(clock.Now().UnixMilli())
	u[0] = byte(ms >> 40)
	u[1] = byte(ms >> 32)
	u[2] = byte(ms >> 24)
	u[3] = byte(ms >> 16)
	u[4] = byte(ms >> 8)
	u[5] = byte(ms)
	binary.BigEndian.PutUint16(u[6:8], uint16(rng.Uint64()))
	binary.BigEndian.PutUint64(u[8:16], rng.Uint64())
	u[6] = (u[6] & 0x0f) | 0x70 // version 7
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return u
}

// String returns the canonical 8-4-4-4-12 lowercase hex form.
func (u UUID) String() string {
	var b [36]byte
	hex.Encode(b[0:8], u[0:4])
	b[8] = '-'
	hex.Encode(b[9:13], u[4:6])
	b[13] = '-'
	hex.Encode(b[14:18], u[6:8])
	b[18] = '-'
	hex.Encode(b[19:23], u[8:10])
	b[23] = '-'
	hex.Encode(b[24:36], u[10:16])
	return string(b[:])
}

// IsZero reports whether u is the nil UUID.
func (u UUID) IsZero() bool { return u == Nil }

// ParseUUID parses the canonical 8-4-4-4-12 hyphenated form; hex digits
// may be upper or lower case. Braced, URN and unhyphenated forms are
// rejected with ErrInvalidID. Any version and variant are accepted.
func ParseUUID(s string) (UUID, error) {
	var u UUID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return u, fmt.Errorf("%w: %q", ErrInvalidID, s)
	}
	h := s[0:8] + s[9:13] + s[14:18] + s[19:23] + s[24:36]
	if _, err := hex.Decode(u[:], []byte(h)); err != nil {
		return Nil, fmt.Errorf("%w: %q", ErrInvalidID, s)
	}
	return u, nil
}

// MustParseUUID is ParseUUID for constants in tests and fixtures; it
// panics on malformed input.
func MustParseUUID(s string) UUID {
	u, err := ParseUUID(s)
	if err != nil {
		panic(err)
	}
	return u
}

// MarshalText implements encoding.TextMarshaler.
func (u UUID) MarshalText() ([]byte, error) { return []byte(u.String()), nil }

// UnmarshalText implements encoding.TextUnmarshaler.
func (u *UUID) UnmarshalText(b []byte) error {
	p, err := ParseUUID(string(b))
	if err != nil {
		return err
	}
	*u = p
	return nil
}

// Value implements driver.Valuer so a UUID can be passed to SQL drivers.
func (u UUID) Value() (driver.Value, error) { return u.String(), nil }

// Scan implements sql.Scanner. It accepts text (string or []byte of the
// hyphenated form), 16 raw bytes, a [16]byte, and NULL, which scans as
// Nil.
func (u *UUID) Scan(src any) error {
	switch v := src.(type) {
	case string:
		return u.UnmarshalText([]byte(v))
	case []byte:
		if len(v) == 16 {
			copy(u[:], v)
			return nil
		}
		return u.UnmarshalText(v)
	case [16]byte:
		*u = v
		return nil
	case nil:
		*u = Nil
		return nil
	}
	return fmt.Errorf("%w: cannot scan %T", ErrInvalidID, src)
}

// Prefix is the type tag carried by public identifiers in REST (spec 1.2).
type Prefix string

// Public identifier prefixes (spec 1.2 lists run_, prg_, ent_, apr_, usr_,
// ten_ and wf_; the others are Flowforge additions for the remaining REST
// resources). A prefix never contains "_".
const (
	PrefixRun             Prefix = "run"
	PrefixProgram         Prefix = "prg"
	PrefixEntity          Prefix = "ent"
	PrefixEdge            Prefix = "edg"
	PrefixTag             Prefix = "tag"
	PrefixApproval        Prefix = "apr"
	PrefixUser            Prefix = "usr"
	PrefixTenant          Prefix = "ten"
	PrefixWorkflow        Prefix = "wf"
	PrefixWorkflowVersion Prefix = "wfv"
	PrefixObservation     Prefix = "obs"
	PrefixDecision        Prefix = "dec"
	PrefixAPIKey          Prefix = "key"
	PrefixAlertRule       Prefix = "alr"
	PrefixAlertEvent      Prefix = "ale"
	PrefixChange          Prefix = "chg"
	PrefixDelivery        Prefix = "dlv"
	PrefixSession         Prefix = "ses"
	PrefixRequest         Prefix = "req"
	PrefixPlan            Prefix = "pln"
	PrefixChannel         Prefix = "chn"
	PrefixSchedule        Prefix = "sch" // workflow schedules (G-OPS-10; not in spec 1.2)
)

// crockford is the Crockford base32 alphabet used for the textual part of
// public identifiers (the same alphabet as ULID).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// PublicID renders u as "<prefix>_<26 Crockford base32 chars>", most
// significant bits first and upper case. Because UUIDv7 is time-ordered
// and the encoding preserves byte order, public ids of one prefix sort in
// creation order to millisecond granularity.
func PublicID(p Prefix, u UUID) string {
	var out [26]byte
	// 128 bits -> 26 chars: the first char carries the top 3 bits.
	hi := binary.BigEndian.Uint64(u[0:8])
	lo := binary.BigEndian.Uint64(u[8:16])
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&0x1f]
		lo = (lo >> 5) | (hi << 59)
		hi >>= 5
	}
	return string(p) + "_" + string(out[:])
}

// ParsePublicID parses an identifier produced by PublicID and checks its
// prefix (case-sensitive). The base32 body is case-insensitive and its
// first character must be 0-7, since it carries only 3 bits. Any 36-byte
// input is parsed as a bare canonical UUID instead, without a prefix
// check. Errors wrap ErrInvalidID.
//
// PORT: unlike full Crockford decoding, the ambiguous letters I, L and O
// are rejected rather than read as 1, 1 and 0, and hyphens are not
// skipped.
func ParsePublicID(want Prefix, s string) (UUID, error) {
	if len(s) == 36 {
		return ParseUUID(s)
	}
	p, body, ok := strings.Cut(s, "_")
	if !ok || Prefix(p) != want || len(body) != 26 {
		return Nil, fmt.Errorf("%w: %q is not a %s_ identifier", ErrInvalidID, s, want)
	}
	var hi, lo uint64
	for i := 0; i < 26; i++ {
		c := body[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		v := strings.IndexByte(crockford, c)
		if v < 0 || (i == 0 && v > 7) {
			return Nil, fmt.Errorf("%w: %q", ErrInvalidID, s)
		}
		hi = (hi << 5) | (lo >> 59)
		lo = (lo << 5) | uint64(v)
	}
	var u UUID
	binary.BigEndian.PutUint64(u[0:8], hi)
	binary.BigEndian.PutUint64(u[8:16], lo)
	return u, nil
}

// IDGen produces UUIDv7 values from injected ports. It is safe for
// concurrent use only if the Rng is (SystemRng is; SplitMix64 is not).
type IDGen struct {
	Clock Clock
	Rng   Rng
}

// New returns a fresh UUIDv7.
func (g IDGen) New() UUID { return NewUUIDv7(g.Clock, g.Rng) }
