package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/core"
)

// budgetClock is a fixed clock for the budget-read tests.
type budgetClock struct{ t time.Time }

func (c budgetClock) Now() time.Time { return c.t }

// fakeBudgetSource is a controllable BudgetSource: it counts reads and records
// the deadline the server passes, so a test can prove the store is never
// touched on the auth/validation paths and that every read is bounded.
type fakeBudgetSource struct {
	limits []budget.Limit
	hold   int64
	snaps  map[string]budget.Snapshot
	err    error

	calls       int
	hasDeadline bool
	deadline    time.Duration
}

func (f *fakeBudgetSource) Snapshot(ctx context.Context, scope, key, period string, _ time.Time) (budget.Snapshot, error) {
	f.calls++
	if dl, ok := ctx.Deadline(); ok {
		f.hasDeadline = true
		f.deadline = time.Until(dl)
	}
	if f.err != nil {
		return budget.Snapshot{}, f.err
	}
	return f.snaps[scope+"|"+key+"|"+period], nil
}

func (f *fakeBudgetSource) Limits() []budget.Limit { return f.limits }

func (f *fakeBudgetSource) ReservationMicros() int64 { return f.hold }

func budgetTestServer(t *testing.T, now time.Time, src BudgetSource, requireAuth bool) *Server {
	t.Helper()
	deps := Deps{
		Accounts: []core.Account{{ID: "acct"}},
		Clients:  []ClientInfo{{Name: "ide", Class: "interactive"}},
		Budgets:  src,
		Clock:    budgetClock{now},
	}
	if requireAuth {
		deps.Authenticate = func(bearer string) (core.Client, bool) {
			if bearer == "good-key" {
				return core.Client{Name: "ide"}, true
			}
			return core.Client{}, false
		}
	}
	return New(deps, Options{RequireAuth: requireAuth})
}

func budgetGet(t *testing.T, h http.Handler, target, key string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeBudgetDoc(t *testing.T, rec *httptest.ResponseRecorder) budgetDoc {
	t.Helper()
	var doc budgetDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return doc
}

// TestBudgetsDisabledSourceIsNilSafe pins the disabled shape: a nil source is a
// 200 {schema_version:1,enabled:false} with no money and no store access, and
// the response is never cached.
func TestBudgetsDisabledSourceIsNilSafe(t *testing.T) {
	srv := budgetTestServer(t, time.Now(), nil, false)
	rec := budgetGet(t, srv.Handler(), "/control/v1/budgets", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"schema_version":1,"enabled":false}` {
		t.Fatalf("disabled body = %s", got)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
}

// TestBudgetsAuthRejectsBeforeAnyRead proves auth runs first: a bad key is a
// 401 and the store is never read.
func TestBudgetsAuthRejectsBeforeAnyRead(t *testing.T) {
	src := &fakeBudgetSource{}
	srv := budgetTestServer(t, time.Now(), src, true)
	rec := budgetGet(t, srv.Handler(), "/control/v1/budgets?scope=client&key=ide", "bad-key")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401: %s", rec.Code, rec.Body.String())
	}
	if src.calls != 0 {
		t.Fatalf("store read on rejected auth: %d", src.calls)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q", rec.Header().Get("Cache-Control"))
	}
}

// TestBudgetsUnknownScopeOrKeyNeverReads proves an unnameable identity is
// rejected before the store is touched: unknown keys are 404, malformed queries
// 400.
func TestBudgetsUnknownScopeOrKeyNeverReads(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name, target string
		want         int
	}{
		{"unknown client", "/control/v1/budgets?scope=client&key=nope", http.StatusNotFound},
		{"unknown account", "/control/v1/budgets?scope=account&key=nope", http.StatusNotFound},
		{"bad scope", "/control/v1/budgets?scope=bogus&key=ide", http.StatusBadRequest},
		{"missing key", "/control/v1/budgets?scope=client", http.StatusBadRequest},
		{"bad period", "/control/v1/budgets?scope=client&key=ide&period=week", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeBudgetSource{}
			srv := budgetTestServer(t, now, src, false)
			rec := budgetGet(t, srv.Handler(), tc.target, "")
			if rec.Code != tc.want {
				t.Fatalf("code = %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if src.calls != 0 {
				t.Fatalf("store read on rejected query: %d", src.calls)
			}
		})
	}
}

// TestBudgetsUTCDayAndMonthBoundaries proves every boundary is derived from one
// UTC instant: the clock is 23:59:59 in a +09:00 zone, so the day instance is
// the UTC day, and the month rollover lands on the first of the next month.
func TestBudgetsUTCDayAndMonthBoundaries(t *testing.T) {
	t.Run("day before UTC midnight", func(t *testing.T) {
		clk := time.Date(2026, 3, 31, 23, 59, 59, 0, time.FixedZone("plus9", 9*3600))
		srv := budgetTestServer(t, clk, &fakeBudgetSource{}, false)
		rec := budgetGet(t, srv.Handler(), "/control/v1/budgets?scope=client&key=ide", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
		}
		doc := decodeBudgetDoc(t, rec)
		if len(doc.Rows) != 2 {
			t.Fatalf("rows = %d, want 2", len(doc.Rows))
		}
		day, month := doc.Rows[0], doc.Rows[1]
		if day.Scope != budget.ScopeClient || day.Key != "ide" || day.Period != budget.PeriodDay {
			t.Fatalf("day row identity = %+v", day)
		}
		if month.Period != budget.PeriodMonth {
			t.Fatalf("month period = %q", month.Period)
		}
		if want := time.Date(2026, 3, 31, 0, 0, 0, 0, time.UTC); !day.Start.Equal(want) {
			t.Fatalf("day start = %s, want %s", day.Start, want)
		}
		if want := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC); !day.Reset.Equal(want) {
			t.Fatalf("day reset = %s, want %s", day.Reset, want)
		}
		if want := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC); !month.Start.Equal(want) {
			t.Fatalf("month start = %s, want %s", month.Start, want)
		}
		if want := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC); !month.Reset.Equal(want) {
			t.Fatalf("month reset = %s, want %s", month.Reset, want)
		}
	})

	t.Run("month rolls into next year", func(t *testing.T) {
		clk := time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC)
		srv := budgetTestServer(t, clk, &fakeBudgetSource{}, false)
		rec := budgetGet(t, srv.Handler(), "/control/v1/budgets?scope=client&key=ide&period=month", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
		}
		doc := decodeBudgetDoc(t, rec)
		if len(doc.Rows) != 1 {
			t.Fatalf("rows = %d, want 1", len(doc.Rows))
		}
		row := doc.Rows[0]
		if want := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC); !row.Start.Equal(want) {
			t.Fatalf("start = %s, want %s", row.Start, want)
		}
		if want := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC); !row.Reset.Equal(want) {
			t.Fatalf("reset = %s, want %s", row.Reset, want)
		}
	})
}

// TestBudgetsExplicitZeroVsUnlimited proves the pointer encoding: an explicit
// "0" day budget is reported (and is available 0), while an absent month budget
// omits limit/available entirely.
func TestBudgetsExplicitZeroVsUnlimited(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	src := &fakeBudgetSource{
		hold:   1_000_000,
		limits: []budget.Limit{{Scope: budget.ScopeClient, Key: "ide", Period: budget.PeriodDay, Micros: 0}},
	}
	srv := budgetTestServer(t, now, src, false)
	rec := budgetGet(t, srv.Handler(), "/control/v1/budgets?scope=client&key=ide", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	doc := decodeBudgetDoc(t, rec)
	if doc.SchemaVersion != SchemaVersion || !doc.Enabled {
		t.Fatalf("doc header = %+v", doc)
	}
	if doc.ReserveMicros != 1_000_000 {
		t.Fatalf("reserve_micros = %d, want 1000000", doc.ReserveMicros)
	}
	if len(doc.Rows) != 2 {
		t.Fatalf("rows = %d", len(doc.Rows))
	}
	day, month := doc.Rows[0], doc.Rows[1]
	if day.LimitMicros == nil || *day.LimitMicros != 0 {
		t.Fatalf("day limit = %v, want explicit 0", day.LimitMicros)
	}
	if day.LimitUSD == nil || *day.LimitUSD != "0.000000" {
		t.Fatalf("day limit_usd = %v", day.LimitUSD)
	}
	if day.AvailableMicros == nil || *day.AvailableMicros != 0 {
		t.Fatalf("day available = %v, want 0", day.AvailableMicros)
	}
	if month.LimitMicros != nil || month.LimitUSD != nil || month.AvailableMicros != nil || month.AvailableUSD != nil {
		t.Fatalf("unlimited month must omit limit/available: %+v", month)
	}
	if n := strings.Count(rec.Body.String(), "limit_micros"); n != 1 {
		t.Fatalf("limit_micros appears %d times in %s", n, rec.Body.String())
	}
}

// TestBudgetsOverrunKeepsNegativeAvailable proves available is signed and never
// clamped: a settled overrun reports a negative available with its exact USD
// rendering.
func TestBudgetsOverrunKeepsNegativeAvailable(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	src := &fakeBudgetSource{
		limits: []budget.Limit{{Scope: budget.ScopeClient, Key: "ide", Period: budget.PeriodDay, Micros: 1_000_000}},
		snaps: map[string]budget.Snapshot{
			"client|ide|day": {Reported: 1_400_000, Reserved: 200_000},
		},
	}
	srv := budgetTestServer(t, now, src, false)
	rec := budgetGet(t, srv.Handler(), "/control/v1/budgets?scope=client&key=ide&period=day", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	doc := decodeBudgetDoc(t, rec)
	if len(doc.Rows) != 1 {
		t.Fatalf("rows = %d", len(doc.Rows))
	}
	row := doc.Rows[0]
	if row.ReportedMicros != 1_400_000 || row.ReportedUSD != "1.400000" {
		t.Fatalf("reported = %d/%q", row.ReportedMicros, row.ReportedUSD)
	}
	if row.ReservedMicros != 200_000 || row.ReservedUSD != "0.200000" {
		t.Fatalf("reserved = %d/%q", row.ReservedMicros, row.ReservedUSD)
	}
	// spent = 1.4 + 0.2 = 1.6; limit 1.0 -> available -0.6, unclamped.
	if row.AvailableMicros == nil || *row.AvailableMicros != -600_000 {
		t.Fatalf("available = %v, want -600000", row.AvailableMicros)
	}
	if row.AvailableUSD == nil || *row.AvailableUSD != "-0.600000" {
		t.Fatalf("available_usd = %v, want -0.600000", row.AvailableUSD)
	}
}

// TestBudgetsOverflowFailsClosed proves an unrepresentable spent total fails
// the whole read with the generic 503 rather than wrapping or clamping.
func TestBudgetsOverflowFailsClosed(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	src := &fakeBudgetSource{
		limits: []budget.Limit{{Scope: budget.ScopeClient, Key: "ide", Period: budget.PeriodDay, Micros: math.MaxInt64}},
		snaps: map[string]budget.Snapshot{
			"client|ide|day": {Reported: math.MaxInt64, Estimated: 1},
		},
	}
	srv := budgetTestServer(t, now, src, false)
	rec := budgetGet(t, srv.Handler(), "/control/v1/budgets?scope=client&key=ide&period=day", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "9223372036854775807") || !strings.Contains(body, budgetUnavailableMsg) {
		t.Fatalf("overflow body = %s", body)
	}
}

// TestBudgetsSourceErrorIsRedacted proves a source failure is a generic 503 that
// never echoes the raw error (paths, DSNs, credentials) and classifies a
// deadline/cancel distinctly.
func TestBudgetsSourceErrorIsRedacted(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"store error", errors.New("open /var/secret/budgets.db: permission denied"), budgetUnavailableMsg},
		{"deadline", context.DeadlineExceeded, budgetTimeoutMsg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &fakeBudgetSource{err: tc.err}
			srv := budgetTestServer(t, now, src, false)
			rec := budgetGet(t, srv.Handler(), "/control/v1/budgets?scope=client&key=ide", "")
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("code = %d, want 503: %s", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			if strings.Contains(body, "/var/secret") || strings.Contains(body, "permission denied") {
				t.Fatalf("raw source error leaked: %s", body)
			}
			var env struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(body), &env); err != nil {
				t.Fatalf("decode %q: %v", body, err)
			}
			if env.Error.Message != tc.want {
				t.Fatalf("message = %q, want %q", env.Error.Message, tc.want)
			}
		})
	}
}

// TestBudgetsReadIsBounded proves both period reads share one bounded context.
func TestBudgetsReadIsBounded(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	src := &fakeBudgetSource{}
	srv := budgetTestServer(t, now, src, false)
	rec := budgetGet(t, srv.Handler(), "/control/v1/budgets?scope=client&key=ide", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d: %s", rec.Code, rec.Body.String())
	}
	if src.calls != 2 {
		t.Fatalf("reads = %d, want 2 (day+month)", src.calls)
	}
	if !src.hasDeadline {
		t.Fatal("store read had no deadline")
	}
	if src.deadline <= 0 || src.deadline > budgetReadTimeout {
		t.Fatalf("deadline = %s, want within %s", src.deadline, budgetReadTimeout)
	}
}

func TestFormatMicrosUSD(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0.000000"},
		{1, "0.000001"},
		{1_000_000, "1.000000"},
		{2_500_000, "2.500000"},
		{-500_000, "-0.500000"},
		{math.MaxInt64, "9223372036854.775807"},
		{math.MinInt64, "-9223372036854.775808"},
	}
	for _, tc := range cases {
		if got := formatMicrosUSD(tc.in); got != tc.want {
			t.Errorf("formatMicrosUSD(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// syncLogBuffer is a mutex-guarded buffer so a log record written from any
// goroutine can never race the test's read under -race.
type syncLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncLogBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncLogBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestBudgetsSourceErrorLogIsRedacted pins the log-privacy contract of a failed
// snapshot read. A source error can embed a filesystem DSN or credentials, and
// that raw chain must never leave the process: the synthetic secret is absent
// from both the response body and the captured default-logger record. What the
// log retains is the generic event with its scope and period, and an err
// attribute carrying only the stable sanitized local code (never the raw error
// chain). No real key or DSN is needed to prove this.
func TestBudgetsSourceErrorLogIsRedacted(t *testing.T) {
	const secret = "postgres://localrouter:hunter2@budgets.internal:5432/spend?sslmode=require"
	raw := errors.New("snapshot read failed: dial " + secret)

	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	srv := budgetTestServer(t, now, &fakeBudgetSource{err: raw}, false)

	logs := &syncLogBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	rec := budgetGet(t, srv.Handler(), "/control/v1/budgets?scope=client&key=ide&period=day", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	body, logged := rec.Body.String(), logs.String()

	// Neither the client nor the log may carry the raw secret or its chain.
	for name, got := range map[string]string{"body": body, "log": logged} {
		if strings.Contains(got, secret) || strings.Contains(got, "hunter2") || strings.Contains(got, "dial postgres") {
			t.Fatalf("%s leaked the raw source error: %s", name, got)
		}
	}
	// The body is the generic reason only.
	if !strings.Contains(body, budgetUnavailableMsg) {
		t.Fatalf("body = %s, want %q", body, budgetUnavailableMsg)
	}
	// The log keeps the operator-visible generic event, scope and period, and
	// the err attribute is the sanitized local code rather than the raw chain.
	for _, want := range []string{
		"control: budget snapshot failed",
		"scope=client",
		"period=day",
		`err="` + budgetUnavailableMsg + `"`,
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log missing %q: %s", want, logged)
		}
	}
}
