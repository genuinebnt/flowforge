package auth_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

func TestRefreshTokenFormat(t *testing.T) {
	tok, err := auth.NewRefreshToken(newReader(1))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "ffr_") || len(tok) != 4+43 || !auth.IsRefreshTokenFormat(tok) {
		t.Fatalf("refresh token %q", tok)
	}
	for _, bad := range []string{"", "ffr_", tok + "A", "ffk_" + tok[4:], tok[:46] + "="} {
		if auth.IsRefreshTokenFormat(bad) {
			t.Errorf("IsRefreshTokenFormat(%q) = true", bad)
		}
	}
	if _, err := auth.NewRefreshToken(strings.NewReader("short")); err == nil {
		t.Fatal("NewRefreshToken accepted a short random source")
	}
}

func TestAPIKey(t *testing.T) {
	k, err := auth.NewAPIKey(newReader(2), "01JD8K0A9Z")
	if err != nil {
		t.Fatal(err)
	}
	if k.Prefix != "ffk_01JD8K0A9Z" || !strings.HasPrefix(k.Token, k.Prefix+"_") || len(k.Token) != len(k.Prefix)+1+43 {
		t.Fatalf("API key %+v", k)
	}
	p, err := auth.ParseAPIKey(k.Token)
	if err != nil || p.ID != k.ID || p.Prefix != k.Prefix {
		t.Fatalf("ParseAPIKey = %+v, %v", p, err)
	}
	if !p.Matches(k.SecretHash[:]) {
		t.Fatal("key does not match its own hash")
	}
	other, _ := auth.NewAPIKey(newReader(3), "01JD8K0A9Z")
	if p.Matches(other.SecretHash[:]) || p.Matches(nil) {
		t.Fatal("key matches a different hash")
	}
	secret := k.Token[len(k.Prefix)+1:]
	bad := []string{"", "ffk_", "ffk__" + secret, "ffr_01JD_" + secret, "ffk_01JD", "ffk_01-JD_" + secret,
		"ffk_01JD_" + secret[:42], "ffk_01JD_" + secret + "=", "ffk_" + strings.Repeat("a", 65) + "_" + secret}
	for _, s := range bad {
		if _, err := auth.ParseAPIKey(s); !errors.Is(err, auth.ErrInvalidAPIKey) {
			t.Errorf("ParseAPIKey(%q) = %v, want ErrInvalidAPIKey", s, err)
		}
	}
	for _, id := range []string{"", "a_b", "a-b", strings.Repeat("a", 65)} {
		if _, err := auth.NewAPIKey(newReader(4), id); !errors.Is(err, auth.ErrInvalidAPIKey) {
			t.Errorf("NewAPIKey(%q) = %v", id, err)
		}
	}
}

// A secret containing underscores must still parse: only the first
// underscore after the id separates it.
func TestAPIKeySecretWithUnderscore(t *testing.T) {
	for seed := uint64(0); seed < 200; seed++ {
		k, err := auth.NewAPIKey(newReader(seed), "abc")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(k.Token[8:], "_") {
			continue
		}
		p, err := auth.ParseAPIKey(k.Token)
		if err != nil || !p.Matches(k.SecretHash[:]) {
			t.Fatalf("ParseAPIKey(%q) = %v", k.Token, err)
		}
		return
	}
	t.Skip("no secret with an underscore in 200 seeds")
}

func TestEmailVerification(t *testing.T) {
	clk := testkit.NewFakeClock(specNow)
	store := auth.NewInMemoryVerificationStore()
	ev, err := auth.NewEmailVerifier(store, clk, newReader(5))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	user := core.IDGen{Clock: clk, Rng: core.NewSplitMix64(1)}.New()

	tok, err := ev.Issue(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	clk.Advance(auth.VerificationTTL - time.Second)
	if got, err := ev.Confirm(ctx, tok); err != nil || got != user {
		t.Fatalf("Confirm = %v, %v", got, err)
	}
	if _, err := ev.Confirm(ctx, tok); !errors.Is(err, auth.ErrVerificationInvalid) {
		t.Fatalf("second Confirm = %v, want single use", err)
	}

	late, _ := ev.Issue(ctx, user)
	clk.Advance(auth.VerificationTTL)
	if _, err := ev.Confirm(ctx, late); !errors.Is(err, auth.ErrVerificationInvalid) {
		t.Fatalf("Confirm after 24h = %v", err)
	}
	for _, bad := range []string{"", "nope", tok + "x"} {
		if _, err := ev.Confirm(ctx, bad); !errors.Is(err, auth.ErrVerificationInvalid) {
			t.Fatalf("Confirm(%q) = %v", bad, err)
		}
	}
}

func TestCSRF(t *testing.T) {
	tok, err := auth.NewCSRFToken(newReader(6))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		header, cookie string
		want           error
	}{
		{tok, tok, nil},
		{tok, tok + "x", auth.ErrCSRFMismatch},
		{tok[:len(tok)-1] + "x", tok, auth.ErrCSRFMismatch},
		{"", tok, auth.ErrCSRFMissing},
		{tok, "", auth.ErrCSRFMissing},
		{"", "", auth.ErrCSRFMissing},
	}
	for _, tt := range tests {
		if err := auth.CheckCSRF(tt.header, tt.cookie); err != tt.want {
			t.Errorf("CheckCSRF(%q, %q) = %v, want %v", tt.header, tt.cookie, err, tt.want)
		}
	}
	required := map[string]bool{"GET": false, "HEAD": false, "OPTIONS": false, "POST": true, "PUT": true, "PATCH": true, "DELETE": true}
	for m, want := range required {
		if got := auth.CSRFRequired(m, false); got != want {
			t.Errorf("CSRFRequired(%s, cookie) = %v", m, got)
		}
		if auth.CSRFRequired(m, true) {
			t.Errorf("CSRFRequired(%s, bearer) = true", m)
		}
	}
}
