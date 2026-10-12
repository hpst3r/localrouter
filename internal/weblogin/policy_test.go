package weblogin

import (
	"strings"
	"testing"
)

func testPolicy() Policy {
	return Policy{
		JIT:           true,
		Claim:         "roles",
		UserValues:    []string{"LocalRouter.User"},
		AdminValues:   []string{"LocalRouter.Admin"},
		AdminSubjects: []string{"admin-subject-1"},
	}
}

func TestPolicyUserValueGrantsUser(t *testing.T) {
	for name, raw := range map[string]string{
		"array":  `{"sub":"s1","roles":["Other","LocalRouter.User"]}`,
		"string": `{"sub":"s1","roles":"LocalRouter.User"}`,
	} {
		d := testPolicy().Evaluate("s1", []byte(raw))
		if !d.Allowed || d.Role != RoleUser || d.Reason != "" {
			t.Errorf("%s: got %+v, want allowed user", name, d)
		}
	}
}

func TestPolicyAdminByValueOrExactSubject(t *testing.T) {
	cases := []struct {
		name, sub, raw string
	}{
		{"admin value", "s1", `{"roles":["LocalRouter.Admin"]}`},
		{"admin value beats user value", "s1", `{"roles":["LocalRouter.User","LocalRouter.Admin"]}`},
		{"admin subject with user value", "admin-subject-1", `{"roles":"LocalRouter.User"}`},
		{"admin subject with admin value", "admin-subject-1", `{"roles":["LocalRouter.Admin"]}`},
	}
	for _, c := range cases {
		d := testPolicy().Evaluate(c.sub, []byte(c.raw))
		if !d.Allowed || d.Role != RoleAdmin {
			t.Errorf("%s: got %+v, want admin", c.name, d)
		}
	}
}

// An admin subject only promotes a login the claim already admits: removing
// the subject from every allowed IdP group must deny it like anyone else.
func TestPolicyAdminSubjectStillRequiresAllowedClaim(t *testing.T) {
	cases := []struct {
		name, raw, reason string
	}{
		{"claim absent", `{}`, ReasonNotPermitted},
		{"no allowed value", `{"roles":["Reader"]}`, ReasonNotPermitted},
		{"empty array", `{"roles":[]}`, ReasonNotPermitted},
		{"admin value in unconfigured claim", `{"groups":["LocalRouter.Admin"]}`, ReasonNotPermitted},
		{"malformed claim", `{"roles":7}`, ReasonClaimMalformed},
	}
	for _, c := range cases {
		d := testPolicy().Evaluate("admin-subject-1", []byte(c.raw))
		if d.Allowed || d.Role != "" || d.Reason != c.reason {
			t.Errorf("%s: got %+v, want denied %s", c.name, d, c.reason)
		}
	}
}

// With admin_subjects as the only admin mapping (no admin values), the
// subject is promoted on a user value and denied without one.
func TestPolicyAdminSubjectsOnlyMapping(t *testing.T) {
	p := Policy{Claim: "groups", UserValues: []string{"lr-users"}, AdminSubjects: []string{"root-sub"}}
	if err := p.validate(); err != nil {
		t.Fatalf("subjects-only policy with user values rejected: %v", err)
	}
	if d := p.Evaluate("root-sub", []byte(`{"groups":["lr-users"]}`)); !d.Allowed || d.Role != RoleAdmin {
		t.Errorf("admin subject with user value: %+v, want admin", d)
	}
	if d := p.Evaluate("other", []byte(`{"groups":["lr-users"]}`)); !d.Allowed || d.Role != RoleUser {
		t.Errorf("other subject with user value: %+v, want user", d)
	}
	if d := p.Evaluate("root-sub", []byte(`{}`)); d.Allowed || d.Reason != ReasonNotPermitted {
		t.Errorf("admin subject without claim: %+v, want not_permitted", d)
	}
	// Subjects alone admit nobody, so they are not a valid policy.
	bare := Policy{Claim: "groups", AdminSubjects: []string{"root-sub"}}
	if err := bare.validate(); err == nil {
		t.Error("policy with admin subjects and no claim values accepted")
	}
}

func TestPolicyDeniesUnauthorizedAndMalformedClaims(t *testing.T) {
	long := strings.Repeat("a", 257)
	many := `["` + strings.Repeat(`x","`, 1024) + `LocalRouter.User"]`
	cases := []struct {
		name, raw, reason string
	}{
		{"claim absent", `{"sub":"s3"}`, ReasonNotPermitted},
		{"unlisted values", `{"roles":["Reader","Writer"]}`, ReasonNotPermitted},
		{"empty array", `{"roles":[]}`, ReasonNotPermitted},
		{"number", `{"roles":7}`, ReasonClaimMalformed},
		{"object", `{"roles":{"LocalRouter.User":true}}`, ReasonClaimMalformed},
		{"mixed array", `{"roles":["LocalRouter.User",1]}`, ReasonClaimMalformed},
		{"null", `{"roles":null}`, ReasonClaimMalformed},
		{"oversized entry", `{"roles":["LocalRouter.User","` + long + `"]}`, ReasonClaimMalformed},
		{"too many entries", `{"roles":` + many + `}`, ReasonClaimMalformed},
		{"not json", `roles`, ReasonClaimMalformed},
	}
	for _, c := range cases {
		d := testPolicy().Evaluate("s3", []byte(c.raw))
		if d.Allowed || d.Role != "" || d.Reason != c.reason {
			t.Errorf("%s: got %+v, want denied %s", c.name, d, c.reason)
		}
	}
}

func TestPolicyGroupOverageFailsClosed(t *testing.T) {
	groups := testPolicy()
	groups.Claim = "groups"
	overage := `"_claim_names":{"groups":"src1"},"_claim_sources":{"src1":{"endpoint":"https://graph.example.test/x"}}`
	cases := []struct {
		name, sub, raw string
		p              Policy
	}{
		{"claim_names names the claim", "s4", `{` + overage + `}`, groups},
		{"claim_names with partial inline values", "s4", `{` + overage + `,"groups":["LocalRouter.Admin"]}`, groups},
		{"claim_names even for admin subject", "admin-subject-1", `{` + overage + `}`, groups},
		{"hasgroups", "s4", `{"hasgroups":true,"groups":["LocalRouter.User"]}`, groups},
	}
	for _, c := range cases {
		d := c.p.Evaluate(c.sub, []byte(c.raw))
		if d.Allowed || d.Reason != ReasonGroupOverage {
			t.Errorf("%s: got %+v, want group_overage", c.name, d)
		}
	}
	// Overage of a claim the policy does not read is irrelevant.
	if d := testPolicy().Evaluate("s4", []byte(`{`+overage+`,"roles":"LocalRouter.User"}`)); !d.Allowed {
		t.Errorf("unrelated overage: got %+v, want allowed", d)
	}
	if d := testPolicy().Evaluate("s4", []byte(`{"_claim_names":"groups","roles":"LocalRouter.User"}`)); d.Reason != ReasonClaimMalformed {
		t.Errorf("non-object _claim_names: got %+v, want claim_malformed", d)
	}
}

func TestPolicyEmailAndOtherClaimsGrantNoPrivilege(t *testing.T) {
	p := testPolicy()
	p.AdminSubjects = []string{"boss@example.com"}
	cases := []struct {
		name, sub, raw string
	}{
		{"email equals admin subject", "s2", `{"email":"boss@example.com","roles":"LocalRouter.User"}`},
		{"preferred_username equals admin subject", "s2", `{"preferred_username":"boss@example.com","roles":"LocalRouter.User"}`},
		{"admin value in unconfigured claim", "s2", `{"groups":["LocalRouter.Admin"],"roles":"LocalRouter.User"}`},
		{"case differs", "s2", `{"roles":["localrouter.admin","LocalRouter.User"]}`},
		{"subject case differs", "BOSS@example.com", `{"roles":"LocalRouter.User"}`},
	}
	for _, c := range cases {
		d := p.Evaluate(c.sub, []byte(c.raw))
		if !d.Allowed || d.Role != RoleUser {
			t.Errorf("%s: got %+v, want plain user", c.name, d)
		}
	}
}
