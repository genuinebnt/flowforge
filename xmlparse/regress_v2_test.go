package xmlparse

import "testing"

// TestRegress_XMLUTF16PartialCharAtEOF: UTF-16 input that ends inside a
// character (an odd final byte, or a high surrogate without its pair) was
// transcoded with the partial character silently dropped, so a document
// whose root had closed parsed as GradeComplete although the input is cut.
// Like a UTF-8 sequence cut by the end, it is an EOF condition:
// unexpected_eof at the end of the transcoded buffer (or no_root_element
// in the prolog).
func TestRegress_XMLUTF16PartialCharAtEOF(t *testing.T) {
	utf16LE := func(s string, tail ...byte) []byte {
		b := []byte{0xFF, 0xFE}
		for _, r := range s {
			b = append(b, byte(r), byte(r>>8))
		}
		return append(b, tail...)
	}
	utf16BE := func(s string, tail ...byte) []byte {
		b := []byte{0xFE, 0xFF}
		for _, r := range s {
			b = append(b, byte(r>>8), byte(r))
		}
		return append(b, tail...)
	}
	cases := []struct {
		name string
		in   []byte
		code ErrorCode
		off  int
	}{
		{"le_high_surrogate_after_root", utf16LE("<a/>", 0x00, 0xD8), ErrUnexpectedEOF, 4},
		{"be_high_surrogate_after_root", utf16BE("<a/>", 0xD8, 0x00), ErrUnexpectedEOF, 4},
		{"le_odd_byte_after_root", utf16LE("<a/>", 0x20), ErrUnexpectedEOF, 4},
		{"le_surrogate_and_odd_byte", utf16LE("<a/>", 0x00, 0xD8, 0x00), ErrUnexpectedEOF, 4},
		{"le_odd_byte_in_prolog", utf16LE("", 0x3C), ErrNoRootElement, 0},
		{"le_odd_byte_inside_root", utf16LE("<a>", 0x3C), ErrUnexpectedEOF, 3},
	}
	for _, c := range cases {
		_, err := Parse(c.in, Options{})
		se, ok := err.(*SyntaxError)
		if !ok || se.Code != c.code || se.Offset != c.off {
			t.Errorf("%s: got %v, want %s at %d", c.name, err, c.code, c.off)
		}
		if GradeOf(err) == GradeComplete {
			t.Errorf("%s: graded complete", c.name)
		}
	}
	// A whole document is still complete.
	if _, err := Parse(utf16LE("<a/>"), Options{}); err != nil {
		t.Fatalf("whole UTF-16 document: %v", err)
	}
}
