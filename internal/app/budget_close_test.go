package app

// App-level lifecycle tests for the Close/Reload boundary and the
// budget-disabled-publication hazard (budget-lifecycle review F2/F3).
//
// These live in package app, not app_test, on purpose: the invariants are about
// the unexported terminal latch (serving/stopped), the shared budgetStore field
// and the generation that may or may not be published. They drive the real
// Build path with real fixture files (no HTTP server, no timing, no sleeps):
//
//   - a Close that was not preceded by a drained Serve is still terminal, so a
//     later Reload is rejected with ErrNotServing and no generation is published
//     with a nil budget gate (the fail-open the review found);
//   - a Serve after Close is rejected rather than starting a half-ready server
//     over a released store;
//   - a Reload racing a Close is race-free on budgetStore and still terminal;
//   - a config with no budgets block keeps its legacy nil/nil default, while a
//     config that enables budgets with no open store is a hard error, never a
//     nil (fail-open) gate.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hpst3r/localrouter/internal/config"
)

const bcUpstreamKey = "bc-upstream-key-000000000000"

func bcLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func bcWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// bcFixture writes a minimal but real config (client key, pricing file, config)
// and returns its path. An empty reserve omits the budgets block, the legacy
// disabled default.
func bcFixture(t *testing.T, reserve, daily string) string {
	t.Helper()
	t.Setenv("BC_UPSTREAM_KEY", bcUpstreamKey)
	dir := t.TempDir()
	bcWrite(t, filepath.Join(dir, "a.key"), "bc-client-key-0000000000")
	bcWrite(t, filepath.Join(dir, "pricing.yaml"), "models:\n  bc-priced:\n    input: 1\n    output: 0\n")
	body := "listen: 127.0.0.1:0\n" +
		"data_dir: " + filepath.Join(dir, "data") + "\n" +
		"pricing_file: " + filepath.Join(dir, "pricing.yaml") + "\n" +
		"clients:\n  - {name: ide, class: interactive, key_file: " + filepath.Join(dir, "a.key") + "}\n" +
		"accounts:\n  - id: acct\n    provider: openai_compat\n    base_url: http://127.0.0.1:1/v1\n    api_key_env: BC_UPSTREAM_KEY\n" +
		"routes:\n  - name: r\n    models: [bc-priced]\n    interactive: [acct]\n    background: [acct]\n"
	if reserve != "" {
		body += "budgets:\n  reserve_usd: \"" + reserve + "\"\n  clients:\n    ide: {daily_usd: \"" + daily + "\"}\n"
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	bcWrite(t, cfgPath, body)
	return cfgPath
}

func bcBuild(t *testing.T, reserve, daily string) (*App, string) {
	t.Helper()
	cfgPath := bcFixture(t, reserve, daily)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := Build(cfg, bcLogger(), Overrides{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a, cfgPath
}

// TestCloseWithoutServeIsTerminalForReload pins the review F2 fix: Close itself
// establishes the terminal stop, so a Reload that races a Close which had no
// preceding Serve (the net.Listen-failure path in cmd/localrouter, with the
// SIGHUP loop already live) cannot publish a generation built from a released
// store.
func TestCloseWithoutServeIsTerminalForReload(t *testing.T) {
	a, cfgPath := bcBuild(t, "1.00", "5.00")
	if a.budgetStore == nil || a.budgetOwnership == nil {
		t.Fatal("Build did not wire the budget store for an enabled budgets block")
	}
	before := a.ReloadStatus().Generation
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if a.Serving() {
		t.Fatal("Close left the app serving")
	}
	if a.budgetStore != nil || a.budgetOwnership != nil {
		t.Fatal("Close did not release the budget store/ownership")
	}
	cand, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload candidate: %v", err)
	}
	st, err := a.ReloadConfig(cand)
	if !errors.Is(err, ErrNotServing) {
		t.Fatalf("reload after Close = (%+v, %v); want ErrNotServing", st, err)
	}
	g := a.current.Load()
	if g == nil {
		t.Fatal("Close left no current generation")
	}
	if g.status.Generation != before {
		t.Fatalf("generation advanced after Close: before=%d after=%d", before, g.status.Generation)
	}
	if a.ReloadStatus().Generation != before {
		t.Fatalf("reload status generation advanced after Close: want %d", before)
	}
}

// TestCloseThenServeIsRejected pins the second half of the terminal latch: a
// Serve after Close is rejected instead of starting a server whose handlers
// would run over a released budget store.
func TestCloseThenServeIsRejected(t *testing.T) {
	a, _ := bcBuild(t, "1.00", "5.00")
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Pre-cancelled: if a Serve were (wrongly) accepted it would return
	// immediately instead of hanging the test.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Serve(ctx, ln); !errors.Is(err, ErrNotServing) {
		t.Fatalf("Serve after Close = %v, want ErrNotServing", err)
	}
}

// TestConcurrentCloseAndReloadIsRaceFreeAndTerminal runs Reload and Close
// concurrently. Run under -race: before the fix Close wrote a.budgetStore in
// closeBudget while makeGeneration read it in budgetGate, an unordered
// read/write pair. After the fix both serialize on reloadMu, so the run is
// race-free and Close is terminal for any later Reload.
func TestConcurrentCloseAndReloadIsRaceFreeAndTerminal(t *testing.T) {
	for i := 0; i < 20; i++ {
		a, cfgPath := bcBuild(t, "1.00", "5.00")
		cand, err := config.Load(cfgPath)
		if err != nil {
			t.Fatalf("iteration %d: candidate: %v", i, err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = a.ReloadConfig(cand) }()
		go func() { defer wg.Done(); _ = a.Close() }()
		wg.Wait()
		if a.Serving() {
			t.Fatalf("iteration %d: app still serving after Close", i)
		}
		gen := a.current.Load().status.Generation
		if _, err := a.ReloadConfig(cand); !errors.Is(err, ErrNotServing) {
			t.Fatalf("iteration %d: reload after Close = %v, want ErrNotServing", i, err)
		}
		if g := a.current.Load(); g.status.Generation != gen {
			t.Fatalf("iteration %d: generation advanced after Close: %d -> %d", i, gen, g.status.Generation)
		}
	}
}

// TestConcurrentCloseIsIdempotent serializes concurrent Close calls via
// reloadMu; every caller gets the ledger's existing idempotent result and the
// budget fields are released exactly once.
func TestConcurrentCloseIsIdempotent(t *testing.T) {
	a, _ := bcBuild(t, "1.00", "5.00")
	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); errs[i] = a.Close() }(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Close #%d: %v", i, err)
		}
	}
	if a.budgetStore != nil || a.budgetOwnership != nil {
		t.Fatal("concurrent Close left the budget fields set")
	}
	if a.Serving() {
		t.Fatal("concurrent Close left the app serving")
	}
}

// TestCloseNilBudgetLegacyAndGateStoreMissing pins both budgetGate defaults: a
// config with no budgets block and no store keeps its legacy (nil gate, nil
// error) default, while a config that enables budgets with no open store is a
// hard error -- never a nil gate, which the proxy treats as "no enforcement".
func TestCloseNilBudgetLegacyAndGateStoreMissing(t *testing.T) {
	a, _ := bcBuild(t, "", "")
	if a.budgetStore != nil || a.budgetOwnership != nil {
		t.Fatal("an app without a budgets block must leave the budget fields nil")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if a.Serving() {
		t.Fatal("Close left the legacy app serving")
	}
	for i := 0; i < 3; i++ {
		if err := a.Close(); err != nil {
			t.Fatalf("close #%d: %v", i+2, err)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Serve(ctx, ln); !errors.Is(err, ErrNotServing) {
		t.Fatalf("Serve after a legacy Close = %v, want ErrNotServing", err)
	}
	// Legacy disabled default is unchanged: no budgets and no store -> nil gate.
	if g, reason, err := a.budgetGate(&config.Config{}, nil); g != nil || reason != "" || err != nil {
		t.Fatalf("legacy budgetGate = (%v, %q, %v); want (nil, \"\", nil)", g, reason, err)
	}
	// Fail-closed: budgets enabled but no open store must error, not nil-gate.
	if g, _, err := a.budgetGate(&config.Config{Budgets: &config.BudgetConfig{}}, nil); err == nil || g != nil {
		t.Fatalf("budgetGate with budgets enabled and no store = (%v, %v); want (nil, non-nil error)", g, err)
	}
}
