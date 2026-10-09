package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func orAcct(id string) core.Account {
	return core.Account{ID: id, Provider: core.ProviderOpenRouter, BaseURL: "https://openrouter.ai/api/v1"}
}

func credits(balance float64, fetched time.Time) *core.Credits {
	return &core.Credits{TotalCreditsUSD: 100, TotalUsageUSD: 100 - balance, BalanceUSD: balance, FetchedAt: fetched}
}

func keyCap(limit, remaining *float64, resetAt, fetched time.Time) *core.KeyUsage {
	return &core.KeyUsage{LimitUSD: limit, LimitRemainingUSD: remaining, LimitResetAt: resetAt, UsageUSD: 500, FetchedAt: fetched}
}

func orSnap(id string, c *core.Credits, k *core.KeyUsage) core.Snapshot {
	s := core.Snapshot{AccountID: id, Source: "usage_api", Credits: c, Key: k}
	if c != nil && !c.FetchedAt.IsZero() {
		s.FetchedAt = c.FetchedAt
	}
	if k != nil && !k.FetchedAt.IsZero() && (s.FetchedAt.IsZero() || k.FetchedAt.Before(s.FetchedAt)) {
		s.FetchedAt = k.FetchedAt
	}
	return s
}

func TestOpenRouterAdmission(t *testing.T) {
	stale := t0.Add(-3 * time.Hour)
	unlimited := keyCap(nil, nil, time.Time{}, t0)
	tests := []struct {
		name   string
		snap   *core.Snapshot
		allow  bool
		reason string
	}{
		{"user's exact negative balance", ptr(orSnap("or", credits(770.8176-770.893717902, t0), unlimited)), false, "account balance -$0.08"},
		{"zero balance", ptr(orSnap("or", credits(0, t0), unlimited)), false, "account balance $0.00"},
		{"stale negative balance still denies", ptr(orSnap("or", credits(-1, stale), nil)), false, "account balance -$1.00"},
		{"tiny positive balance at 99.99% lifetime use", ptr(orSnap("or", &core.Credits{TotalCreditsUSD: 100, TotalUsageUSD: 99.99, BalanceUSD: 0.01, FetchedAt: t0}, unlimited)), true, ""},
		{"stale positive balance allowed (no reserve)", ptr(orSnap("or", credits(5, stale), nil)), true, ""},
		{"no snapshot yet: unknown, upstream 402 is the backstop", nil, true, ""},
		{"credits unavailable (403), key unlimited", ptr(orSnap("or", &core.Credits{Err: "credits: management key required (http 403); balance unavailable"}, unlimited)), true, ""},
		{"credits unavailable, key cap exhausted", ptr(orSnap("or", &core.Credits{Err: "x"}, keyCap(ptr(10.0), ptr(0.0), time.Time{}, t0))), false, "key spending cap exhausted"},
		{"zero cap is not unlimited", ptr(orSnap("or", credits(50, t0), keyCap(ptr(0.0), ptr(0.0), time.Time{}, t0))), false, "key spending cap exhausted"},
		{"negative key remaining", ptr(orSnap("or", credits(50, t0), keyCap(ptr(10.0), ptr(-0.2), time.Time{}, t0))), false, "key spending cap exhausted"},
		{"stale exhausted key cap without reset still denies", ptr(orSnap("or", nil, keyCap(ptr(10.0), ptr(0.0), time.Time{}, stale))), false, "key spending cap exhausted"},
		{"daily cap remaining is authoritative, not cap minus lifetime usage", ptr(orSnap("or", credits(50, t0), keyCap(ptr(10.0), ptr(9.0), t0.Add(time.Hour), t0))), true, ""},
		// A predicted cap reset is informational only: reaching LimitResetAt
		// does not restore spending credit, so a stale exhausted key cap keeps
		// denying BOTH classes until a fresh /key observation reports positive
		// remaining. Reopening on the clock alone would burn an upstream 402.
		{"exhausted key cap past its reset still denies", ptr(orSnap("or", credits(50, stale), keyCap(ptr(10.0), ptr(0.0), t0.Add(-time.Minute), stale))), false, "key spending cap exhausted"},
		{"exhausted key cap reset far in the past still denies", ptr(orSnap("or", credits(50, stale), keyCap(ptr(10.0), ptr(0.0), t0.Add(-72*time.Hour), stale))), false, "key spending cap exhausted"},
		{"key reset never clears account exhaustion", ptr(orSnap("or", credits(-0.5, stale), keyCap(ptr(10.0), ptr(0.0), t0.Add(-time.Minute), stale))), false, "account balance -$0.50"},
		{"unknown remaining with a cap is not treated as exhausted", ptr(orSnap("or", credits(5, t0), keyCap(ptr(10.0), nil, time.Time{}, t0))), true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for _, class := range []core.Class{core.ClassInteractive, core.ClassBackground} {
				h := newHarness([]core.Account{orAcct("or")}, Options{InflightEstimate: 0.01, SafetyMargin: 0.05})
				if tc.snap != nil {
					h.q.Set(*tc.snap)
				}
				l, d := h.p.Acquire(class, []string{"or"}, nil)
				if d.Allow != tc.allow || (l != nil) != tc.allow {
					t.Fatalf("%s: allow=%v want %v (%s)", class, d.Allow, tc.allow, d.Reason)
				}
				if tc.reason != "" && !strings.Contains(d.Reason, tc.reason) {
					t.Errorf("%s: reason %q missing %q", class, d.Reason, tc.reason)
				}
				st := h.p.Status("or")
				if st.InteractiveAdmissible != tc.allow && class == core.ClassInteractive {
					t.Errorf("status = %+v", st)
				}
			}
		})
	}
}

func TestOpenRouterExhaustedFailsOverToNextAccount(t *testing.T) {
	h := newHarness([]core.Account{orAcct("or"), acct("cx", 0)}, Options{})
	h.q.Set(orSnap("or", credits(-0.076117902, t0), nil))
	l, d := h.p.Acquire(core.ClassBackground, []string{"or", "cx"}, nil)
	if !d.Allow || l.AccountID() != "cx" || !strings.Contains(d.Reason, "or: openrouter account balance -$0.08") {
		t.Fatalf("decision = %+v", d)
	}
}

func TestOpenRouter402Cooldown(t *testing.T) {
	h := newHarness([]core.Account{orAcct("or")}, Options{})
	h.q.Set(orSnap("or", credits(5, t0), nil))
	l, _ := h.p.Acquire(core.ClassInteractive, []string{"or"}, nil)
	l.Release(core.Outcome{Status: 402, ResetAt: t0.Add(5 * time.Minute)})
	st := h.p.Status("or")
	if !st.CooldownUntil.Equal(t0.Add(5*time.Minute)) || st.InteractiveAdmissible {
		t.Fatalf("402 must cool the account down until Retry-After: %+v", st)
	}
	if r := h.q.Refreshes(); len(r) != 1 || !r[0].urgent {
		t.Errorf("402 must request an urgent refresh: %+v", r)
	}

	// An unchanged positive balance after the 402 is not proof the request
	// is affordable (e.g. max_tokens beyond the balance): keep cooling down.
	h.clock.Advance(10 * time.Second)
	h.q.Set(orSnap("or", credits(5, h.clock.Now()), keyCap(nil, nil, time.Time{}, h.clock.Now())))
	if _, d := h.p.Acquire(core.ClassInteractive, []string{"or"}, nil); d.Allow {
		t.Fatal("unchanged balance must not clear a 402 cooldown")
	}

	// A fresh top-up observed after the 402 clears the cooldown.
	h.clock.Advance(20 * time.Second)
	h.q.Set(orSnap("or", credits(20, h.clock.Now()), keyCap(nil, nil, time.Time{}, h.clock.Now())))
	if _, d := h.p.Acquire(core.ClassInteractive, []string{"or"}, nil); !d.Allow {
		t.Errorf("fresh top-up should clear cooldown: %s", d.Reason)
	}
}

func TestOpenRouter402CooldownNotClearedByKeyOnly(t *testing.T) {
	h := newHarness([]core.Account{orAcct("or")}, Options{})
	l, _ := h.p.Acquire(core.ClassInteractive, []string{"or"}, nil)
	l.Release(core.Outcome{Status: 402})
	h.clock.Advance(10 * time.Second)
	// /credits unavailable; only the key refreshed: not proof of funds.
	h.q.Set(orSnap("or", &core.Credits{Err: "x"}, keyCap(nil, nil, time.Time{}, h.clock.Now())))
	if _, d := h.p.Acquire(core.ClassInteractive, []string{"or"}, nil); d.Allow || !strings.Contains(d.Reason, "cooldown") {
		t.Errorf("decision = %+v", d)
	}
	h.clock.Advance(61 * time.Second)
	if _, d := h.p.Acquire(core.ClassInteractive, []string{"or"}, nil); !d.Allow {
		t.Errorf("cooldown should expire: %s", d.Reason)
	}
}

// 402 handling stays scoped to openrouter accounts.
func Test402OnOtherProvidersUnchanged(t *testing.T) {
	h := newHarness([]core.Account{acct("cx", 0)}, Options{})
	l, _ := h.p.Acquire(core.ClassInteractive, []string{"cx"}, nil)
	l.Release(core.Outcome{Status: 402})
	if st := h.p.Status("cx"); !st.CooldownUntil.IsZero() || !st.InteractiveAdmissible {
		t.Errorf("status = %+v", st)
	}
	if r := h.q.Refreshes(); len(r) != 1 || r[0].urgent {
		t.Errorf("refreshes = %+v", r)
	}
}

// Only 402 cooldowns clear on a top-up; a 429 rate limit is unrelated to the
// balance.
func TestOpenRouter429CooldownNotClearedByBalance(t *testing.T) {
	h := newHarness([]core.Account{orAcct("or")}, Options{})
	h.q.Set(orSnap("or", credits(5, t0), nil))
	l, _ := h.p.Acquire(core.ClassInteractive, []string{"or"}, nil)
	l.Release(core.Outcome{Status: 429})
	h.clock.Advance(10 * time.Second)
	h.q.Set(orSnap("or", credits(50, h.clock.Now()), nil))
	if _, d := h.p.Acquire(core.ClassInteractive, []string{"or"}, nil); d.Allow {
		t.Error("429 cooldown cleared by balance")
	}
}

// A predicted cap reset (LimitResetAt) is informational only. A stale key cap
// with zero remaining keeps denying BOTH classes after its computed reset has
// passed; only a fresh /key observation with positive remaining reopens. The
// account balance is independent: an exhausted balance denies on its own even
// when the key cap looks reset.
func TestOpenRouterStaleKeyCapResetDoesNotReopen(t *testing.T) {
	now := t0
	stale := t0.Add(-3 * time.Hour)
	classes := []core.Class{core.ClassInteractive, core.ClassBackground}

	// Stale snapshot: key cap is 10 in total, 0 remaining, reset already passed.
	h := newHarness([]core.Account{orAcct("or")}, Options{InflightEstimate: 0.01, SafetyMargin: 0.05})
	h.clock.now = now
	// The account balance is healthy and independent of the key cap.
	h.q.Set(orSnap("or", credits(50, stale), keyCap(ptr(10.0), ptr(0.0), t0.Add(-time.Minute), stale)))
	for _, class := range classes {
		if l, d := h.p.Acquire(class, []string{"or"}, nil); d.Allow || l != nil {
			t.Fatalf("%s: stale exhausted key at/past its predicted reset must deny: %+v", class, d)
		} else if !strings.Contains(d.Reason, "key spending cap exhausted") {
			t.Errorf("%s: reason = %q", class, d.Reason)
		}
	}
	if st := h.p.Status("or"); st.InteractiveAdmissible || st.BackgroundAdmissible {
		t.Fatalf("status must stay denied while the key cap is exhausted: %+v", st)
	}

	// A fresh /key observation with positive remaining reopens both classes.
	h.q.Set(orSnap("or", credits(50, now), keyCap(ptr(10.0), ptr(5.0), t0.Add(time.Hour), now)))
	for _, class := range classes {
		if _, d := h.p.Acquire(class, []string{"or"}, nil); !d.Allow {
			t.Fatalf("%s: fresh positive remaining must reopen: %s", class, d.Reason)
		}
	}

	// Balance independence: a fresh key cap that is exhausted by a zero cap
	// still denies, and a reobserved positive cap never overrides an exhausted
	// account balance.
	h2 := newHarness([]core.Account{orAcct("or")}, Options{InflightEstimate: 0.01})
	h2.q.Set(orSnap("or", credits(-0.5, now), keyCap(ptr(10.0), ptr(5.0), t0.Add(time.Hour), now)))
	if _, d := h2.p.Acquire(core.ClassInteractive, []string{"or"}, nil); d.Allow || !strings.Contains(d.Reason, "account balance") {
		t.Fatalf("known negative balance must deny regardless of a positive key cap: %+v", d)
	}
}
