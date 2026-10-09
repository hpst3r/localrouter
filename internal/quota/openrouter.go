package quota

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// OpenRouter account quota: GET <base_url>/credits (account-level signed USD
// balance) and GET <base_url>/key (the inference key's own spend and cap).
// Both endpoints derive from the account's base_url so credentials never go
// to another host. /credits uses the optional management credential when one
// is configured and the inference credential otherwise; /key always uses the
// inference credential.

// fetchOpenRouter refreshes both parts independently. A failed part keeps its
// last-good values and FetchedAt and records Err; the other part is still
// updated. prev is a private copy of the current snapshot (nil if none).
func (m *Manager) fetchOpenRouter(ctx context.Context, acct core.Account, prev *core.Snapshot) core.Snapshot {
	now := m.opts.Clock.Now()
	snap := core.Snapshot{AccountID: acct.ID, Source: SourceUsageAPI}
	if prev != nil {
		snap = *prev
	}
	snap.Source = SourceUsageAPI
	if snap.Credits == nil {
		snap.Credits = &core.Credits{}
	}
	if snap.Key == nil {
		snap.Key = &core.KeyUsage{}
	}
	base := strings.TrimRight(acct.BaseURL, "/")
	hdr := http.Header{"Accept": {"application/json"}}

	if c, err := m.fetchORCredits(ctx, acct.ID, base+"/credits", hdr); err != nil {
		snap.Credits.Err = err.Error()
	} else {
		c.FetchedAt = now
		*snap.Credits = c
	}
	if k, err := m.fetchORKey(ctx, acct.ID, base+"/key", hdr, now); err != nil {
		snap.Key.Err = err.Error()
	} else {
		k.FetchedAt = now
		*snap.Key = k
	}

	// The account is only as fresh as its oldest known part.
	snap.FetchedAt = time.Time{}
	for _, t := range []time.Time{snap.Credits.FetchedAt, snap.Key.FetchedAt} {
		if !t.IsZero() && (snap.FetchedAt.IsZero() || t.Before(snap.FetchedAt)) {
			snap.FetchedAt = t
		}
	}
	var errs []string
	if snap.Credits.Err != "" {
		errs = append(errs, snap.Credits.Err)
	}
	if snap.Key.Err != "" {
		errs = append(errs, snap.Key.Err)
	}
	snap.Err = strings.Join(errs, "; ")
	return snap
}

func (m *Manager) fetchORCredits(ctx context.Context, id, url string, hdr http.Header) (core.Credits, error) {
	var mgmt core.CredentialSource
	if m.opts.ManagementCredentials != nil {
		mgmt = m.opts.ManagementCredentials(id)
	}
	creds := m.creds
	if mgmt != nil {
		creds = mgmt
	}
	body, err := m.getWith(ctx, creds, id, url, hdr)
	if err != nil {
		var se *httpStatusError
		switch {
		case mgmt != nil && errors.Is(err, errCredential):
			return core.Credits{}, errors.New("credits: management key unavailable")
		case mgmt != nil && errors.As(err, &se) && (se.code == http.StatusUnauthorized || se.code == http.StatusForbidden):
			return core.Credits{}, fmt.Errorf("credits: management key rejected (http %d)", se.code)
		case mgmt == nil && errors.As(err, &se) && se.code == http.StatusForbidden:
			return core.Credits{}, errors.New("credits: management key required (http 403); balance unavailable")
		}
		return core.Credits{}, fmt.Errorf("credits: %w", err)
	}
	var env struct {
		Data *struct {
			TotalCredits *float64 `json:"total_credits"`
			TotalUsage   *float64 `json:"total_usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Data == nil {
		return core.Credits{}, fmt.Errorf("credits: %w", errMalformed)
	}
	tc, tu := env.Data.TotalCredits, env.Data.TotalUsage
	if !nonNegative(tc) || !nonNegative(tu) {
		return core.Credits{}, fmt.Errorf("credits: %w", errMalformed)
	}
	// Signed: usage beyond purchased credit is a real negative balance.
	return core.Credits{TotalCreditsUSD: *tc, TotalUsageUSD: *tu, BalanceUSD: *tc - *tu}, nil
}

func (m *Manager) fetchORKey(ctx context.Context, id, url string, hdr http.Header, now time.Time) (core.KeyUsage, error) {
	body, err := m.get(ctx, id, url, hdr)
	if err != nil {
		return core.KeyUsage{}, fmt.Errorf("key: %w", err)
	}
	k, ok := parseORKey(body, now)
	if !ok {
		return core.KeyUsage{}, fmt.Errorf("key: %w", errMalformed)
	}
	return k, nil
}

// parseORKey validates a /key body. limit and limit_remaining must be present
// (null = no cap); usage must be a non-negative number. Optional counters are
// nil when absent or null. Unknown fields (including the deprecated
// rate_limit and the key label) are ignored.
func parseORKey(body []byte, now time.Time) (core.KeyUsage, bool) {
	var env struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Data == nil {
		return core.KeyUsage{}, false
	}
	f := orFields(env.Data)
	var k core.KeyUsage
	limit, present, ok := f.num("limit")
	if !ok || !present || (limit != nil && *limit < 0) {
		return k, false
	}
	remaining, present, ok := f.num("limit_remaining")
	if !ok || !present {
		return k, false
	}
	usage, _, ok := f.num("usage")
	if !ok || !nonNegative(usage) {
		return k, false
	}
	k.LimitUSD, k.LimitRemainingUSD, k.UsageUSD = limit, remaining, *usage
	for key, dst := range map[string]**float64{
		"usage_daily": &k.UsageDailyUSD, "usage_weekly": &k.UsageWeeklyUSD, "usage_monthly": &k.UsageMonthlyUSD,
		"byok_usage": &k.BYOKUsageUSD, "byok_usage_daily": &k.BYOKUsageDailyUSD,
		"byok_usage_weekly": &k.BYOKUsageWeeklyUSD, "byok_usage_monthly": &k.BYOKUsageMonthlyUSD,
	} {
		v, _, ok := f.num(key)
		if !ok || (v != nil && *v < 0) {
			return k, false
		}
		*dst = v
	}
	for key, dst := range map[string]**bool{"include_byok_in_limit": &k.IncludeBYOKInLimit, "is_free_tier": &k.IsFreeTier} {
		v, ok := f.boolean(key)
		if !ok {
			return k, false
		}
		*dst = v
	}
	reset, ok := f.str("limit_reset")
	if !ok {
		return k, false
	}
	k.LimitReset = reset
	k.LimitResetAt = nextLimitReset(reset, now)
	return k, true
}

// nextLimitReset is the next cap reset after now. OpenRouter resets key caps
// at midnight UTC; weeks run Monday to Sunday. Unknown periods yield zero.
func nextLimitReset(period string, now time.Time) time.Time {
	now = now.UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	switch period {
	case "daily":
		return day.AddDate(0, 0, 1)
	case "weekly":
		days := (8 - int(now.Weekday())) % 7 // days until next Monday
		if days == 0 {
			days = 7
		}
		return day.AddDate(0, 0, days)
	case "monthly":
		return time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC)
	}
	return time.Time{}
}

func nonNegative(p *float64) bool { return p != nil && *p >= 0 }

type orFields map[string]json.RawMessage

func isNull(r json.RawMessage) bool { return string(bytes.TrimSpace(r)) == "null" }

// num decodes an optional numeric field: nil for absent or null. ok is false
// for a non-number or non-finite value.
func (f orFields) num(k string) (v *float64, present, ok bool) {
	r, present := f[k]
	if !present || isNull(r) {
		return nil, present, true
	}
	var x float64
	if err := json.Unmarshal(r, &x); err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
		return nil, true, false
	}
	return &x, true, true
}

func (f orFields) boolean(k string) (*bool, bool) {
	r, present := f[k]
	if !present || isNull(r) {
		return nil, true
	}
	var b bool
	if err := json.Unmarshal(r, &b); err != nil {
		return nil, false
	}
	return &b, true
}

func (f orFields) str(k string) (string, bool) {
	r, present := f[k]
	if !present || isNull(r) {
		return "", true
	}
	var s string
	if err := json.Unmarshal(r, &s); err != nil {
		return "", false
	}
	return s, true
}
