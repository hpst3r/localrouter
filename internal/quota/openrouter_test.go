package quota

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// The user's real example: purchased 770.8176, cumulative usage 770.893717902.
const (
	userCredits = `{"data":{"total_credits":770.8176,"total_usage":770.893717902}}`
	userBalance = 770.8176 - 770.893717902 // -0.076117902
	keyBody     = `{"data":{"label":"sk-or-v1-abc...xyz","limit":null,"limit_remaining":null,"limit_reset":null,
		"include_byok_in_limit":false,"usage":770.89,"usage_daily":1.25,"usage_weekly":7.5,"usage_monthly":30.25,
		"byok_usage":2,"byok_usage_daily":0.5,"byok_usage_weekly":1,"byok_usage_monthly":1.5,"is_free_tier":false,
		"rate_limit":{"requests":-1,"interval":"10s"},"some_future_field":{"x":1}}}`
	mgmtToken = "sk-or-MGMT-secret-999"
)

// staticCreds is a fake credential source with a fixed bearer token.
type staticCreds struct {
	mu          sync.Mutex
	token       string
	invalidated int
}

func (s *staticCreds) Credential(context.Context, string) (core.Credential, error) {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+s.token)
	return core.Credential{Headers: h, Identity: "x"}, nil
}

func (s *staticCreds) Invalidate(string) {
	s.mu.Lock()
	s.invalidated++
	s.mu.Unlock()
}

type orHarness struct {
	m     *Manager
	clock *fakeClock
	creds *fakeCreds
	mgmt  *staticCreds
	logs  *bytes.Buffer

	mu    sync.Mutex
	auths map[string][]string // path -> Authorization headers seen
	// handlers answer /api/v1/credits and /api/v1/key; swap between refreshes.
	credits, key http.HandlerFunc
}

func (h *orHarness) set(credits, key http.HandlerFunc) {
	h.mu.Lock()
	h.credits, h.key = credits, key
	h.mu.Unlock()
}

func (h *orHarness) seen(path string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.auths[path]...)
}

func body(s string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte(s)) }
}

func status(code int, s string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code); w.Write([]byte(s)) }
}

// newORHarness serves a fake OpenRouter at <srv>/api/v1. withMgmt configures
// a management credential for the account.
func newORHarness(t *testing.T, withMgmt bool) *orHarness {
	t.Helper()
	h := &orHarness{clock: &fakeClock{t: t0}, creds: &fakeCreds{}, mgmt: &staticCreds{token: mgmtToken},
		logs: &bytes.Buffer{}, auths: map[string][]string{}}
	h.credits, h.key = body(userCredits), body(keyBody)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.auths[r.URL.Path] = append(h.auths[r.URL.Path], r.Header.Get("Authorization"))
		credits, key := h.credits, h.key
		h.mu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("method %s", r.Method)
		}
		switch r.URL.Path {
		case "/api/v1/credits":
			credits(w, r)
		case "/api/v1/key":
			key(w, r)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	opts := Options{
		Clock:        h.clock,
		HTTPClient:   srv.Client(),
		Logger:       slog.New(slog.NewTextHandler(h.logs, nil)),
		PollInterval: time.Hour,
	}
	if withMgmt {
		opts.ManagementCredentials = func(id string) core.CredentialSource {
			if id == "or" {
				return h.mgmt
			}
			return nil
		}
	}
	h.m = New([]core.Account{{ID: "or", Provider: core.ProviderOpenRouter, BaseURL: srv.URL + "/api/v1"}}, h.creds, opts)
	return h
}

func (h *orHarness) refresh(t *testing.T) core.Snapshot {
	t.Helper()
	h.m.refresh(context.Background(), "or", true)
	s, ok := h.m.Latest("or")
	if !ok {
		t.Fatal("no snapshot")
	}
	return s
}

func eqp(p *float64, want float64) bool { return p != nil && math.Abs(*p-want) < 1e-9 }

func TestOpenRouterUserExampleSignedBalance(t *testing.T) {
	h := newORHarness(t, false)
	s := h.refresh(t)
	if s.Credits == nil || s.Key == nil {
		t.Fatalf("missing parts: %+v", s)
	}
	c := s.Credits
	if !approx(c.TotalCreditsUSD, 770.8176) || !approx(c.TotalUsageUSD, 770.893717902) || !approx(c.BalanceUSD, userBalance) {
		t.Errorf("credits = %+v", c)
	}
	if math.Abs(c.BalanceUSD-(-0.076117902)) > 1e-9 || c.BalanceUSD >= 0 {
		t.Errorf("balance must stay signed negative, got %v", c.BalanceUSD)
	}
	if !c.FetchedAt.Equal(t0) || c.Err != "" || s.Err != "" || !s.FetchedAt.Equal(t0) || s.Source != SourceUsageAPI {
		t.Errorf("freshness: credits=%+v snap=%+v", c, s)
	}
	if len(s.Windows) != 0 || s.Allowed != nil {
		t.Errorf("no fake windows/allowed for openrouter: %+v", s)
	}
}

func TestOpenRouterKeyFields(t *testing.T) {
	h := newORHarness(t, false)
	k := h.refresh(t).Key
	if k.LimitUSD != nil || k.LimitRemainingUSD != nil || k.LimitReset != "" || !k.LimitResetAt.IsZero() {
		t.Errorf("null limit must be unlimited: %+v", k)
	}
	if !approx(k.UsageUSD, 770.89) || !eqp(k.UsageDailyUSD, 1.25) || !eqp(k.UsageWeeklyUSD, 7.5) || !eqp(k.UsageMonthlyUSD, 30.25) {
		t.Errorf("usage = %+v", k)
	}
	// BYOK kept separate, never summed into usage.
	if !eqp(k.BYOKUsageUSD, 2) || !eqp(k.BYOKUsageDailyUSD, 0.5) || !eqp(k.BYOKUsageWeeklyUSD, 1) || !eqp(k.BYOKUsageMonthlyUSD, 1.5) {
		t.Errorf("byok = %+v", k)
	}
	if k.IncludeBYOKInLimit == nil || *k.IncludeBYOKInLimit || k.IsFreeTier == nil || *k.IsFreeTier {
		t.Errorf("bools = %+v", k)
	}
	if !k.FetchedAt.Equal(t0) || k.Err != "" {
		t.Errorf("key freshness %+v", k)
	}
}

func TestOpenRouterZeroCapAndDailyRemaining(t *testing.T) {
	h := newORHarness(t, false)
	h.set(body(userCredits), body(`{"data":{"limit":0,"limit_remaining":0,"limit_reset":null,"usage":3}}`))
	k := h.refresh(t).Key
	if !eqp(k.LimitUSD, 0) || !eqp(k.LimitRemainingUSD, 0) {
		t.Errorf("zero cap must be preserved, got %+v", k)
	}
	// Optional counters absent: unknown (nil), not zero.
	if k.UsageDailyUSD != nil || k.BYOKUsageUSD != nil || k.IsFreeTier != nil {
		t.Errorf("missing optional fields became values: %+v", k)
	}

	// Daily cap: remaining is authoritative, never cap - lifetime usage.
	h.set(body(userCredits), body(`{"data":{"limit":10,"limit_remaining":9,"limit_reset":"daily","usage":500,"usage_daily":1}}`))
	k = h.refresh(t).Key
	if !eqp(k.LimitUSD, 10) || !eqp(k.LimitRemainingUSD, 9) || k.LimitReset != "daily" {
		t.Errorf("daily cap = %+v", k)
	}
	if want := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC); !k.LimitResetAt.Equal(want) {
		t.Errorf("daily reset at %v, want %v", k.LimitResetAt, want)
	}
}

func TestOpenRouterLimitResetAt(t *testing.T) {
	for reset, want := range map[string]time.Time{
		"daily":   time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC),
		"weekly":  time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), // t0 is a Thursday; weeks start Monday
		"monthly": time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC),
		"yearly":  {}, // unknown period: no computed reset
	} {
		h := newORHarness(t, false)
		h.set(body(userCredits), body(`{"data":{"limit":5,"limit_remaining":0,"limit_reset":"`+reset+`","usage":1}}`))
		if got := h.refresh(t).Key.LimitResetAt; !got.Equal(want) {
			t.Errorf("%s: reset at %v, want %v", reset, got, want)
		}
	}
}

func TestOpenRouterCreditsMalformed(t *testing.T) {
	for _, bad := range []string{
		`not json`,
		`{}`,
		`{"data":null}`,
		`{"data":[]}`,
		`{"data":{"total_credits":10}}`,
		`{"data":{"total_usage":10}}`,
		`{"data":{"total_credits":null,"total_usage":1}}`,
		`{"data":{"total_credits":"10","total_usage":1}}`,
		`{"data":{"total_credits":-1,"total_usage":1}}`,
		`{"data":{"total_credits":10,"total_usage":-0.5}}`,
		`{"data":{"total_credits":1e999,"total_usage":1}}`,
	} {
		h := newORHarness(t, false)
		h.set(body(bad), body(keyBody))
		s := h.refresh(t)
		if s.Credits == nil || !s.Credits.FetchedAt.IsZero() || !strings.Contains(s.Credits.Err, "malformed") {
			t.Errorf("%s: credits = %+v", bad, s.Credits)
		}
		if s.Key == nil || !s.Key.FetchedAt.Equal(t0) {
			t.Errorf("%s: key should still be fetched: %+v", bad, s.Key)
		}
		if !strings.Contains(s.Err, "credits") {
			t.Errorf("%s: snapshot err %q", bad, s.Err)
		}
	}
}

func TestOpenRouterKeyMalformed(t *testing.T) {
	for _, bad := range []string{
		`{"data":null}`,
		`{"data":{"limit":null,"limit_remaining":null}}`,            // usage missing
		`{"data":{"limit_remaining":null,"usage":1}}`,               // limit missing
		`{"data":{"limit":null,"usage":1}}`,                         // limit_remaining missing
		`{"data":{"limit":-5,"limit_remaining":0,"usage":1}}`,       // negative cap
		`{"data":{"limit":null,"limit_remaining":null,"usage":-1}}`, // negative cumulative usage
		`{"data":{"limit":null,"limit_remaining":null,"usage":1,"usage_daily":-2}}`,
		`{"data":{"limit":null,"limit_remaining":null,"usage":"1"}}`,
	} {
		h := newORHarness(t, false)
		h.set(body(userCredits), body(bad))
		s := h.refresh(t)
		if s.Key == nil || !s.Key.FetchedAt.IsZero() || !strings.Contains(s.Key.Err, "malformed") {
			t.Errorf("%s: key = %+v", bad, s.Key)
		}
		if s.Credits == nil || !approx(s.Credits.BalanceUSD, userBalance) {
			t.Errorf("%s: credits should still be fetched: %+v", bad, s.Credits)
		}
	}
}

// Without a management key the inference key is tried on /credits; a 403
// leaves the balance unavailable but /key is still used.
func TestOpenRouterCredits403WithoutManagementKey(t *testing.T) {
	h := newORHarness(t, false)
	h.set(status(403, `{"error":{"message":"Only management keys can perform this operation `+secretToken+`"}}`), body(keyBody))
	s := h.refresh(t)
	if s.Credits == nil || !s.Credits.FetchedAt.IsZero() || s.Credits.BalanceUSD != 0 {
		t.Fatalf("credits must be unavailable, not invented: %+v", s.Credits)
	}
	if !strings.Contains(s.Credits.Err, "management key required") {
		t.Errorf("credits err = %q", s.Credits.Err)
	}
	if s.Key == nil || !s.Key.FetchedAt.Equal(t0) || s.Key.Err != "" {
		t.Errorf("key must still work: %+v", s.Key)
	}
	if s.Err == "" {
		t.Error("snapshot must not claim full health")
	}
	for _, leak := range []string{secretToken, "Only management keys"} {
		if strings.Contains(s.Err+s.Credits.Err+h.logs.String(), leak) {
			t.Errorf("leaked %q", leak)
		}
	}
	// A 403 is not an auth refresh trigger for the inference key.
	if h.creds.invalidated != 0 {
		t.Errorf("invalidated %d", h.creds.invalidated)
	}
}

func TestOpenRouterManagementKeyIsolation(t *testing.T) {
	h := newORHarness(t, true)
	s := h.refresh(t)
	if !approx(s.Credits.BalanceUSD, userBalance) || s.Err != "" {
		t.Fatalf("snap = %+v", s)
	}
	for _, a := range h.seen("/api/v1/credits") {
		if a != "Bearer "+mgmtToken {
			t.Errorf("/credits used %q, want management key only", a)
		}
	}
	keyAuths := h.seen("/api/v1/key")
	if len(keyAuths) == 0 {
		t.Fatal("/key not fetched")
	}
	for _, a := range keyAuths {
		if strings.Contains(a, mgmtToken) || !strings.Contains(a, secretToken) {
			t.Errorf("/key used %q, want inference key only", a)
		}
	}
	if strings.Contains(h.logs.String(), mgmtToken) || strings.Contains(h.logs.String(), secretToken) {
		t.Error("credential in logs")
	}
}

// An explicitly configured management key that fails is surfaced, not masked
// by falling back to the inference key.
func TestOpenRouterManagementKeyFailureSurfaced(t *testing.T) {
	h := newORHarness(t, true)
	h.set(status(401, "nope"), body(keyBody))
	s := h.refresh(t)
	if !strings.Contains(s.Credits.Err, "management key") || !strings.Contains(s.Credits.Err, "401") || !s.Credits.FetchedAt.IsZero() {
		t.Errorf("credits = %+v", s.Credits)
	}
	for _, a := range h.seen("/api/v1/credits") {
		if a != "Bearer "+mgmtToken {
			t.Errorf("fell back to %q", a)
		}
	}
	h.mgmt.mu.Lock()
	inv := h.mgmt.invalidated
	h.mgmt.mu.Unlock()
	if inv != 1 || h.creds.invalidated != 0 {
		t.Errorf("401 must invalidate only the management key: mgmt=%d inference=%d", inv, h.creds.invalidated)
	}
	if len(h.seen("/api/v1/credits")) != 2 {
		t.Errorf("want one retry after 401, got %d", len(h.seen("/api/v1/credits")))
	}
}

// A later failure keeps last-good values and timestamps. A successful /key
// never refreshes (or clears) a previously known exhausted balance.
func TestOpenRouterPartialFailureKeepsExhaustedCredits(t *testing.T) {
	h := newORHarness(t, false)
	h.set(body(`{"data":{"total_credits":10,"total_usage":10.5}}`), body(keyBody))
	h.refresh(t)
	h.clock.Advance(5 * time.Minute)
	t1 := h.clock.Now()
	h.set(status(500, "boom"), body(`{"data":{"limit":null,"limit_remaining":null,"usage":11}}`))
	s := h.refresh(t)
	c := s.Credits
	if !approx(c.BalanceUSD, -0.5) || !c.FetchedAt.Equal(t0) || !strings.Contains(c.Err, "500") {
		t.Errorf("credits must keep last-good exhausted balance and old timestamp: %+v", c)
	}
	if !approx(s.Key.UsageUSD, 11) || !s.Key.FetchedAt.Equal(t1) || s.Key.Err != "" {
		t.Errorf("key = %+v", s.Key)
	}
	if !s.FetchedAt.Equal(t0) {
		t.Errorf("snapshot FetchedAt %v must stay at the oldest part (%v)", s.FetchedAt, t0)
	}
	if !strings.Contains(s.Err, "credits") || strings.Contains(s.Err, "key:") {
		t.Errorf("snap err = %q", s.Err)
	}

	// Both fail: everything last-good, both errors reported.
	h.clock.Advance(time.Minute)
	h.set(status(502, ""), status(503, ""))
	s = h.refresh(t)
	if !approx(s.Credits.BalanceUSD, -0.5) || !approx(s.Key.UsageUSD, 11) || !s.Key.FetchedAt.Equal(t1) {
		t.Errorf("last-good lost: %+v %+v", s.Credits, s.Key)
	}
	if !strings.Contains(s.Err, "credits") || !strings.Contains(s.Err, "key") {
		t.Errorf("snap err = %q", s.Err)
	}

	// Top-up recovers.
	h.set(body(`{"data":{"total_credits":30,"total_usage":10.5}}`), body(keyBody))
	s = h.refresh(t)
	if !approx(s.Credits.BalanceUSD, 19.5) || s.Credits.Err != "" || s.Err != "" {
		t.Errorf("after top-up: %+v err=%q", s.Credits, s.Err)
	}
}

func TestOpenRouterFirstFetchTransportFailure(t *testing.T) {
	h := newORHarness(t, false)
	h.set(status(500, ""), status(500, ""))
	s := h.refresh(t)
	if !s.FetchedAt.IsZero() || s.Credits == nil || s.Key == nil || !s.Credits.FetchedAt.IsZero() || !s.Key.FetchedAt.IsZero() {
		t.Errorf("first failure must not fabricate data: %+v", s)
	}
	if s.Err == "" {
		t.Error("err not set")
	}
}

func TestOpenRouterLatestDeepCopy(t *testing.T) {
	h := newORHarness(t, false)
	h.set(body(userCredits), body(`{"data":{"limit":10,"limit_remaining":9,"limit_reset":"daily","usage":1,"usage_daily":1,"usage_weekly":1,"usage_monthly":1,
		"byok_usage":1,"byok_usage_daily":1,"byok_usage_weekly":1,"byok_usage_monthly":1,"include_byok_in_limit":true,"is_free_tier":false}}`))
	s := h.refresh(t)
	s.Credits.BalanceUSD = 999
	*s.Key.LimitUSD, *s.Key.LimitRemainingUSD = 1, 1
	for _, p := range []*float64{s.Key.UsageDailyUSD, s.Key.UsageWeeklyUSD, s.Key.UsageMonthlyUSD, s.Key.BYOKUsageUSD,
		s.Key.BYOKUsageDailyUSD, s.Key.BYOKUsageWeeklyUSD, s.Key.BYOKUsageMonthlyUSD} {
		*p = 42
	}
	*s.Key.IncludeBYOKInLimit, *s.Key.IsFreeTier = false, true
	s.Key.UsageUSD = 42
	again, _ := h.m.Latest("or")
	k := again.Key
	if !approx(again.Credits.BalanceUSD, userBalance) || !eqp(k.LimitUSD, 10) || !eqp(k.LimitRemainingUSD, 9) ||
		!eqp(k.UsageDailyUSD, 1) || !eqp(k.UsageWeeklyUSD, 1) || !eqp(k.UsageMonthlyUSD, 1) || !eqp(k.BYOKUsageUSD, 1) ||
		!eqp(k.BYOKUsageDailyUSD, 1) || !eqp(k.BYOKUsageWeeklyUSD, 1) || !eqp(k.BYOKUsageMonthlyUSD, 1) ||
		!*k.IncludeBYOKInLimit || *k.IsFreeTier || !approx(k.UsageUSD, 1) {
		t.Errorf("Latest shares state with caller: %+v %+v", again.Credits, k)
	}
}

// Concurrent readers, refreshes and header observations must be race-free.
func TestOpenRouterConcurrentLatest(t *testing.T) {
	h := newORHarness(t, true)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); h.m.RequestRefresh("or", true) }()
		go func() {
			defer wg.Done()
			if s, ok := h.m.Latest("or"); ok && s.Key != nil && s.Key.UsageDailyUSD != nil {
				_ = *s.Key.UsageDailyUSD
			}
			h.m.ObserveHeaders("or", http.Header{"X-Codex-Primary-Used-Percent": {"50"}})
		}()
	}
	wg.Wait()
	h.m.wait()
	s, _ := h.m.Latest("or")
	if len(s.Windows) != 0 {
		t.Errorf("headers must not add windows to openrouter: %+v", s.Windows)
	}
}

// The credential only goes to the account's own base_url host.
func TestOpenRouterEndpointsDeriveFromBaseURL(t *testing.T) {
	h := newORHarness(t, true)
	h.refresh(t)
	if len(h.seen("/api/v1/credits")) != 1 || len(h.seen("/api/v1/key")) != 1 {
		t.Errorf("credits=%v key=%v", h.seen("/api/v1/credits"), h.seen("/api/v1/key"))
	}
}
