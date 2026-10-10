package app_test

// App-level spend-control lifecycle tests: the slice that wires the optional
// budgets block into Build/reload and the *budget.Store's process ownership.
//
// Scope: this file only. It drives the real App HTTP surface (App.Handler +
// httptest) with the shared hot-reload fake upstream, so every assertion is
// observable from outside the process: a second Build on the same DataDir is
// denied while an owner is live, a Build that fails after acquiring ownership
// releases it, enabling or disabling the budgets block is restart-only while
// editing its reserve/ceilings reloads live and takes effect on the next
// request, and an orphan reservation left open by a previous process is charged
// its ceiling at startup.
//
// The upstream API key is a synthetic placeholder supplied through the
// environment; every key in this file is fake fixture data.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/budget"
	"github.com/hpst3r/localrouter/internal/config"

	"os"
	"path/filepath"
)

// budgetConfigYAML renders the shared minimal config with an optional budgets
// block. reserve and daily empty disables spend controls (no budgets block).
func budgetConfigYAML(upstreamURL, keyFile, reserve, daily string) string {
	var b strings.Builder
	fmt.Fprintf(&b, `listen: 127.0.0.1:0
data_dir: data
pricing_file: pricing.yaml
clients:
  - {name: ide, class: interactive, key_file: %s}
accounts:
  - id: acct
    provider: openai_compat
    base_url: %s/v1
    api_key_env: HR_UPSTREAM_KEY
routes:
  - name: r
    models: [%s]
    interactive: [acct]
    background: [acct]
`, keyFile, upstreamURL, hrPricedModel)
	if reserve != "" {
		fmt.Fprintf(&b, "budgets:\n  reserve_usd: %q\n  clients:\n    ide: {daily_usd: %q}\n", reserve, daily)
	}
	return b.String()
}

func budgetDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// budgetSetup writes the fixture files (client key, pricing, config) into a
// fresh temp dir and returns them. The fake upstream is created first so the
// generated config can name it.
func budgetSetup(t *testing.T, reserve, daily string) (dir, cfgPath, keyFile string, up *hrUpstream) {
	t.Helper()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	dir = t.TempDir()
	key := "budget-client-keyA-0000000000"
	hrWriteFile(t, filepath.Join(dir, "a.key"), key)
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"), hrPricingYAML(1.0))
	up = hrNewUpstream(t)
	cfgPath = filepath.Join(dir, "config.yaml")
	hrWriteFile(t, cfgPath, budgetConfigYAML(up.srv.URL, "a.key", reserve, daily))
	return dir, cfgPath, key, up
}

func budgetArtifact(dir, name string) string {
	return filepath.Join(dir, "data", name)
}

// TestBuildWithoutBudgetsCreatesNoArtifacts pins the backwards-compatible
// default: a config with no budgets block must leave no budgets.db and no
// budgets.lock behind.
func TestBuildWithoutBudgetsCreatesNoArtifacts(t *testing.T) {
	dir, cfgPath, _, _ := budgetSetup(t, "", "")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := app.Build(cfg, budgetDiscardLogger(), app.Overrides{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	defer a.Close()
	for _, name := range []string{"budgets.db", "budgets.lock"} {
		if _, err := os.Stat(budgetArtifact(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s exists or stat failed unexpectedly: %v", name, err)
		}
	}
}

// TestBuildSecondInstanceDeniedAndReleasedOnClose covers the startup ownership
// contract: while an enabled App is alive a second Build against the same
// DataDir is denied without reconciling, and once the first App is closed the
// ownership is released so a later Build succeeds.
func TestBuildSecondInstanceDeniedAndReleasedOnClose(t *testing.T) {
	dir, cfgPath, _, _ := budgetSetup(t, "1.00", "5.00")

	cfg1, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config 1: %v", err)
	}
	a1, err := app.Build(cfg1, budgetDiscardLogger(), app.Overrides{})
	if err != nil {
		t.Fatalf("first build: %v", err)
	}

	cfg2, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config 2: %v", err)
	}
	if _, err := app.Build(cfg2, budgetDiscardLogger(), app.Overrides{}); !errors.Is(err, budget.ErrOwnershipHeld) {
		t.Fatalf("second build while owned = %v, want budget.ErrOwnershipHeld", err)
	}

	if err := a1.Close(); err != nil {
		t.Fatalf("close first app: %v", err)
	}

	cfg3, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config 3: %v", err)
	}
	a3, err := app.Build(cfg3, budgetDiscardLogger(), app.Overrides{})
	if err != nil {
		t.Fatalf("build after close = %v, want success (ownership must be released)", err)
	}
	if err := a3.Close(); err != nil {
		t.Fatalf("close third app: %v", err)
	}
	_ = dir
}

// TestBuildFailureReleasesOwnership pins the cleanup contract: a Build that
// fails after it acquired ownership (here, a client key file that cannot be
// loaded while the generation is prepared) must release the lock so the next
// Build can proceed.
func TestBuildFailureReleasesOwnership(t *testing.T) {
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	dir := t.TempDir()
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"), hrPricingYAML(1.0))
	up := hrNewUpstream(t)
	cfgPath := filepath.Join(dir, "config.yaml")
	// key_file names a file that does not exist yet: config validation is
	// satisfied (the path is present), but the generation's key load fails.
	hrWriteFile(t, cfgPath, budgetConfigYAML(up.srv.URL, "missing.key", "1.00", "5.00"))

	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if _, err := app.Build(cfg, budgetDiscardLogger(), app.Overrides{}); err == nil {
		t.Fatal("build with a missing client key file succeeded, want failure")
	}

	// Repair the key file; the failed Build must have released ownership.
	hrWriteFile(t, filepath.Join(dir, "missing.key"), "budget-client-keyB-0000000000")
	cfg2, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load repaired config: %v", err)
	}
	a, err := app.Build(cfg2, budgetDiscardLogger(), app.Overrides{})
	if err != nil {
		t.Fatalf("build after a failed build = %v, want success (ownership must be released)", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

// TestReloadEnableBudgetsRequiresRestart and its disable counterpart pin the
// restart bit: the presence of the budgets block is restart-only, in both
// directions.
func TestReloadEnableBudgetsRequiresRestart(t *testing.T) {
	dir, cfgPath, _, up := budgetSetup(t, "", "")
	e := hrBuild(t, dir, cfgPath, up)

	hrWriteFile(t, cfgPath, budgetConfigYAML(up.srv.URL, "a.key", "1.00", "5.00"))
	candidate, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load enabled config: %v", err)
	}
	st, err := e.a.ReloadConfig(candidate)
	if !errors.Is(err, app.ErrRestartRequired) {
		t.Fatalf("enable budgets reload = %v, want ErrRestartRequired", err)
	}
	if st.OK {
		t.Fatal("restart-required reload reported OK")
	}
	if st.Generation != 1 {
		t.Fatalf("generation advanced to %d, want 1", st.Generation)
	}
	if !slices.Contains(st.RestartOnly, "budgets") {
		t.Fatalf("restart-only fields = %v, want to contain %q", st.RestartOnly, "budgets")
	}
}

func TestReloadDisableBudgetsRequiresRestart(t *testing.T) {
	dir, cfgPath, _, up := budgetSetup(t, "1.00", "5.00")
	e := hrBuild(t, dir, cfgPath, up)

	hrWriteFile(t, cfgPath, budgetConfigYAML(up.srv.URL, "a.key", "", ""))
	candidate, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load disabled config: %v", err)
	}
	st, err := e.a.ReloadConfig(candidate)
	if !errors.Is(err, app.ErrRestartRequired) {
		t.Fatalf("disable budgets reload = %v, want ErrRestartRequired", err)
	}
	if st.Generation != 1 {
		t.Fatalf("generation advanced to %d, want 1", st.Generation)
	}
	if !slices.Contains(st.RestartOnly, "budgets") {
		t.Fatalf("restart-only fields = %v, want to contain %q", st.RestartOnly, "budgets")
	}
}

// TestReloadBudgetLimitAndReserveEditAppliesToNextRequest pins that editing the
// reserve and a ceiling reloads live (no restart) and that the published
// generation actually enforces the new, tighter ceiling: the first request is
// admitted, the next is denied with 429.
func TestReloadBudgetLimitAndReserveEditAppliesToNextRequest(t *testing.T) {
	dir, cfgPath, key, up := budgetSetup(t, "1.00", "5.00")
	e := hrBuild(t, dir, cfgPath, up)

	if r := e.responses(key, hrPricedModel); r.status != 200 {
		t.Fatalf("first request under generous budget = %d %s, want 200", r.status, r.body)
	}

	// Tighten both the fixed reserve and the daily ceiling, then reload.
	hrWriteFile(t, cfgPath, budgetConfigYAML(up.srv.URL, "a.key", "2.00", "0.000001"))
	st, err := e.a.Reload(cfgPath)
	if err != nil || !st.OK || st.Generation != 2 {
		t.Fatalf("limit/reserve edit reload = %#v, %v; want OK generation 2", st, err)
	}
	if len(st.RestartOnly) != 0 {
		t.Fatalf("limit/reserve edit reported restart-only fields %v", st.RestartOnly)
	}

	if r := e.responses(key, hrPricedModel); r.status != 429 {
		t.Fatalf("request under the tightened ceiling = %d %s, want 429", r.status, r.body)
	}
}

// TestReconcileOrphansAtStartupChargesStartupCeiling pins the startup recovery
// contract: an orphan reservation left open by a previous process is charged
// its reserved ceiling at Build, which consumes the configured ceiling and
// makes the first request fail closed with 429.
func TestReconcileOrphansAtStartupChargesStartupCeiling(t *testing.T) {
	dir, cfgPath, key, up := budgetSetup(t, "0.50", "0.50")

	// Seed one open reservation as a previous process would have left it, then
	// close that store without settling: Build must reconcile it.
	seed, err := budget.Open(budgetArtifact(dir, "budgets.db"))
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	if err := seed.Reserve(context.Background(), budget.Reservation{
		ID: "orphan-startup", Client: "ide", Account: "acct",
		At: time.Now().UTC(), Micros: 500_000,
	}, nil); err != nil {
		t.Fatalf("seed reserve: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}

	e := hrBuild(t, dir, cfgPath, up)

	// The ceiling must have been charged at startup as unknown spend.
	read, err := budget.Open(budgetArtifact(dir, "budgets.db"))
	if err != nil {
		t.Fatalf("read open: %v", err)
	}
	snap, err := read.Snapshot(context.Background(), budget.ScopeClient, "ide", budget.PeriodDay, time.Now().UTC())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if err := read.Close(); err != nil {
		t.Fatalf("read close: %v", err)
	}
	if snap.Unknown != 500_000 {
		t.Fatalf("orphan startup unknown spend = %d, want 500000", snap.Unknown)
	}

	if r := e.responses(key, hrPricedModel); r.status != 429 {
		t.Fatalf("request after startup reconcile = %d %s, want 429", r.status, r.body)
	}
}
