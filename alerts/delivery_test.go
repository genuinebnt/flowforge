package alerts_test

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/alerts"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

func TestAlrWebhookSignature(t *testing.T) {
	secret := []byte("whsec_test")
	body := []byte(`{"alert_id":"ale_01JD8KBA","rule":"new_admin_host"}`)
	const ts = int64(1794489900)
	m := hmac.New(sha256.New, secret)
	m.Write([]byte("1794489900." + string(body)))
	want := "v1=" + hex.EncodeToString(m.Sum(nil))
	if got := alerts.Sign(secret, ts, body); got != want {
		t.Fatalf("Sign = %s, want %s", got, want)
	}
	now := time.Unix(ts, 0)
	hdr, err := alerts.WebhookHeaders(secret, "dlv_01JD8KBB", now, body)
	if err != nil {
		t.Fatal(err)
	}
	if hdr[alerts.HeaderSignature] != want || hdr[alerts.HeaderTimestamp] != "1794489900" ||
		hdr[alerts.HeaderEvent] != "alert.fired" || hdr[alerts.HeaderDelivery] != "dlv_01JD8KBB" || hdr["Content-Type"] != "application/json" {
		t.Fatalf("headers %v", hdr)
	}
	if _, err := alerts.WebhookHeaders(nil, "d", now, body); !errors.Is(err, alerts.ErrEmptySecret) {
		t.Fatal(err)
	}

	tsh := strconv.FormatInt(ts, 10)
	cases := []struct {
		name string
		ts   string
		sig  string
		body []byte
		now  time.Time
		err  error
	}{
		{"valid", tsh, want, body, now, nil},
		{"valid at +300s", tsh, want, body, now.Add(300 * time.Second), nil},
		{"valid at -300s", tsh, want, body, now.Add(-300 * time.Second), nil},
		{"stale at +301s", tsh, want, body, now.Add(301 * time.Second), alerts.ErrTimestampSkew},
		{"future at -301s", tsh, want, body, now.Add(-301 * time.Second), alerts.ErrTimestampSkew},
		{"tampered body", tsh, want, append([]byte{' '}, body...), now, alerts.ErrSignatureMismatch},
		{"other timestamp", strconv.FormatInt(ts+1, 10), want, body, now, alerts.ErrSignatureMismatch},
		{"rotation list", tsh, "v1=" + hex.EncodeToString(make([]byte, 32)) + ", " + want, body, now, nil},
		{"wrong scheme", tsh, "v0=" + want[3:], body, now, alerts.ErrSignatureMalformed},
		{"not hex", tsh, "v1=zz", body, now, alerts.ErrSignatureMalformed},
		{"empty", tsh, "", body, now, alerts.ErrSignatureMalformed},
		{"bad timestamp", "17944x", want, body, now, alerts.ErrTimestampMalformed},
		{"plus timestamp", "+" + tsh, want, body, now, alerts.ErrTimestampMalformed},
		{"huge negative timestamp", "-9223372036854775808", want, body, now, alerts.ErrTimestampSkew},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := alerts.Verify(secret, c.ts, c.sig, c.body, c.now)
			if c.err == nil && err != nil || c.err != nil && !errors.Is(err, c.err) {
				t.Fatalf("Verify = %v, want %v", err, c.err)
			}
		})
	}
	if err := alerts.Verify(nil, tsh, want, body, now); !errors.Is(err, alerts.ErrEmptySecret) {
		t.Fatal(err)
	}
}

func TestAlrSSRF(t *testing.T) {
	urls := []struct {
		url string
		err error
	}{
		{"https://hooks.example.com/flowforge", nil},
		{"https://hooks.example.com:8443/x", nil},
		{"https://8.8.8.8/hook", nil},
		{"http://hooks.example.com/flowforge", alerts.ErrWebhookScheme},
		{"ftp://hooks.example.com/", alerts.ErrWebhookURL},
		{"https://user:pw@hooks.example.com/", alerts.ErrWebhookURL},
		{"https://127.0.0.1/", alerts.ErrForbiddenAddress},
		{"https://[::1]/", alerts.ErrForbiddenAddress},
		{"https://169.254.169.254/latest/meta-data", alerts.ErrForbiddenAddress},
		{"https://10.0.0.8/", alerts.ErrForbiddenAddress},
		{"https://[::ffff:192.168.0.1]/", alerts.ErrForbiddenAddress},
		{"https://localhost/", alerts.ErrForbiddenHost},
		{"https://api.localhost/", alerts.ErrForbiddenHost},
		{"https://metadata.google.internal/", alerts.ErrForbiddenHost},
		{"https://printer.local/", alerts.ErrForbiddenHost},
		{"https://intranet/", alerts.ErrForbiddenHost},
		{"https://0x7f.0.0.1/", alerts.ErrWebhookURL},
		{"", alerts.ErrWebhookURL},
	}
	for _, c := range urls {
		_, err := alerts.ValidateWebhookURL(c.url)
		if c.err == nil && err != nil || c.err != nil && !errors.Is(err, c.err) {
			t.Errorf("ValidateWebhookURL(%q) = %v, want %v", c.url, err, c.err)
		}
	}

	public := []string{"8.8.8.8", "1.1.1.1", "93.184.215.14", "2606:4700:4700::1111", "2a00:1450:4001:80b::200e", "64:ff9b::808:808", "2002:808:808::1"}
	forbidden := []string{
		"0.0.0.0", "0.1.2.3", "127.0.0.1", "127.255.255.254", "10.1.2.3", "172.16.0.1", "172.31.255.255", "192.168.1.1",
		"169.254.169.254", "100.64.0.1", "100.127.255.255", "192.0.0.8", "192.0.2.1", "198.18.0.1", "198.51.100.7",
		"203.0.113.10", "224.0.0.1", "239.255.255.250", "240.0.0.1", "255.255.255.255",
		"::", "::1", "::ffff:127.0.0.1", "::ffff:10.0.0.1", "::127.0.0.1", "fe80::1", "fc00::1", "fd12:3456::1",
		"ff02::1", "2001:db8::1", "3fff::1", "100::1", "2001::1", "fec0::1", "64:ff9b::7f00:1", "64:ff9b:1::1",
		"2002:a00:1::1", "2002:7f00:1::",
	}
	for _, s := range public {
		if err := alerts.CheckAddr(netip.MustParseAddr(s)); err != nil {
			t.Errorf("%s rejected: %v", s, err)
		}
	}
	for _, s := range forbidden {
		if err := alerts.CheckAddr(netip.MustParseAddr(s)); !errors.Is(err, alerts.ErrForbiddenAddress) {
			t.Errorf("%s accepted", s)
		}
	}
	if !alerts.IsForbiddenAddr(netip.Addr{}) || !alerts.IsForbiddenAddr(netip.MustParseAddr("2606:4700::1%eth0")) {
		t.Error("invalid or zoned address accepted")
	}
	if err := alerts.CheckResolved(nil); !errors.Is(err, alerts.ErrNoResolvedAddresses) {
		t.Errorf("empty resolution: %v", err)
	}
	mixed := []netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("10.0.0.1")}
	if err := alerts.CheckResolved(mixed); !errors.Is(err, alerts.ErrForbiddenAddress) {
		t.Errorf("one private address among public ones must fail: %v", err)
	}
	if err := alerts.CheckResolved(mixed[:1]); err != nil {
		t.Error(err)
	}
}

// maxRng always returns the largest value, so full jitter yields the
// un-jittered backoff.
type maxRng struct{}

func (maxRng) Uint64() uint64 { return ^uint64(0) }

func TestAlrRetrySchedule(t *testing.T) {
	want := []time.Duration{10 * time.Second, 30 * time.Second, 2 * time.Minute, 10 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		if got := alerts.RetryBackoff(i + 1); got != w {
			t.Errorf("RetryBackoff(%d) = %s, want %s", i+1, got, w)
		}
		if got := alerts.RetryDelay(i+1, maxRng{}); got != w {
			t.Errorf("RetryDelay(%d, max) = %s, want %s", i+1, got, w)
		}
	}
	if got := alerts.RetryDelay(1, core.NewSplitMix64(0)); got < alerts.MinRetryDelay {
		t.Errorf("delay below floor: %s", got)
	}
	rng := testkit.Rng(t)
	for range testkit.Cases() {
		n := 1 + core.IntN(rng, 30)
		d := alerts.RetryDelay(n, rng)
		if d < alerts.MinRetryDelay || d > alerts.RetryBackoff(n) {
			t.Fatalf("RetryDelay(%d) = %s outside [1s, %s]", n, d, alerts.RetryBackoff(n))
		}
	}

	// Without jitter the attempts happen at the documented offsets and the
	// delivery gives up once the next attempt would pass 24 h.
	first := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)
	at, attempts := first, 1
	var offsets []time.Duration
	for {
		next, ok := alerts.NextAttempt(attempts, first, at, maxRng{})
		if !ok {
			break
		}
		at = next
		attempts++
		offsets = append(offsets, at.Sub(first))
	}
	wantPrefix := []time.Duration{10 * time.Second, 40 * time.Second, 160 * time.Second, 760 * time.Second, 760*time.Second + time.Hour}
	for i, w := range wantPrefix {
		if offsets[i] != w {
			t.Fatalf("attempt %d at +%s, want +%s", i+2, offsets[i], w)
		}
	}
	if last := offsets[len(offsets)-1]; last > alerts.GiveUpAfter || last+time.Hour <= alerts.GiveUpAfter {
		t.Fatalf("last retry at +%s; want the final one within an hour of the 24 h limit", last)
	}
	if _, ok := alerts.NextAttempt(3, first, first.Add(alerts.GiveUpAfter), testkit.Rng(t)); ok {
		t.Fatal("retry scheduled after 24 h")
	}
}
