package wellknown

import (
	"bytes"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// SecurityOptions configure ParseSecurityTxt. The zero value uses the
// defaults.
type SecurityOptions struct {
	// MaxBytes bounds the bytes parsed (0 = DefaultMaxBytes).
	MaxBytes int
	// MaxItems bounds the fields kept (0 = DefaultMaxItems).
	MaxItems int
}

// SecurityField is one field line as written (name in its original case).
type SecurityField struct {
	Name, Value string
	Line        int
}

// SecurityCode is a stable diagnostic code (conformance/wellknown/
// securitytxt.json holds the names and levels); IsWarning gives the level.
type SecurityCode uint8

// security.txt diagnostic codes; sections are RFC 9116.
const (
	SecurityNone                  SecurityCode = iota
	SecurityMissingContact                     // §2.5.3: at least one Contact is required (error)
	SecurityMissingExpires                     // §2.5.5: Expires is required (error)
	SecurityMultipleExpires                    // §2.5.5: Expires must appear once (error; the first is used)
	SecurityInvalidExpires                     // §2.5.5: not an RFC 3339 date-time (error)
	SecurityExpired                            // §2.5.5: Expires is not after the time given (error)
	SecurityExpiresTooFar                      // §2.5.5: more than a year ahead (warning)
	SecurityMultiplePreferredLang              // §2.5.8: Preferred-Languages must appear at most once (error)
	SecurityInvalidURI                         // a URI-valued field that is not an absolute URI (error)
	SecurityInsecureURI                        // a web URI that is not https:// (error)
	SecurityInvalidLine                        // a line that is neither a field, a comment nor blank (error)
	SecurityInvalidUTF8                        // the file must be UTF-8 text (error; the line is skipped)
	SecurityUnterminatedSignature              // a signed message without a complete signature block (warning)
	SecurityContentAfterSignature              // lines after the signature are not signed (warning; ignored)
	SecurityInputTooLarge                      // content past MaxBytes was ignored
	SecurityLimitExceeded                      // fields past MaxItems were ignored
)

var securityCodeNames = [...]string{
	"none", "missing_contact", "missing_expires", "multiple_expires",
	"invalid_expires", "expired", "expires_too_far", "multiple_preferred_languages",
	"invalid_uri", "insecure_uri", "invalid_line", "invalid_utf8",
	"unterminated_signature", "content_after_signature", "input_too_large",
	"limit_exceeded",
}

// String returns the stable name of c.
func (c SecurityCode) String() string {
	if int(c) < len(securityCodeNames) {
		return securityCodeNames[c]
	}
	return "code(" + strconv.Itoa(int(c)) + ")"
}

// IsWarning reports whether c does not make the file invalid.
func (c SecurityCode) IsWarning() bool {
	switch c {
	case SecurityExpiresTooFar, SecurityUnterminatedSignature, SecurityContentAfterSignature,
		SecurityInputTooLarge, SecurityLimitExceeded:
		return true
	}
	return false
}

// SecurityDiag is one diagnostic with its 1-based line number (0 for
// file-level findings).
type SecurityDiag struct {
	Code SecurityCode
	Line int
}

// SecurityTxt is a parsed security.txt (RFC 9116).
type SecurityTxt struct {
	Contact []string // §2.5.3, in file order (order is preference)
	// Expires is the first Expires field in UTC; HasExpires is false when
	// it is missing or not a valid RFC 3339 date-time.
	Expires            time.Time
	HasExpires         bool
	Encryption         []string // §2.5.4
	Acknowledgments    []string // §2.5.1
	PreferredLanguages []string // §2.5.8, the tags of the single field
	Canonical          []string // §2.5.2
	Policy             []string // §2.5.7
	Hiring             []string // §2.5.6
	CSAF               []string // CSAF 2.0 §7.1.8 registered extension
	// Fields holds every field line in order, known or not (§2.4:
	// unknown fields are allowed).
	Fields []SecurityField
	// Signed is set when the file is an OpenPGP cleartext signed message
	// (§2.3); the signature is not verified.
	Signed bool
	// Diags: a byte-limit diagnostic first (Line 0), then line
	// diagnostics in line order, then the file-level findings (Line 0,
	// except expired and expires_too_far, which point at Expires).
	Diags []SecurityDiag
	// Grade is GradePartial when MaxBytes or MaxItems cut the read.
	Grade Grade
}

// Valid reports whether the file meets RFC 9116's requirements (no
// error-level diagnostic).
func (s *SecurityTxt) Valid() bool {
	for _, d := range s.Diags {
		if !d.Code.IsWarning() {
			return false
		}
	}
	return true
}

// CanonicalMatches reports whether u is one of the Canonical URIs (§2.5.2:
// a file found at a URI not listed as canonical should be treated with
// suspicion).
func (s *SecurityTxt) CanonicalMatches(u string) bool {
	for _, c := range s.Canonical {
		if c == u {
			return true
		}
	}
	return false
}

// pgpState is the OpenPGP cleartext framing state (RFC 4880 §7, which
// RFC 9116 §2.3 references).
type pgpState uint8

const (
	// pgpStart: before any field; a "BEGIN PGP SIGNED MESSAGE" line here
	// switches to the armor headers.
	pgpStart pgpState = iota
	// pgpArmorHeaders: "Hash:" headers up to the first blank line.
	pgpArmorHeaders
	// pgpBody: the (dash-escaped, when signed) field lines.
	pgpBody
	// pgpSignature: inside BEGIN/END PGP SIGNATURE; content is skipped.
	pgpSignature
	// pgpTrailer: after END PGP SIGNATURE.
	pgpTrailer
)

// Armor lines of RFC 4880 §7, compared after trimming WSP.
const (
	pgpBeginMessage   = "-----BEGIN PGP SIGNED MESSAGE-----"
	pgpBeginSignature = "-----BEGIN PGP SIGNATURE-----"
	pgpEndSignature   = "-----END PGP SIGNATURE-----"
)

// ParseSecurityTxt parses a security.txt body and checks it against now
// (Expires must lie after now; the clock is never read). It never fails:
// problems are diagnostics, and Valid tells whether the file conforms. A
// field whose value fails its URI check is kept in Fields but not in the
// typed list.
func ParseSecurityTxt(data []byte, now time.Time, opts SecurityOptions) *SecurityTxt {
	maxBytes, maxItems := opts.MaxBytes, opts.MaxItems
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	if maxItems <= 0 {
		maxItems = DefaultMaxItems
	}
	s := &SecurityTxt{}
	diag := func(c SecurityCode, n int) { s.Diags = append(s.Diags, SecurityDiag{Code: c, Line: n}) }
	if len(data) > maxBytes {
		cut := data[:maxBytes]
		if i := bytes.LastIndexAny(cut, "\r\n"); i >= 0 {
			cut = cut[:i+1]
		} else {
			cut = nil
		}
		data = cut
		diag(SecurityInputTooLarge, 0)
		s.Grade = GradePartial
	}
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	st := pgpStart
	expiresLine, langSeen := 0, false
	splitLines(data, func(raw []byte, n, _ int) bool {
		line := string(trimWS(raw))
		switch st {
		case pgpStart:
			if line == pgpBeginMessage {
				s.Signed = true
				st = pgpArmorHeaders
				return true
			}
			st = pgpBody
		case pgpArmorHeaders:
			if line == "" {
				st = pgpBody
			}
			return true
		case pgpSignature:
			if line == pgpEndSignature {
				st = pgpTrailer
			}
			return true
		case pgpTrailer:
			if line != "" {
				diag(SecurityContentAfterSignature, n)
			}
			return true
		}
		// pgpBody.
		if s.Signed {
			if line == pgpBeginSignature {
				st = pgpSignature
				return true
			}
			// RFC 4880 §7.1: dash-escaped lines start with "- ".
			line = strings.TrimPrefix(line, "- ")
		}
		if line == "" || line[0] == '#' {
			return true
		}
		if !utf8.ValidString(line) {
			diag(SecurityInvalidUTF8, n)
			return true
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !validFieldName(name) {
			diag(SecurityInvalidLine, n)
			return true
		}
		value = strings.TrimSpace(value)
		if len(s.Fields) >= maxItems {
			diag(SecurityLimitExceeded, n)
			s.Grade = GradePartial
			return false
		}
		s.Fields = append(s.Fields, SecurityField{Name: name, Value: value, Line: n})
		switch strings.ToLower(name) {
		case "contact":
			if checkURI(value, false, n, diag) {
				s.Contact = append(s.Contact, value)
			}
		case "expires":
			if s.HasExpires || expiresLine > 0 {
				diag(SecurityMultipleExpires, n)
				return true
			}
			expiresLine = n
			// PORT: time.RFC3339 parsing also accepts fractional seconds,
			// which RFC 3339 §5.6 allows; the Rust port must too.
			t, err := time.Parse(time.RFC3339, value)
			if err != nil {
				diag(SecurityInvalidExpires, n)
				return true
			}
			s.Expires, s.HasExpires = t.UTC(), true
		case "encryption":
			if checkURI(value, false, n, diag) {
				s.Encryption = append(s.Encryption, value)
			}
		case "acknowledgments":
			if checkURI(value, true, n, diag) {
				s.Acknowledgments = append(s.Acknowledgments, value)
			}
		case "canonical":
			if checkURI(value, true, n, diag) {
				s.Canonical = append(s.Canonical, value)
			}
		case "policy":
			if checkURI(value, true, n, diag) {
				s.Policy = append(s.Policy, value)
			}
		case "hiring":
			if checkURI(value, true, n, diag) {
				s.Hiring = append(s.Hiring, value)
			}
		case "csaf":
			if checkURI(value, true, n, diag) {
				s.CSAF = append(s.CSAF, value)
			}
		case "preferred-languages":
			if langSeen {
				diag(SecurityMultiplePreferredLang, n)
				return true
			}
			langSeen = true
			for _, tag := range strings.Split(value, ",") {
				if tag = strings.TrimSpace(tag); tag != "" {
					s.PreferredLanguages = append(s.PreferredLanguages, tag)
				}
			}
		}
		return true
	})
	if s.Signed && st != pgpTrailer {
		diag(SecurityUnterminatedSignature, 0)
	}
	if len(s.Contact) == 0 {
		diag(SecurityMissingContact, 0)
	}
	switch {
	case expiresLine == 0:
		diag(SecurityMissingExpires, 0)
	case s.HasExpires && !s.Expires.After(now):
		diag(SecurityExpired, expiresLine)
	case s.HasExpires && s.Expires.After(now.AddDate(1, 0, 0)):
		diag(SecurityExpiresTooFar, expiresLine)
	}
	return s
}

// validFieldName is RFC 9116 §2.4 / RFC 5322 field-name: one or more
// printable US-ASCII characters other than ":" (and no spaces).
func validFieldName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c <= 0x20 || c >= 0x7f {
			return false
		}
	}
	return true
}

// checkURI validates a URI-valued field: an absolute URI without white
// space, and https with a host for web URIs (§2.5: "MUST begin with
// https://"); http is insecure_uri. webOnly fields (all but Contact and
// Encryption, which also take mailto:, tel:, dns: and similar) must be
// https URIs; another scheme there is invalid_uri. It reports through diag
// and returns whether the value is kept.
func checkURI(v string, webOnly bool, n int, diag func(SecurityCode, int)) bool {
	u, err := url.Parse(v)
	if err != nil || u.Scheme == "" || strings.ContainsAny(v, " \t") {
		diag(SecurityInvalidURI, n)
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	switch {
	case scheme == "http":
		diag(SecurityInsecureURI, n)
		return false
	case scheme == "https":
		if u.Host == "" {
			diag(SecurityInvalidURI, n)
			return false
		}
	case webOnly:
		diag(SecurityInvalidURI, n)
		return false
	}
	return true
}
