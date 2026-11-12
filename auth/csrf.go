package auth

import (
	"crypto/subtle"
	"io"
)

// Console CSRF double-submit names (spec 13.5).
const (
	CSRFHeader = "X-CSRF-Token"
	CSRFCookie = "ff_csrf"
)

// NewCSRFToken returns a per-session CSRF token: 32 bytes from rand,
// base64url.
func NewCSRFToken(rand io.Reader) (string, error) { return newSecret(rand) }

// CSRFRequired reports whether a request needs the double-submit check:
// state-changing methods authenticated by cookies. Requests carrying a
// bearer token are exempt.
func CSRFRequired(method string, bearer bool) bool {
	if bearer {
		return false
	}
	switch method {
	case "GET", "HEAD", "OPTIONS", "TRACE":
		return false
	}
	return true
}

// CheckCSRF compares the X-CSRF-Token header with the ff_csrf cookie in
// constant time.
func CheckCSRF(header, cookie string) error {
	if header == "" || cookie == "" {
		return ErrCSRFMissing
	}
	if subtle.ConstantTimeCompare([]byte(header), []byte(cookie)) != 1 {
		return ErrCSRFMismatch
	}
	return nil
}
