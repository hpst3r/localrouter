package weblogin

import (
	"bytes"
	"encoding/json"
	"slices"
)

// Roles granted by a successful login. They match the core role strings the
// identity store uses for humans; OIDC never grants any other role.
const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

// Policy decides whether a verified identity may log in and with which role.
// Matching is exact and case-sensitive; there is no wildcard, prefix, email or
// domain matching and no first-user promotion.
type Policy struct {
	// JIT allows the adapter to create an unknown (issuer, subject) user. It is
	// only ever exercised for logins this Policy admitted.
	JIT bool
	// Claim names the top-level ID-token claim holding a string or an array of
	// strings, for example "roles" or "groups".
	Claim string
	// UserValues and AdminValues are the Claim values granting each role.
	UserValues  []string
	AdminValues []string
	// AdminSubjects are exact `sub` values (of the configured issuer) promoted
	// to admin when Claim also holds a user or admin value; a subject alone
	// admits nobody. They never match email, name or any other claim.
	AdminSubjects []string
}

// Decision is the outcome of Policy.Evaluate. When Allowed is false Reason is
// one of ReasonNotPermitted, ReasonGroupOverage or ReasonClaimMalformed.
type Decision struct {
	Allowed bool
	Role    string
	Reason  string
}

// Fixed policy denial reasons.
const (
	ReasonNotPermitted   = "not_permitted"
	ReasonGroupOverage   = "group_overage"
	ReasonClaimMalformed = "claim_malformed"
)

// Evaluate applies the policy to subject and the raw JSON ID-token payload.
func (p Policy) Evaluate(subject string, rawClaims []byte) Decision {
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(rawClaims, &claims); err != nil {
		return Decision{Reason: ReasonClaimMalformed}
	}
	// Overage (Entra: >200 groups replaced by a distributed-claim reference,
	// or hasgroups in length-limited flows) denies the login. The referenced
	// endpoint is never fetched.
	if raw, ok := claims["_claim_names"]; ok {
		var names map[string]json.RawMessage
		if err := json.Unmarshal(raw, &names); err != nil || names == nil {
			return Decision{Reason: ReasonClaimMalformed}
		}
		if _, ok := names[p.Claim]; ok {
			return Decision{Reason: ReasonGroupOverage}
		}
	}
	if _, ok := claims["hasgroups"]; ok && p.Claim == "groups" {
		return Decision{Reason: ReasonGroupOverage}
	}
	values, ok := claimValues(claims[p.Claim])
	if !ok {
		return Decision{Reason: ReasonClaimMalformed}
	}
	// Every login, admin subjects included, needs an allowed claim value: a
	// subject only promotes a login the claim already admits, so removing it
	// from every allowed IdP group denies it.
	if intersects(values, p.AdminValues) {
		return Decision{Allowed: true, Role: RoleAdmin}
	}
	if intersects(values, p.UserValues) {
		if slices.Contains(p.AdminSubjects, subject) {
			return Decision{Allowed: true, Role: RoleAdmin}
		}
		return Decision{Allowed: true, Role: RoleUser}
	}
	return Decision{Reason: ReasonNotPermitted}
}

// Bounds on the policy claim. Anything larger is treated as malformed rather
// than truncated, so an oversized claim can never partially match.
const (
	maxClaimValues   = 1024
	maxClaimValueLen = 256
)

// claimValues decodes an absent claim as no values, a JSON string as one value
// and a JSON array of strings as is. Anything else, including null, is
// malformed.
func claimValues(raw json.RawMessage) ([]string, bool) {
	if raw == nil {
		return nil, true
	}
	var many []string
	switch t := bytes.TrimSpace(raw); {
	case len(t) > 0 && t[0] == '"':
		var one string
		if err := json.Unmarshal(t, &one); err != nil {
			return nil, false
		}
		many = []string{one}
	case len(t) > 0 && t[0] == '[':
		if err := json.Unmarshal(t, &many); err != nil {
			return nil, false
		}
	default:
		return nil, false
	}
	if len(many) > maxClaimValues {
		return nil, false
	}
	for _, v := range many {
		if len(v) > maxClaimValueLen {
			return nil, false
		}
	}
	return many, true
}

func intersects(values, allowed []string) bool {
	for _, v := range values {
		if slices.Contains(allowed, v) {
			return true
		}
	}
	return false
}
