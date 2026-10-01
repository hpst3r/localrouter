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

	if now.Before(st.cooldownUntil) {
		if has && snap.FetchedAt.After(st.cooldownStart) && showsHeadroom(snap, now) {
			st.cooldownStart, st.cooldownUntil = time.Time{}, time.Time{}
		} else {
			return false, "cooldown until " + st.cooldownUntil.UTC().Format(time.RFC3339)
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
		floor := 0.0
		if background {
			floor = acct.Reserve[w.Kind]
		}
		limit := 1 - floor - margin - load
		if used > limit+epsilon {
			what := "interactive limit"
			if background {
				what = "background reserve"
			}
			return false, fmt.Sprintf("%s %s (used %.2f > %.2f)", what, w.Kind, used, limit)
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
	}
	if !until.IsZero() {
		st.cooldownStart = now
		if until.After(st.cooldownUntil) {
			st.cooldownUntil = until
		}
	}
	p.mu.Unlock()

	if !until.IsZero() {
		p.opts.Logger.Info("account cooldown", "account", id, "status", o.Status, "until", until)
	}
	p.quota.RequestRefresh(id, o.Status == 429)
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
