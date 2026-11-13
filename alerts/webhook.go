package alerts

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Webhook header names and constants (spec 15.4).
const (
	HeaderEvent     = "X-Flowforge-Event"
	HeaderDelivery  = "X-Flowforge-Delivery"
	HeaderTimestamp = "X-Flowforge-Timestamp"
	HeaderSignature = "X-Flowforge-Signature"

	// EventAlertFired is the X-Flowforge-Event value of alert deliveries.
	EventAlertFired = "alert.fired"
	// SignatureTolerance is the largest accepted |now - timestamp|.
	SignatureTolerance = 300 * time.Second

	signatureScheme = "v1="
)

// Signature verification errors.
var (
	ErrSignatureMalformed = errors.New("alerts: malformed webhook signature header")
	ErrTimestampMalformed = errors.New("alerts: malformed webhook timestamp")
	ErrTimestampSkew      = errors.New("alerts: webhook timestamp outside tolerance")
	ErrSignatureMismatch  = errors.New("alerts: webhook signature mismatch")
	ErrEmptySecret        = errors.New("alerts: empty webhook secret")
)

// Sign returns the X-Flowforge-Signature value
// v1=hex(HMAC-SHA256(secret, timestamp + "." + body)) for a Unix-seconds
// timestamp.
func Sign(secret []byte, timestamp int64, body []byte) string {
	return signatureScheme + hex.EncodeToString(mac(secret, strconv.FormatInt(timestamp, 10), body))
}

func mac(secret []byte, ts string, body []byte) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ts))
	m.Write([]byte{'.'})
	m.Write(body)
	return m.Sum(nil)
}

// WebhookHeaders returns the headers of one signed delivery sent at at.
func WebhookHeaders(secret []byte, deliveryID string, at time.Time, body []byte) (map[string]string, error) {
	if len(secret) == 0 {
		return nil, ErrEmptySecret
	}
	ts := at.Unix()
	return map[string]string{
		"Content-Type":  "application/json",
		HeaderEvent:     EventAlertFired,
		HeaderDelivery:  deliveryID,
		HeaderTimestamp: strconv.FormatInt(ts, 10),
		HeaderSignature: Sign(secret, ts, body),
	}, nil
}

// Verify checks a received delivery the way receivers are told to (spec
// 15.4): the timestamp must be within SignatureTolerance of now and one
// of the comma-separated v1= signatures (several appear during secret
// rotation) must match the raw body. Comparison is constant-time.
func Verify(secret []byte, timestampHeader, signatureHeader string, body []byte, now time.Time) error {
	if len(secret) == 0 {
		return ErrEmptySecret
	}
	tsText := strings.TrimSpace(timestampHeader)
	ts, err := strconv.ParseInt(tsText, 10, 64)
	if err != nil || tsText == "" || tsText[0] == '+' {
		return fmt.Errorf("%w: %q", ErrTimestampMalformed, timestampHeader)
	}
	n, tol := now.Unix(), int64(SignatureTolerance/time.Second)
	if ts < n-tol || ts > n+tol {
		return fmt.Errorf("%w: timestamp %d, now %d", ErrTimestampSkew, ts, n)
	}
	// The MAC covers the timestamp text as received (trimmed), not a
	// re-formatted number, so "0123" is verified as "0123". PORT: keep
	// the received text.
	want := mac(secret, tsText, body)
	found := false
	for part := range strings.SplitSeq(signatureHeader, ",") {
		part = strings.TrimSpace(part)
		hexSig, ok := strings.CutPrefix(part, signatureScheme)
		if !ok {
			continue
		}
		got, err := hex.DecodeString(hexSig)
		if err != nil || len(got) != sha256.Size {
			continue
		}
		found = true
		if hmac.Equal(got, want) {
			return nil
		}
	}
	if !found {
		return ErrSignatureMalformed
	}
	return ErrSignatureMismatch
}
