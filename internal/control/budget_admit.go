package control

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/core"
)

// Reason codes for the advisory budget estimate. They are generic and
// non-leaking — a raw store error (which may embed paths, DSNs or credentials)
// is never echoed — and they are deliberately distinct from the policy
// decision's own reason: a missing budget input is never reported as a policy
// denial, and vice versa.
const (
	// admitBudgetClientUnavailable: no authenticating client, so the
	// client-scope budget cannot even be named.
	admitBudgetClientUnavailable = "client_identity_unavailable"
	// admitBudgetAccountUnavailable: the policy named an account this
	// generation does not configure.
	admitBudgetAccountUnavailable = "account_identity_unavailable"
	// admitBudgetStoreError: the store could not be read, or an amount did not
	// fit the money type; the estimate fails closed rather than guessing.
	admitBudgetStoreError = "budget_store_error"
	// admitBudgetExceeded: every input was read and the fixed hold would not
	// fit under a configured ceiling. This mirrors the proxy's own
	// "budget_exceeded" denial; it does not itself deny anything.
	admitBudgetExceeded = "budget_exceeded"
)

// admitBudgetTimeout bounds the whole estimate: the four instance reads share
// one context, so the estimate is a single bounded store interaction.
const admitBudgetTimeout = 2 * time.Second

// admitBudgetEstimate is the advisory spend-control block the admit response
// carries when spend controls are configured. It is informational only: it
// never changes the response's decision/account_id/reason, takes no
// reservation and guarantees nothing — the authoritative admission is the
// proxy's own Gate.Reserve for the attempt it actually sends.
//
// Allow answers one question: would the generation's fixed per-attempt hold
// fit under every ceiling that names the authenticated client or the
// policy-chosen account, for the UTC day and month containing this instant? An
// empty Reason means the estimate was computed; otherwise it names why no
// estimate could be made (see the codes above).
type admitBudgetEstimate struct {
	Advisory           bool   `json:"advisory"`
	ReservationCreated bool   `json:"reservation_created"`
	Client             string `json:"client,omitempty"`
	AccountID          string `json:"account_id,omitempty"`
	HoldMicros         int64  `json:"hold_micros"`
	HoldUSD            string `json:"hold_usd"`
	Allow              bool   `json:"allow"`
	Reason             string `json:"reason,omitempty"`
}

// admitBudgetInstance names one budget instance the estimate reads.
type admitBudgetInstance struct{ scope, key, period string }

// admitBudget builds the advisory estimate for an admit decision, or nil when
// there is nothing to say.
//
// It returns nil when spend controls are not configured (a nil BudgetSource),
// so the response stays byte-for-byte the legacy one, and when the policy
// denied: a denial is terminal and already reported by the response's own
// fields, and there is no admitted account whose hold could be tested. The
// policy decision is never overridden and nothing here writes to the store.
//
// candidates is exactly the list the policy was asked to choose from, so the
// estimate names only an identity this generation actually configured as
// eligible; an empty or foreign account id yields no estimate rather than a
// read of some other account's budget.
//
// detail is false for a multi-user caller that is not a global reader (a
// user bearer). Its estimate then covers only its own client and user
// buckets: the shared account-scope ceilings belong to the operator, so they
// are neither read nor named, and the chosen account is not echoed.
//
// When controls are configured the estimate revalidates the request's bearer
// through the same Authenticate the auth gate uses — even under RequireAuth
// false, because the client-scope budget must name a real client — and reads
// exactly the four instances an admitted attempt pins (client and account, day
// and month) with one clock instant and one bounded context. Ceilings come
// from the generation-pinned Limits, so the estimate can never disagree with
// what the generation's Gate enforces.
func (s *Server) admitBudget(r *http.Request, d core.Decision, candidates []string, detail bool) *admitBudgetEstimate {
	src := s.deps.Budgets
	if src == nil || !d.Allow {
		return nil
	}
	hold := src.ReservationMicros()
	est := &admitBudgetEstimate{
		Advisory:           true,
		ReservationCreated: false,
		HoldMicros:         hold,
		HoldUSD:            formatMicrosUSD(hold),
	}
	if hold <= 0 {
		// A non-positive hold can never be admitted (the Gate rejects it as a
		// wiring error), so claiming room would be a lie.
		est.Reason = admitBudgetStoreError
		return est
	}

	client, user, ok := s.admitBudgetIdentity(r)
	if !ok {
		est.Reason = admitBudgetClientUnavailable
		return est
	}
	est.Client = client

	account := d.AccountID
	if detail {
		if account == "" || !slices.Contains(candidates, account) {
			est.Reason = admitBudgetAccountUnavailable
			return est
		}
		est.AccountID = account
	}

	// One clock read and one context govern the whole estimate, exactly as the
	// operator budget read does. The instant is UTC because the period
	// instances are defined in UTC.
	now := s.deps.Clock.Now().UTC()
	ctx, cancel := context.WithTimeout(r.Context(), admitBudgetTimeout)
	defer cancel()
	limits, ok := effectiveLimits(src, user)
	if !ok {
		// Limits the Gate would refuse (duplicate or invalid user ceilings)
		// deny there; the estimate fails closed rather than guess.
		est.Reason = admitBudgetStoreError
		return est
	}

	instances := []admitBudgetInstance{
		{budget.ScopeClient, client, budget.PeriodDay},
		{budget.ScopeClient, client, budget.PeriodMonth},
	}
	if detail {
		instances = append(instances,
			admitBudgetInstance{budget.ScopeAccount, account, budget.PeriodDay},
			admitBudgetInstance{budget.ScopeAccount, account, budget.PeriodMonth})
	}
	if user != "" {
		// A user-owned credential also pins the user's day and month
		// instances, shared by all of that user's keys, exactly as
		// Gate.Reserve does for rec.UserID.
		instances = append(instances,
			admitBudgetInstance{budget.ScopeUser, user, budget.PeriodDay},
			admitBudgetInstance{budget.ScopeUser, user, budget.PeriodMonth})
	}
	allow := true
	exceeded := false
	for _, inst := range instances {
		snap, err := src.Snapshot(ctx, inst.scope, inst.key, inst.period, now)
		if err != nil {
			est.Reason = admitBudgetStoreError
			return est
		}
		limit, has := limitFor(limits, inst.scope, inst.key, inst.period)
		if !has {
			continue // no configured ceiling: this instance is unlimited
		}
		avail, ok := budgetAvailable(limit, snap)
		if !ok {
			// An amount the money type cannot represent is never wrapped or
			// clamped: fail closed.
			est.Reason = admitBudgetStoreError
			return est
		}
		// available >= hold is exactly the store's outstanding+hold <= limit,
		// evaluated with checked arithmetic on both sides.
		if avail < hold {
			allow, exceeded = false, true
		}
	}
	est.Allow = allow
	if exceeded {
		est.Reason = admitBudgetExceeded
	}
	return est
}

// admitBudgetIdentity names the budget identities of the caller: the client
// key and, for a user-owned credential, the user. In multi-user mode they come
// from the request's authenticated principal only — the client is
// Principal.Client.Name (the key id for a user API key, as the proxy records
// it) and the user is Principal.UserID; the legacy Authenticate is never
// consulted. Legacy requests keep admitBudgetClient.
func (s *Server) admitBudgetIdentity(r *http.Request) (client, user string, ok bool) {
	a, multi := authFrom(r.Context())
	if !multi {
		client, ok = s.admitBudgetClient(r)
		return client, "", ok
	}
	p := a.principal
	client = p.Client.Name
	if client == "" && p.Kind == core.PrincipalUserKey {
		client = p.KeyID
	}
	if strings.TrimSpace(client) == "" {
		return "", "", false
	}
	return client, p.UserID, true
}

// effectiveLimits is the ceiling set an estimate or report evaluates for an
// optional user. When the source implements UserBudgetSource its UserLimits
// are authoritative for the user scope (any ScopeUser entry in Limits is then
// ignored, so a default and an explicit entry are never both applied);
// otherwise ScopeUser entries of Limits apply as listed. ok is false when the
// resulting user ceilings would be refused by the store (duplicate period,
// unknown period or negative amount), so the caller fails closed.
func effectiveLimits(src BudgetSource, user string) ([]budget.Limit, bool) {
	base := src.Limits()
	us, dynamic := src.(UserBudgetSource)
	if !dynamic {
		return base, validUserLimits(base, user)
	}
	out := make([]budget.Limit, 0, len(base)+2)
	for _, l := range base {
		if l.Scope != budget.ScopeUser {
			out = append(out, l)
		}
	}
	if user != "" {
		for _, l := range us.UserLimits(user) {
			if l.Scope == budget.ScopeUser && l.Key == user {
				out = append(out, l)
			}
		}
	}
	return out, validUserLimits(out, user)
}

// validUserLimits reports whether the ceilings naming user are ones the store
// accepts: known period, non-negative, at most one per period.
func validUserLimits(limits []budget.Limit, user string) bool {
	if user == "" {
		return true
	}
	seen := map[string]bool{}
	for _, l := range limits {
		if l.Scope != budget.ScopeUser || l.Key != user {
			continue
		}
		if (l.Period != budget.PeriodDay && l.Period != budget.PeriodMonth) || l.Micros < 0 || seen[l.Period] {
			return false
		}
		seen[l.Period] = true
	}
	return true
}

// admitBudgetClient revalidates the request's bearer through the same
// authenticator the auth gate uses and returns the authenticated client's
// name. It is deliberately independent of RequireAuth: whenever spend controls
// are configured the estimate needs a real client identity (the bearer's
// class is irrelevant — the caller's request class already drove policy), so
// an absent, malformed or unknown bearer simply yields no estimate. It never
// logs and never fails the request.
func (s *Server) admitBudgetClient(r *http.Request) (string, bool) {
	if s.deps.Authenticate == nil {
		return "", false
	}
	token, ok := bearer(r.Header.Get("Authorization"))
	if !ok {
		return "", false
	}
	c, ok := s.deps.Authenticate(token)
	if !ok || strings.TrimSpace(c.Name) == "" {
		return "", false
	}
	return c.Name, true
}
