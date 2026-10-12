package weblogin

import (
	"strings"
	"testing"
)

// wantReservedClaims pins the access-policy claim denylist: JWT registered
// and OIDC protocol claims, OIDC Core §5.1 standard claims, and common
// provider identity/profile claims.
var wantReservedClaims = []string{
	"iss", "sub", "aud", "exp", "nbf", "iat", "jti", "auth_time", "nonce", "azp",
	"at_hash", "c_hash", "s_hash", "acr", "amr", "sid", "cnf", "sub_jwk",
	"name", "given_name", "family_name", "middle_name", "nickname", "preferred_username",
	"profile", "picture", "website", "email", "email_verified", "gender", "birthdate",
	"zoneinfo", "locale", "phone_number", "phone_number_verified", "address", "updated_at",
	"tid", "oid", "upn", "unique_name", "ipaddr", "idp", "_claim_names", "_claim_sources", "hasgroups",
}

func TestNewRejectsEveryReservedClaim(t *testing.T) {
	for _, claim := range wantReservedClaims {
		if !IsReservedClaim(claim) {
			t.Errorf("IsReservedClaim(%q) = false", claim)
		}
		c := validConfig()
		c.Policy.Claim = claim
		if _, err := New(c); err == nil || !strings.Contains(err.Error(), "non-reserved claim") {
			t.Errorf("New accepted reserved claim %q: %v", claim, err)
		}
	}
}

func TestNewAcceptsAuthorizationClaims(t *testing.T) {
	for _, claim := range []string{"groups", "roles", "wids", "https://example.test/roles", "Email", "SUB", "team"} {
		if IsReservedClaim(claim) {
			t.Errorf("IsReservedClaim(%q) = true", claim)
		}
		c := validConfig()
		c.Policy.Claim = claim
		if _, err := New(c); err != nil {
			t.Errorf("New rejected claim %q: %v", claim, err)
		}
	}
}

func TestReservedClaimsIsACopy(t *testing.T) {
	got := ReservedClaims()
	if len(got) != len(wantReservedClaims) {
		t.Fatalf("ReservedClaims() has %d names, want %d", len(got), len(wantReservedClaims))
	}
	got[0] = "groups"
	if !IsReservedClaim("iss") || IsReservedClaim("groups") {
		t.Fatal("mutating ReservedClaims() result changed the denylist")
	}
}
