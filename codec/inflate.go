package codec

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"slices"

	"github.com/klauspost/compress/zstd"
)

// Kind names an encoding or compression format found by Sniff and decoded
// by DecodeLayers.
type Kind uint8

const (
	// KindBase64 is base64 with the standard alphabet (or no
	// alphabet-specific character).
	KindBase64 Kind = iota + 1
	// KindBase64URL is base64 with the URL alphabet ('-' or '_' seen).
	KindBase64URL
	// KindHex is base16.
	KindHex
	// KindGzip is a gzip file (RFC 1952), one or more members.
	KindGzip
	// KindZlib is a zlib stream (RFC 1950).
	KindZlib
	// KindDeflate is a raw DEFLATE stream (RFC 1951). It has no magic, so
	// Sniff never reports it.
	KindDeflate
	// KindZstd is a Zstandard stream (RFC 8878): one or more frames,
	// skippable frames included.
	KindZstd
)

var kindNames = [...]string{KindBase64: "base64", KindBase64URL: "base64url", KindHex: "hex", KindGzip: "gzip",
	KindZlib: "zlib", KindDeflate: "deflate", KindZstd: "zstd"}

// String returns the stable name ("base64", "gzip", ...).
func (k Kind) String() string {
	if k == 0 || int(k) >= len(kindNames) {
		return "unknown"
	}
	return kindNames[k]
}

// ParseKind maps a name to its Kind.
func ParseKind(name string) (Kind, bool) {
	for i, n := range kindNames {
		if n != "" && n == name {
			return Kind(i), true
		}
	}
	return 0, false
}

// IsCompression reports a compressed-stream kind (Inflate's domain).
func (k Kind) IsCompression() bool {
	return k == KindGzip || k == KindZlib || k == KindDeflate || k == KindZstd
}

// Inflate defaults.
const (
	// DefaultMaxOutput is the default output cap: 8 MiB, the content
	// pipeline's response bound (V2_DESIGN §12.10).
	DefaultMaxOutput = 8 << 20
	// DefaultMaxRatio is the default bomb guard: output may not exceed
	// 100 times the compressed size once past RatioFloor. Real text
	// (HTML, JS, JSON, sitemaps) compresses 3-30x; DEFLATE peaks near
	// 1032x and zstd far higher on crafted input.
	DefaultMaxRatio = 100
	// DefaultRatioFloor is the output size below which the ratio guard
	// never trips: 1 MiB, so tiny but highly compressible inputs still
	// decode.
	DefaultRatioFloor = 1 << 20
	// zstdMinWindow is the smallest zstd window accepted whatever the
	// cap: frames written by common encoders declare up to 8 MiB.
	zstdMinWindow = 8 << 20
)

// InflateOptions bounds a decompression. Zero fields take the defaults.
type InflateOptions struct {
	// MaxOutput caps the output in bytes (default DefaultMaxOutput).
	MaxOutput int
	// MaxRatio caps output/input once the output exceeds RatioFloor
	// (default DefaultMaxRatio); negative disables the ratio guard.
	MaxRatio int
	// RatioFloor is the output size the ratio guard allows regardless of
	// the input size (default DefaultRatioFloor).
	RatioFloor int
}

// limit returns the effective cap for an input of n bytes and the code a
// stop at that cap reports.
func (o InflateOptions) limit(n int) (int, Code) {
	maxOut := o.MaxOutput
	if maxOut <= 0 {
		maxOut = DefaultMaxOutput
	}
	ratio, floor := o.MaxRatio, o.RatioFloor
	if ratio == 0 {
		ratio = DefaultMaxRatio
	}
	if floor <= 0 {
		floor = DefaultRatioFloor
	}
	if ratio < 0 {
		return maxOut, CodeOutputLimit
	}
	byRatio := floor
	if n > 0 && n <= (1<<62)/ratio && n*ratio > floor {
		byRatio = n * ratio
	}
	if byRatio < maxOut {
		return byRatio, CodeRatioLimit
	}
	return maxOut, CodeOutputLimit
}

// DetectCompression reports the compression format whose magic starts b:
// gzip 1F 8B 08 (RFC 1952 §2.3.1, CM = deflate), zlib 78 xx with FDICT
// clear and a valid FCHECK (RFC 1950 §2.2, CM = 8, 32 KiB window), zstd
// 28 B5 2F FD (RFC 8878 §3.1.1).
func DetectCompression(b []byte) (Kind, bool) {
	switch {
	case len(b) >= 3 && b[0] == 0x1F && b[1] == 0x8B && b[2] == 8:
		return KindGzip, true
	case len(b) >= 4 && b[0] == 0x28 && b[1] == 0xB5 && b[2] == 0x2F && b[3] == 0xFD:
		return KindZstd, true
	case len(b) >= 2 && b[0] == 0x78 && b[1]&0x20 == 0 && (uint16(b[0])<<8|uint16(b[1]))%31 == 0:
		return KindZlib, true
	}
	return 0, false
}

// Inflate decompresses src of kind k (gzip, zlib, deflate or zstd) and
// never produces more than the cap of o: min(MaxOutput, max(RatioFloor,
// MaxRatio × len(src))). At the cap it stops reading and returns exactly
// the first cap bytes of the output with ErrOutputLimit (CodeOutputLimit
// or CodeRatioLimit). A stream that fails returns the output decoded
// before the failure with ErrCorrupt (CodeBadHeader before any output can
// exist, CodeCorrupt after).
//
// Trailing data: after the last gzip member it is an error (a gzip file
// is a sequence of members, RFC 1952 §2.2); after the zlib trailer or the
// final DEFLATE block it is ignored; zstd accepts only further frames.
func Inflate(src []byte, k Kind, o InflateOptions) ([]byte, error) {
	return AppendInflate(nil, src, k, o)
}

// AppendInflate appends the decompression of src to dst (see Inflate).
// The cap applies to the bytes appended.
func AppendInflate(dst, src []byte, k Kind, o InflateOptions) ([]byte, error) {
	lim, limCode := o.limit(len(src))
	var r io.Reader
	var closeFn func()
	br := bytes.NewReader(src)
	switch k {
	case KindGzip:
		zr, err := gzip.NewReader(br)
		if err != nil {
			return dst, errAt(CodeBadHeader, 0)
		}
		r = zr
	case KindZlib:
		zr, err := zlib.NewReader(br)
		if errors.Is(err, zlib.ErrDictionary) {
			return dst, errAt(CodeUnsupported, 0)
		}
		if err != nil {
			return dst, errAt(CodeBadHeader, 0)
		}
		r = zr
	case KindDeflate:
		r = flate.NewReader(br)
	case KindZstd:
		if len(src) < 4 {
			return dst, errAt(CodeBadHeader, 0)
		}
		// PORT: concurrency 1 makes klauspost's stream decoder
		// synchronous (no goroutines, V2_DESIGN §1.5). The window bound
		// keeps memory proportional to the cap; a frame that declares a
		// larger window stops with the cap's code, as its output could
		// exceed it. Rust: zstd::stream::Decoder with window_log_max.
		win := uint64(max(lim, zstdMinWindow))
		zr, err := zstd.NewReader(br, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxWindow(win),
			zstd.WithDecoderMaxMemory(win), zstd.WithDecoderLowmem(true))
		if err != nil {
			return dst, errAt(CodeBadHeader, 0)
		}
		r, closeFn = zr, zr.Close
	default:
		return dst, errAt(CodeUnsupported, 0)
	}
	if closeFn != nil {
		defer closeFn()
	}
	start := len(dst)
	buf := dst
	// Read at most lim+1 bytes: one byte past the cap proves the output
	// is longer without decoding further.
	for {
		if len(buf) == cap(buf) {
			buf = slices.Grow(buf, min(max(cap(buf)-start, 4<<10), 1<<20))
		}
		want := min(cap(buf)-len(buf), lim+1-(len(buf)-start))
		n, err := r.Read(buf[len(buf) : len(buf)+want])
		buf = buf[:len(buf)+n]
		if len(buf)-start > lim {
			return buf[:start+lim], errAt(limCode, lim)
		}
		if err == io.EOF {
			return buf, nil
		}
		if err != nil {
			return buf, streamErr(k, err, len(buf)-start)
		}
	}
}

// streamErr maps a decompressor error to a code. Header errors of later
// gzip members and of zstd frames surface during reading and are
// CodeCorrupt unless nothing was decoded yet.
func streamErr(k Kind, err error, produced int) error {
	if k == KindZstd && (errors.Is(err, zstd.ErrWindowSizeExceeded) || errors.Is(err, zstd.ErrDecoderSizeExceeded)) {
		return errAt(CodeOutputLimit, produced)
	}
	if produced == 0 && (k == KindZstd && errors.Is(err, zstd.ErrMagicMismatch)) {
		return errAt(CodeBadHeader, 0)
	}
	return errAt(CodeCorrupt, produced)
}
