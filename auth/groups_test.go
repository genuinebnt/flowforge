package auth_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/genuinebnt/flowforge/auth"
	"github.com/genuinebnt/flowforge/core"
	"github.com/genuinebnt/flowforge/testkit"
)

// TestRegress_G_LIM_05_GroupsClaimBounded: internal tokens carry the
// caller's role groups; the claim is bounded (MaxGroups valid names, no
// membership-role names) at minting and at verification, and never
// accepted in an access token.
func TestRegress_G_LIM_05_GroupsClaimBounded(t *testing.T) {
	ctx := context.Background()
	clk := testkit.NewFakeClock(time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC))
	key, err := auth.GenerateSigningKey(rand.Reader, "k1")
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := auth.NewSigner(key)
	set, _ := auth.NewKeySet(key.Public())
	m, _ := auth.NewMinter(auth.MinterConfig{Signer: signer, Clock: clk, IDs: core.NewSplitMix64(1), Issuer: "iss"})
	v, _ := auth.NewVerifier(auth.InternalVerifierConfig(set, clk, "iss"))
	tenant := core.MustParseUUID("0190a1b2-0000-7000-8000-00000000a001")

	tok, _, err := m.Internal(auth.InternalGrant{TenantID: tenant, Actor: "user:x", Role: auth.RoleApprover, Groups: []string{"finance_ap", "lead"}})
	if err != nil {
		t.Fatal(err)
	}
	c, err := v.Verify(ctx, tok)
	if err != nil || !slices.Equal(c.Groups, []string{"finance_ap", "lead"}) {
		t.Fatalf("groups claim: %+v %v", c, err)
	}
	many := make([]string, auth.MaxGroups+1)
	for i := range many {
		many[i] = fmt.Sprintf("g%d", i)
	}
	if _, _, err := m.Internal(auth.InternalGrant{TenantID: tenant, Actor: "user:x", Role: auth.RoleApprover, Groups: many[:auth.MaxGroups]}); err != nil {
		t.Fatalf("MaxGroups groups: %v", err)
	}
	for _, bad := range [][]string{many, {"admin"}, {"Finance"}, {""}, {"a b"}} {
		if _, _, err := m.Internal(auth.InternalGrant{TenantID: tenant, Actor: "user:x", Role: auth.RoleApprover, Groups: bad}); err == nil {
			t.Errorf("minted groups %q", bad)
		}
	}
	// A token signed outside the minter with too many groups, or an
	// access token with groups, is rejected.
	now := clk.Now().Unix()
	base := auth.Claims{Issuer: "iss", Audience: auth.AudienceInternal, Subject: "user:x", TenantID: core.PublicID(core.PrefixTenant, tenant),
		Role: auth.RoleApprover, ID: "j", IssuedAt: now, ExpiresAt: now + 60}
	over := base
	over.Groups = many
	forged, _ := signer.Sign(over)
	if _, err := v.Verify(ctx, forged); !errors.Is(err, auth.ErrClaimInvalid) {
		t.Fatalf("oversized groups claim: %v", err)
	}
	acc := base
	acc.Audience, acc.Groups = auth.AudienceAPI, []string{"lead"}
	av, _ := auth.NewVerifier(auth.VerifierConfig{Keys: set, Clock: clk, Issuer: "iss", Audience: auth.AudienceAPI, Revocation: auth.NoRevocation{}})
	at, _ := signer.Sign(acc)
	if _, err := av.Verify(ctx, at); !errors.Is(err, auth.ErrClaimInvalid) {
		t.Fatalf("access token with groups: %v", err)
	}
	if !auth.ValidGroupName("finance_ap") || auth.ValidGroupName("owner") {
		t.Fatal("ValidGroupName")
	}
}
