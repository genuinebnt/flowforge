package auth_test

import (
	"context"
	"encoding/hex"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/testkit"
)

// jwtCase is the schema of conformance/auth/jwt/*.json.
type jwtCase struct {
	Description string    `json:"description"`
	Now         int64     `json:"now"`
	Issuer      string    `json:"issuer"`
	Audience    string    `json:"audience"`
	Keys        []confKey `json:"keys"` // the verifier's key set
	// UnlistedKeys signed some tokens but are not in the key set.
	UnlistedKeys    []confKey        `json:"unlisted_keys,omitempty"`
	RevokedJTIs     []string         `json:"revoked_jtis,omitempty"`
	SessionVersions map[string]int64 `json:"session_versions,omitempty"`
	Token           string           `json:"token"`
	// Resign marks tokens that are exactly the Signer's output for
	// Expect.Claims with SignedBy, pinning the encoder too.
	Resign   bool      `json:"resign,omitempty"`
	SignedBy string    `json:"signed_by,omitempty"`
	Expect   jwtExpect `json:"expect"`
}

type confKey struct {
	KID     string        `json:"kid"`
	SeedHex string        `json:"seed_hex"`
	State   auth.KeyState `json:"state"`
}

type jwtExpect struct {
	Accept bool         `json:"accept"`
	Reason string       `json:"reason,omitempty"`
	Claims *auth.Claims `json:"claims,omitempty"`
}

func (k confKey) signingKey(t *testing.T) auth.SigningKey {
	seed, err := hex.DecodeString(k.SeedHex)
	if err != nil {
		t.Fatal(err)
	}
	sk, err := auth.NewSigningKey(k.KID, seed, k.State)
	if err != nil {
		t.Fatal(err)
	}
	return sk
}

func TestAuth002_JWTRejections(t *testing.T) {
	files := testkit.LoadCases[jwtCase](t, "auth/jwt")
	required := []string{"alg_none", "alg_not_allowed", "wrong_aud", "wrong_iss", "expired",
		"not_yet_valid", "unknown_kid", "bad_signature"}
	reasons := map[string]bool{}
	for _, f := range files {
		c := f.Data
		t.Run(f.Name, func(t *testing.T) {
			var pubs []auth.PublicKey
			signers := map[string]auth.SigningKey{}
			for _, k := range c.Keys {
				sk := k.signingKey(t)
				pubs = append(pubs, sk.Public())
				signers[k.KID] = sk
			}
			rev := auth.NewInMemoryRevocation()
			for _, j := range c.RevokedJTIs {
				rev.Deny(j)
			}
			for sub, v := range c.SessionVersions {
				rev.SetSessionVersion(sub, v)
			}
			v, err := auth.NewVerifier(auth.VerifierConfig{
				Keys: mustKeySet(t, pubs...), Clock: testkit.NewFakeClock(time.Unix(c.Now, 0)),
				Issuer: c.Issuer, Audience: c.Audience, Revocation: rev,
			})
			if err != nil {
				t.Fatal(err)
			}
			claims, err := v.Verify(context.Background(), c.Token)
			if c.Expect.Accept {
				if err != nil {
					t.Fatalf("Verify rejected a valid token: %v", err)
				}
				if c.Expect.Claims != nil && !reflect.DeepEqual(*claims, *c.Expect.Claims) {
					t.Fatalf("claims = %+v, want %+v", *claims, *c.Expect.Claims)
				}
			} else {
				if err == nil {
					t.Fatalf("Verify accepted a token that must be rejected with %q", c.Expect.Reason)
				}
				if got := auth.RejectionReason(err); got != c.Expect.Reason {
					t.Fatalf("rejection reason = %q (%v), want %q", got, err, c.Expect.Reason)
				}
				if !errors.Is(err, auth.ErrInvalidToken) {
					t.Fatalf("rejection %v does not match ErrInvalidToken", err)
				}
				reasons[c.Expect.Reason] = true
			}
			if c.Resign {
				sk, ok := signers[c.SignedBy]
				if !ok || c.Expect.Claims == nil {
					t.Fatal("resign case needs signed_by in keys and expected claims")
				}
				got, err := mustSigner(t, sk).Sign(*c.Expect.Claims)
				if err != nil || got != c.Token {
					t.Fatalf("Sign(claims) = %q, %v; want the conformance token", got, err)
				}
			}
		})
	}
	for _, r := range required {
		if !reasons[r] {
			t.Errorf("auth_002 requires a %q case in conformance/auth/jwt", r)
		}
	}
	// Distinct reasons must come from distinct sentinels.
	sentinels := []error{auth.ErrAlgNone, auth.ErrAlgNotAllowed, auth.ErrAudience, auth.ErrIssuer,
		auth.ErrExpired, auth.ErrNotYetValid, auth.ErrUnknownKid, auth.ErrBadSignature}
	for i, a := range sentinels {
		if got := auth.RejectionReason(a); !slices.Contains(required, got) {
			t.Errorf("sentinel %v has reason %q", a, got)
		}
		for _, b := range sentinels[i+1:] {
			if errors.Is(a, b) || errors.Is(b, a) {
				t.Errorf("sentinels %v and %v are not distinct", a, b)
			}
		}
	}
}
