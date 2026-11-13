package dnsmsg

import (
	"net/netip"
	"strconv"
	"strings"
)

// Name limits (RFC 1035 §2.3.4).
const (
	MaxLabelLen = 63
	MaxNameLen  = 255 // octets in wire form, length octets and root included
	// MaxPointerHops bounds the compression pointers one name may follow.
	// Pointers must also point strictly backwards, which alone guarantees
	// termination; the cap bounds work per name.
	MaxPointerHops = 127
)

// nameState is the state of the wire-form name decoder (RFC 1035 §4.1.4).
type nameState uint8

const (
	// stateLength reads a length octet: a label length (00xxxxxx), the
	// first octet of a pointer (11xxxxxx) or the root (0).
	stateLength nameState = iota
	// stateLabel copies the octets of one label.
	stateLabel
	// statePointer reads the second octet of a pointer and jumps.
	statePointer
)

// readName decodes the name at msg[off:]. Before the first pointer the
// name must lie inside msg[:limit] (the record data window for names in
// record data). Every pointer must point strictly before the position of
// the last pointer followed, and after a jump the name may not reach that
// position again, so decoding terminates on any input. It returns the
// canonical presentation text and the offset following the name in the
// original stream.
func readName(msg []byte, off, limit int) (string, int, error) {
	var (
		st      = stateLength
		pos     = off
		end     = limit // reads must stay below end
		next    = -1    // offset after the name once a pointer was followed
		hops    = 0
		wireLen = 0
		label   = 0 // octets left in the current label
		ptrAt   = 0
		text    strings.Builder
	)
	short := func() error {
		if next >= 0 {
			return fail(CodePointerLoop, pos)
		}
		return fail(CodeShortMessage, pos)
	}
	for {
		switch st {
		case stateLength:
			if pos >= end {
				return "", 0, short()
			}
			c := msg[pos]
			switch c & 0xC0 {
			case 0x00:
				if c == 0 {
					wireLen++
					if wireLen > MaxNameLen {
						return "", 0, fail(CodeNameTooLong, pos)
					}
					if next < 0 {
						next = pos + 1
					}
					if text.Len() == 0 {
						return ".", next, nil
					}
					return text.String(), next, nil
				}
				label = int(c)
				wireLen += 1 + label
				if wireLen+1 > MaxNameLen {
					return "", 0, fail(CodeNameTooLong, pos)
				}
				pos++
				st = stateLabel
			case 0xC0:
				ptrAt = pos
				pos++
				st = statePointer
			default:
				return "", 0, fail(CodeBadLabelType, pos)
			}
		case stateLabel:
			if pos+label > end {
				pos = end
				return "", 0, short()
			}
			appendLabelText(&text, msg[pos:pos+label])
			text.WriteByte('.')
			pos += label
			st = stateLength
		case statePointer:
			if pos >= end {
				return "", 0, short()
			}
			target := int(msg[ptrAt]&0x3F)<<8 | int(msg[pos])
			if target >= len(msg) {
				return "", 0, fail(CodePointerOutOfBounds, ptrAt)
			}
			if target >= ptrAt {
				return "", 0, fail(CodePointerLoop, ptrAt)
			}
			hops++
			if hops > MaxPointerHops {
				return "", 0, fail(CodePointerLoop, ptrAt)
			}
			if next < 0 {
				next = pos + 1
			}
			end = ptrAt
			pos = target
			st = stateLength
		}
	}
}

// appendLabelText writes one label in canonical presentation form.
func appendLabelText(b *strings.Builder, label []byte) {
	for _, c := range label {
		switch {
		case c == '.' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x21 || c > 0x7E:
			b.WriteByte('\\')
			b.WriteByte('0' + c/100)
			b.WriteByte('0' + c/10%10)
			b.WriteByte('0' + c%10)
		default:
			b.WriteByte(c)
		}
	}
}

// textState is the state of the presentation-form name parser (RFC 1035
// §5.1).
type textState uint8

const (
	// textLabelStart expects the first character of a label.
	textLabelStart textState = iota
	// textLabel reads label characters.
	textLabel
	// textEscape follows a backslash.
	textEscape
	// textEscapeDigits reads the second and third digit of \DDD.
	textEscapeDigits
)

// appendNameWire appends the uncompressed wire form of the presentation
// name s (relative names are taken as absolute).
func appendNameWire(dst []byte, s string) ([]byte, error) {
	if s == "." {
		return append(dst, 0), nil
	}
	if s == "" {
		return dst, fail(CodeBadNameText, -1)
	}
	start := len(dst)
	lenAt := len(dst)
	dst = append(dst, 0)
	st := textLabelStart
	digits, val := 0, 0
	closeLabel := func() error {
		n := len(dst) - lenAt - 1
		if n == 0 {
			return fail(CodeBadNameText, -1)
		}
		if n > MaxLabelLen {
			return fail(CodeLabelTooLong, -1)
		}
		dst[lenAt] = byte(n)
		return nil
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch st {
		case textLabelStart, textLabel:
			switch c {
			case '.':
				if err := closeLabel(); err != nil {
					return dst, err
				}
				lenAt = len(dst)
				dst = append(dst, 0)
				st = textLabelStart
			case '\\':
				st = textEscape
			default:
				dst = append(dst, c)
				st = textLabel
			}
		case textEscape:
			if c >= '0' && c <= '9' {
				digits, val = 1, int(c-'0')
				st = textEscapeDigits
				continue
			}
			dst = append(dst, c)
			st = textLabel
		case textEscapeDigits:
			if c < '0' || c > '9' {
				return dst, fail(CodeBadNameText, -1)
			}
			digits++
			val = val*10 + int(c-'0')
			if digits == 3 {
				if val > 255 {
					return dst, fail(CodeBadNameText, -1)
				}
				dst = append(dst, byte(val))
				st = textLabel
			}
		}
		// Stop early on huge input; the exact 255-octet check is below
		// (a label is only measured when it closes).
		if len(dst)-start > MaxNameLen+MaxLabelLen+1 {
			return dst, fail(CodeNameTooLong, -1)
		}
	}
	switch st {
	case textEscape, textEscapeDigits:
		return dst, fail(CodeBadNameText, -1)
	case textLabel:
		if err := closeLabel(); err != nil {
			return dst, err
		}
		dst = append(dst, 0)
	case textLabelStart:
		// s ended with '.': the pending label is the root.
		dst = dst[:len(dst)-1]
		dst = append(dst, 0)
	}
	if len(dst)-start > MaxNameLen {
		return dst, fail(CodeNameTooLong, -1)
	}
	return dst, nil
}

// CanonicalName returns the canonical presentation form of s (absolute,
// escapes normalized, case kept) or an error with CodeBadNameText,
// CodeLabelTooLong or CodeNameTooLong.
func CanonicalName(s string) (string, error) {
	w, err := appendNameWire(nil, s)
	if err != nil {
		return "", err
	}
	n, _, err := readName(w, 0, len(w))
	return n, err
}

// EqualNames reports whether a and b name the same domain: canonical forms
// equal after ASCII lowercasing (RFC 4343). Invalid names are never equal.
func EqualNames(a, b string) bool {
	ca, err1 := CanonicalName(a)
	cb, err2 := CanonicalName(b)
	return err1 == nil && err2 == nil && asciiLower(ca) == asciiLower(cb)
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

// ReverseName returns the PTR owner name of a (in-addr.arpa for IPv4,
// nibble format under ip6.arpa for IPv6; RFC 1035 §3.5, RFC 3596 §2.5).
func ReverseName(a netip.Addr) string {
	a = a.Unmap()
	var b strings.Builder
	if a.Is4() {
		v := a.As4()
		for i := 3; i >= 0; i-- {
			b.WriteString(strconv.Itoa(int(v[i])))
			b.WriteByte('.')
		}
		b.WriteString("in-addr.arpa.")
		return b.String()
	}
	const hexd = "0123456789abcdef"
	v := a.As16()
	for i := 15; i >= 0; i-- {
		b.WriteByte(hexd[v[i]&0xF])
		b.WriteByte('.')
		b.WriteByte(hexd[v[i]>>4])
		b.WriteByte('.')
	}
	b.WriteString("ip6.arpa.")
	return b.String()
}

// ParseReverseName is the inverse of ReverseName: it returns the address a
// complete in-addr.arpa or ip6.arpa name denotes (case-insensitive).
func ParseReverseName(name string) (netip.Addr, bool) {
	n := asciiLower(strings.TrimSuffix(name, "."))
	if rest, ok := strings.CutSuffix(n, ".in-addr.arpa"); ok {
		parts := strings.Split(rest, ".")
		if len(parts) != 4 {
			return netip.Addr{}, false
		}
		var v [4]byte
		for i, p := range parts {
			if p == "" || len(p) > 3 || (len(p) > 1 && p[0] == '0') {
				return netip.Addr{}, false
			}
			x, err := strconv.Atoi(p)
			if err != nil || x > 255 {
				return netip.Addr{}, false
			}
			v[3-i] = byte(x)
		}
		return netip.AddrFrom4(v), true
	}
	if rest, ok := strings.CutSuffix(n, ".ip6.arpa"); ok {
		parts := strings.Split(rest, ".")
		if len(parts) != 32 {
			return netip.Addr{}, false
		}
		var v [16]byte
		for i, p := range parts {
			if len(p) != 1 {
				return netip.Addr{}, false
			}
			x := strings.IndexByte("0123456789abcdef", p[0])
			if x < 0 {
				return netip.Addr{}, false
			}
			j := 31 - i
			if j%2 == 0 {
				v[j/2] |= byte(x) << 4
			} else {
				v[j/2] |= byte(x)
			}
		}
		return netip.AddrFrom16(v), true
	}
	return netip.Addr{}, false
}
