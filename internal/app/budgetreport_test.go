package app

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/config"
)

// TestBudgetReportAdapterCopiesLimitsAndHold pins the adapter's exact
// control.BudgetSource contract: it copies the generation's ceilings and fixed
// hold out of the config block (never the store's informational limit column),
// returns a copy of the limits on every call, and reads the very store the
// generation's Gate writes.
func TestBudgetReportAdapterCopiesLimitsAndHold(t *testing.T) {
	a, cfgPath := bcBuild(t, "1.00", "5.00")
	if a.budgetStore == nil {
		t.Fatal("Build did not wire the budget store for an enabled budgets block")
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	src := a.budgetSource(c.Budgets)
	if src == nil {
		t.Fatal("budgetSource returned nil for an enabled budgets block")
	}
	if got := src.ReservationMicros(); got != 1_000_000 {
		t.Fatalf("ReservationMicros = %d, want 1000000", got)
	}
	limits := src.Limits()
	if len(limits) != 1 || limits[0].Scope != budget.ScopeClient || limits[0].Key != "ide" ||
		limits[0].Period != budget.PeriodDay || limits[0].Micros != 5_000_000 {
		t.Fatalf("Limits = %+v", limits)
	}
	// Limits is a copy: a caller cannot mutate the captured set.
	limits[0].Micros = 1
	if again := src.Limits(); len(again) != 1 || again[0].Micros != 5_000_000 {
		t.Fatalf("Limits is not a copy: %+v", again)
	}

	// Snapshot reads the actual stored state: after a real reserve on the shared
	// store, the report shows the hold rather than echoing config.
	at := time.Now().UTC()
	if err := a.budgetStore.Reserve(context.Background(), budget.Reservation{
		ID: "br-1", Client: "ide", Account: "acct", At: at, Micros: 1_000_000,
	}, nil); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	snap, err := src.Snapshot(context.Background(), budget.ScopeClient, "ide", budget.PeriodDay, at)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Reserved != 1_000_000 {
		t.Fatalf("snapshot reserved = %d, want 1000000", snap.Reserved)
	}

	// A nil adapter fails closed rather than panicking.
	var nilReport *budgetReport
	if _, err := nilReport.Snapshot(context.Background(), budget.ScopeClient, "ide", budget.PeriodDay, at); err == nil {
		t.Fatal("nil adapter Snapshot returned no error")
	}
}

// TestBudgetReportDisabledSourceIsNil pins the disabled default: without a
// budgets block there is no store and no source, which the endpoint reports as
// enabled:false (the legacy behavior is unchanged).
func TestBudgetReportDisabledSourceIsNil(t *testing.T) {
	a, cfgPath := bcBuild(t, "", "")
	if a.budgetStore != nil {
		t.Fatal("budget store wired without a budgets block")
	}
	c, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if src := a.budgetSource(c.Budgets); src != nil {
		t.Fatalf("budgetSource = %#v, want nil when disabled", src)
	}
}

// TestBudgetReportReloadedLimitMatchesNewGeneration proves a reload publishes a
// fresh adapter carrying the new generation's ceilings, copied from the reloaded
// config rather than the store's stale echo.
func TestBudgetReportReloadedLimitMatchesNewGeneration(t *testing.T) {
	a, cfgPath := bcBuild(t, "1.00", "5.00")
	before, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if src := a.budgetSource(before.Budgets); len(src.Limits()) != 1 || src.Limits()[0].Micros != 5_000_000 {
		t.Fatalf("generation 1 limits = %+v", src.Limits())
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	raised := strings.Replace(string(raw), `daily_usd: "5.00"`, `daily_usd: "9.00"`, 1)
	if raised == string(raw) {
		t.Fatalf("daily_usd not found in config:\n%s", raw)
	}
	if err := os.WriteFile(cfgPath, []byte(raised), 0o600); err != nil {
		t.Fatalf("rewrite config: %v", err)
	}
	st, err := a.Reload(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v (status %+v)", err, st)
	}
	if st.Generation != 2 {
		t.Fatalf("generation = %d, want 2", st.Generation)
	}
	after, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load reloaded config: %v", err)
	}
	src := a.budgetSource(after.Budgets)
	if src == nil {
		t.Fatal("budgetSource nil after reload")
	}
	if got := src.Limits(); len(got) != 1 || got[0].Micros != 9_000_000 {
		t.Fatalf("generation 2 limits = %+v, want day 9000000", got)
	}
	if got := src.ReservationMicros(); got != 1_000_000 {
		t.Fatalf("generation 2 hold = %d, want 1000000", got)
	}
}

// TestBudgetReportEndpointWiring is the end-to-end slice over the real HTTP
// surface: the wired endpoint reports the configured ceiling and fixed reserve
// for a configured identity, and 404s an unknown one before any store read.
func TestBudgetReportEndpointWiring(t *testing.T) {
	a, _ := bcBuild(t, "1.00", "5.00")
	base, stop := serveBudgetReport(t, a)
	defer stop()

	doc := getBudgetReportDoc(t, base+"/control/v1/budgets?scope=client&key=ide")
	if doc.SchemaVersion != 1 || !doc.Enabled {
		t.Fatalf("doc = %+v", doc)
	}
	if doc.ReserveMicros != 1_000_000 {
		t.Fatalf("reserve_micros = %d, want 1000000", doc.ReserveMicros)
	}
	if len(doc.Rows) != 2 || doc.Rows[0].Period != "day" || doc.Rows[1].Period != "month" {
		t.Fatalf("rows = %+v", doc.Rows)
	}
	if doc.Rows[0].LimitMicros == nil || *doc.Rows[0].LimitMicros != 5_000_000 {
		t.Fatalf("day limit = %v, want 5000000", doc.Rows[0].LimitMicros)
	}
	if doc.Rows[1].LimitMicros != nil {
		t.Fatalf("month limit = %v, want omitted (unlimited)", doc.Rows[1].LimitMicros)
	}
	if code, _ := budgetReportStatus(t, base+"/control/v1/budgets?scope=client&key=nope"); code != http.StatusNotFound {
		t.Fatalf("unknown key code = %d, want 404", code)
	}
}

// TestBudgetReportEndpointDisabledIsFalse pins the disabled document over the
// real surface.
func TestBudgetReportEndpointDisabledIsFalse(t *testing.T) {
	a, _ := bcBuild(t, "", "")
	base, stop := serveBudgetReport(t, a)
	defer stop()
	code, body := budgetReportStatus(t, base+"/control/v1/budgets")
	if code != http.StatusOK || strings.TrimSpace(body) != `{"schema_version":1,"enabled":false}` {
		t.Fatalf("disabled endpoint = %d %s", code, body)
	}
}

// budgetReportDoc mirrors just the fields these tests assert.
type budgetReportDoc struct {
	SchemaVersion int   `json:"schema_version"`
	Enabled       bool  `json:"enabled"`
	ReserveMicros int64 `json:"reserve_micros"`
	Rows          []struct {
		Scope           string `json:"scope"`
		Key             string `json:"key"`
		Period          string `json:"period"`
		LimitMicros     *int64 `json:"limit_micros"`
		AvailableMicros *int64 `json:"available_micros"`
	} `json:"rows"`
}

func serveBudgetReport(t *testing.T, a *App) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = a.Serve(ctx, ln)
	}()
	base := "http://" + ln.Addr().String()
	deadline := time.Now().Add(3 * time.Second)
	for {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("control server never became ready at %s", base)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return base, func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	}
}

func budgetReportStatus(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("get %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return resp.StatusCode, string(b)
}

func getBudgetReportDoc(t *testing.T, url string) budgetReportDoc {
	t.Helper()
	code, body := budgetReportStatus(t, url)
	if code != http.StatusOK {
		t.Fatalf("get %s = %d %s", url, code, body)
	}
	var doc budgetReportDoc
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return doc
}
