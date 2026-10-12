package config

import (
	"strconv"
	"testing"
)

// Every claim weblogin reserves is refused by config Load too, from the one
// shared list. The names are pinned here independently of that list.
func TestIdentityAccessRejectsEveryReservedClaim(t *testing.T) {
	for _, claim := range []string{
		"iss", "sub", "aud", "exp", "nbf", "iat", "jti", "auth_time", "nonce", "azp",
		"at_hash", "c_hash", "s_hash", "acr", "amr", "sid", "cnf", "sub_jwk",
		"name", "given_name", "family_name", "middle_name", "nickname", "preferred_username",
		"profile", "picture", "website", "email", "email_verified", "gender", "birthdate",
		"zoneinfo", "locale", "phone_number", "phone_number_verified", "address", "updated_at",
		"tid", "oid", "upn", "unique_name", "ipaddr", "idp", "_claim_names", "_claim_sources", "hasgroups",
	} {
		t.Run(claim, func(t *testing.T) {
			wantErrs(t, withAccess("    claim: "+strconv.Quote(claim)+"\n    user_values: [u]\n    admin_values: [a]\n"),
				"identity.access.claim must be a non-reserved claim name")
		})
	}
}

func TestIdentityAccessAllowsAuthorizationClaims(t *testing.T) {
	for _, claim := range []string{"groups", "roles", "wids", "Email", "team"} {
		if _, err := Load(write(t, withAccess("    claim: "+claim+"\n    user_values: [u]\n    admin_values: [a]\n"))); err != nil {
			t.Errorf("claim %q rejected: %v", claim, err)
		}
	}
}
