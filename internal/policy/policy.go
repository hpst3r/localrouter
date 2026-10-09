package policy

import (
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const (
	defaultStaleAfter = 10 * time.Minute
	cooldownMin       = 60 * time.Second
	exhaustedFrac     = 0.999
	epsilon           = 1e-9
)

// Options configures a Policy.
type Options struct {
	// StaleAfter is the maximum snapshot age before it is considered stale
	// (default 10m).
	StaleAfter time.Duration
	// SafetyMargin is added to background demand on every window.
	SafetyMargin float64
	// InflightEstimate is the assumed quota fraction consumed per open lease.
	InflightEstimate float64
	// Clock defaults to core.SystemClock.
	Clock core.Clock
	// Logger defaults to a discarding logger.
	Logger *slog.Logger
}

// Policy is the admission gate. It is safe for concurrent use.
type Policy struct {
	quota    core.QuotaSource
	opts     Options
	accounts map[string]core.Account

	mu    sync.Mutex
	state map[string]*accountState
}

type accountState struct {
	inflight      int
	cooldownStart time.Time
	cooldownUntil time.Time
	// topUp is set for a prepaid 402 cooldown: it may clear early only once
	// a fresh balance exceeds topUpFrom (the balance known at the 402; nil
	// when unknown, then any positive balance).
	topUp     bool
	topUpFrom *float64
}

var _ core.Policy = (*Policy)(nil)

// New returns a Policy over the given accounts using quota for snapshots.
func New(accounts []core.Account, quota core.QuotaSource, opts Options) *Policy {
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = defaultStaleAfter
	}
	if opts.Clock == nil {
		opts.Clock = core.SystemClock{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	p := &Policy{
		quota:    quota,
		opts:     opts,
		accounts: make(map[string]core.Account, len(accounts)),
		state:    make(map[string]*accountState, len(accounts)),
	}
	for _, a := range accounts {
		p.accounts[a.ID] = a
		p.state[a.ID] = &accountState{}
	}
	return p
}

// Acquire selects the first admissible account from candidates (in order,
// skipping exclude) and creates a lease on it. On denial the lease is nil and
// the decision reason lists each candidate's rejection.
func (p *Policy) Acquire(class core.Class, candidates []string, exclude map[string]bool) (core.Lease, core.Decision) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id, dec := p.selectLocked(class, candidates, exclude)
	if !dec.Allow {
		p.opts.Logger.Info("policy denied", "class", class, "reason", dec.Reason)
		return nil, dec
	}
	p.state[id].inflight++
	return &lease{p: p, id: id}, dec
}

// DryRun evaluates candidates like Acquire without creating a lease.
func (p *Policy) DryRun(class core.Class, candidates []string) core.Decision {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, dec := p.selectLocked(class, candidates, nil)
	return dec
}

// Status describes the admission state of one account.
func (p *Policy) Status(accountID string) core.AccountState {
	p.mu.Lock()
	defer p.mu.Unlock()
	acct, ok := p.accounts[accountID]
	if !ok {
		return core.AccountState{Reason: "unknown account"}
	}
	now := p.opts.Clock.Now()
	st := p.state[accountID]
	snap, has := p.quota.Latest(accountID)
	bgOK, bgReason := p.admitLocked(acct, core.ClassBackground, now)
	inOK, inReason := p.admitLocked(acct, core.ClassInteractive, now)
	out := core.AccountState{
		Inflight:              st.inflight,
		Stale:                 p.isStale(snap, has, now),
		BackgroundAdmissible:  bgOK,
		InteractiveAdmissible: inOK,
	}
	if now.Before(st.cooldownUntil) {
		out.CooldownUntil = st.cooldownUntil
	}
	switch {
	case !inOK:
		out.Reason = inReason
	case !bgOK:
		out.Reason = bgReason
	}
	return out
}

func (p *Policy) selectLocked(class core.Class, candidates []string, exclude map[string]bool) (string, core.Decision) {
	now := p.opts.Clock.Now()
	var reasons []string
	for _, id := range candidates {
		if exclude[id] {
			reasons = append(reasons, id+": excluded")
			continue
		}
		acct, ok := p.accounts[id]
		if !ok {
			reasons = append(reasons, id+": unknown account")
			continue
		}
		if ok, why := p.admitLocked(acct, class, now); !ok {
			reasons = append(reasons, id+": "+why)
			continue
		}
		reason := "admitted " + id
		if len(reasons) > 0 {
			reason += " (skipped " + strings.Join(reasons, "; ") + ")"
		}
		return id, core.Decision{Allow: true, AccountID: id, Reason: reason}
	}
	if len(reasons) == 0 {
		return "", core.Decision{Reason: "no candidate accounts"}
	}
	return "", core.Decision{Reason: strings.Join(reasons, "; ")}
}

// admitLocked applies the admission rules to one account. It may clear an
// expired or superseded cooldown. p.mu must be held.
func (p *Policy) admitLocked(acct core.Account, class core.Class, now time.Time) (bool, string) {
	st := p.state[acct.ID]
	snap, has := p.quota.Latest(acct.ID)

	prepaid := acct.Provider == core.ProviderOpenRouter
	if now.Before(st.cooldownUntil) {
		if has && (prepaid && st.topUp && prepaidHeadroom(snap, st.cooldownStart, st.topUpFrom, now) ||
			!prepaid && snap.FetchedAt.After(st.cooldownStart) && showsHeadroom(snap, now)) {
			st.cooldownStart, st.cooldownUntil, st.topUp, st.topUpFrom = time.Time{}, time.Time{}, false, nil
		} else {
			return false, "cooldown until " + st.cooldownUntil.UTC().Format(time.RFC3339)
		}
	}

	// Known prepaid exhaustion denies every class even when the snapshot is
	// stale: a balance never rolls over on its own, so failing open would
	// only burn an upstream 402.
	if prepaid && has {
		if why := prepaidExhausted(snap); why != "" {
			return false, why
		}
	}

	background := class == core.ClassBackground
	if p.isStale(snap, has, now) {
		if background && hasReserve(acct) {
			return false, "quota snapshot stale or missing; reserved account denies background"
		}
		return true, ""
	}

	if snap.Allowed != nil && !*snap.Allowed && !anyRolled(snap, now) {
		return false, "provider reports limit reached"
	}

	leases := st.inflight
	margin := 0.0
	if background {
		leases++ // the request being admitted
		margin = p.opts.SafetyMargin
	}
	load := float64(leases) * p.opts.InflightEstimate
	for _, w := range snap.Windows {
		used := effectiveUsed(w, now)
		// A fully exhausted (unrolled) window refuses every class: the
		// upstream will 429, so admitting only burns a failover attempt.
		if used >= exhaustedFrac {
			return false, fmt.Sprintf("%s window exhausted (0%% left)", w.Kind)
		}
		floor := 0.0
		if background {
			floor = acct.Reserve[w.Kind]
		}
		limit := 1 - floor - margin - load
		if used > limit+epsilon {
			if background {
				// e.g. "5h: 91% used, 9% left; background needs more than 11% left (reserve 10% + margin 0% + in-flight 1%)"
				return false, fmt.Sprintf("%s: %s used, %s left; background needs more than %s left (reserve %s + margin %s + in-flight %s)",
					w.Kind, pct(used), pct(1-used), pct(1-limit), pct(floor), pct(margin), pct(load))
			}
			return false, fmt.Sprintf("%s: %s used, %s left; interactive needs more than %s left (in-flight %s)",
				w.Kind, pct(used), pct(1-used), pct(1-limit), pct(load))
		}
	}
	return true, ""
}

func (p *Policy) isStale(snap core.Snapshot, has bool, now time.Time) bool {
	return !has || now.Sub(snap.FetchedAt) > p.opts.StaleAfter
}

func (p *Policy) release(id string, o core.Outcome) {
	now := p.opts.Clock.Now()
	p.mu.Lock()
	st := p.state[id]
	if st.inflight > 0 {
		st.inflight--
	}
	prepaid := p.accounts[id].Provider == core.ProviderOpenRouter
	var until time.Time
	switch o.Status {
	case 429:
		until = now.Add(cooldownMin)
		if o.ResetAt.After(until) {
			until = o.ResetAt
		}
		if snap, ok := p.quota.Latest(id); ok {
			if r := earliestExhaustedReset(snap, now); r.After(until) {
				until = r
			}
		}
	case 401, 403:
		until = now.Add(cooldownMin)
	case 402:
		// Payment required: OpenRouter's out-of-credit answer. Other
		// providers keep their previous (no cooldown) handling.
		if prepaid {
			until = now.Add(cooldownMin)
			if o.ResetAt.After(until) {
				until = o.ResetAt
			}
		}
	}
	if !until.IsZero() {
		st.cooldownStart = now
		if until.After(st.cooldownUntil) {
			st.cooldownUntil = until
		}
		st.topUp, st.topUpFrom = prepaid && o.Status == 402, nil
		if snap, ok := p.quota.Latest(id); st.topUp && ok && snap.Credits != nil && !snap.Credits.FetchedAt.IsZero() {
			b := snap.Credits.BalanceUSD
			st.topUpFrom = &b
		}
	}
	p.mu.Unlock()

	if !until.IsZero() {
		p.opts.Logger.Info("account cooldown", "account", id, "status", o.Status, "until", until)
	}
	p.quota.RequestRefresh(id, o.Status == 429 || prepaid && o.Status == 402)
}

func effectiveUsed(w core.Window, now time.Time) float64 {
	if !w.ResetAt.IsZero() && !now.Before(w.ResetAt) {
		return 0
	}
	return w.UsedFrac
}

func anyRolled(s core.Snapshot, now time.Time) bool {
	for _, w := range s.Windows {
		if !w.ResetAt.IsZero() && !now.Before(w.ResetAt) {
			return true
		}
	}
	return false
}

// showsHeadroom reports whether a snapshot indicates the account can serve
// again: provider gate not closed and every window below 1.0.
func showsHeadroom(s core.Snapshot, now time.Time) bool {
	if s.Allowed != nil && !*s.Allowed {
		return false
	}
	if len(s.Windows) == 0 && s.Allowed == nil {
		return false
	}
	for _, w := range s.Windows {
		if effectiveUsed(w, now) >= 1.0 {
			return false
		}
	}
	return true
}

// earliestExhaustedReset returns the earliest future ResetAt among windows at
// or above exhaustedFrac, or zero if none.
func earliestExhaustedReset(s core.Snapshot, now time.Time) time.Time {
	var best time.Time
	for _, w := range s.Windows {
		if w.UsedFrac < exhaustedFrac || w.ResetAt.IsZero() || !w.ResetAt.After(now) {
			continue
		}
		if best.IsZero() || w.ResetAt.Before(best) {
			best = w.ResetAt
		}
	}
	return best
}

// prepaidExhausted explains why a prepaid (openrouter) account cannot serve,
// or returns "". A known balance at or below zero always denies; a known key
// cap with no remaining spend denies regardless of its predicted reset. The
// provider limit_remaining is the only authoritative signal that a cap has
// been restored, so a stale exhaustion is never cleared on the clock alone.
// Unknown parts (never fetched, or credits unavailable) never deny on their
// own.
func prepaidExhausted(s core.Snapshot) string {
	if c := s.Credits; c != nil && !c.FetchedAt.IsZero() && c.BalanceUSD <= 0 {
		return "openrouter account balance " + usd(c.BalanceUSD) + " (credit exhausted)"
	}
	if keyCapExhausted(s.Key) {
		k := s.Key
		return fmt.Sprintf("openrouter key spending cap exhausted (%s left of %s)", usd(*k.LimitRemainingUSD), usd(*k.LimitUSD))
	}
	return ""
}

// keyCapExhausted reports whether a known key cap has no spend left. A cap is
// exhausted when both the cap and its remaining amount are known and remaining
// is at or below zero; a malformed pair (finite cap with unknown remaining, or
// null cap with finite remaining) is rejected by the quota parser and must not
// be read as exhausted. LimitResetAt is informational: reaching the predicted
// reset does not restore spending credit, so it never clears exhaustion. Only
// a fresh /key observation with positive LimitRemainingUSD reopens the gate.
func keyCapExhausted(k *core.KeyUsage) bool {
	return k != nil && !k.FetchedAt.IsZero() && k.LimitRemainingUSD != nil && k.LimitUSD != nil &&
		*k.LimitRemainingUSD <= 0
}

// prepaidHeadroom reports whether a prepaid account was topped up after a
// 402: a balance observed after since that is positive and above from (the
// balance known at the 402, if any), with no exhausted key cap. Key data alone
// is not proof of funds, so unavailable credits never clear a cooldown early.
func prepaidHeadroom(s core.Snapshot, since time.Time, from *float64, now time.Time) bool {
	c := s.Credits
	return c != nil && c.FetchedAt.After(since) && c.BalanceUSD > 0 &&
		(from == nil || c.BalanceUSD > *from+epsilon) && !keyCapExhausted(s.Key)
}

func hasReserve(a core.Account) bool {
	for _, r := range a.Reserve {
		if r > 0 {
			return true
		}
	}
	return false
}

type lease struct {
	p    *Policy
	id   string
	once sync.Once
}

// AccountID returns the leased account.
func (l *lease) AccountID() string { return l.id }

// Release ends the lease; subsequent calls are no-ops.
func (l *lease) Release(o core.Outcome) {
	l.once.Do(func() { l.p.release(l.id, o) })
}
