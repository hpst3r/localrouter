package app_test

// Integrated-stack acceptance over the real App: a config with a
// capability-constrained route (per-candidate backend aliases) and a budgets
// block is loaded, built and served, and one request fails over across two
// differently priced backends. The real budget store (budgets.db) and the
// real ledger (localrouter.db) must both attribute cost to the backend each
// attempt actually resolved — never to the client alias — and the operator
// budget report must show the per-account and per-client charges.
//
// Every upstream is an httptest.Server on loopback; key material is fake
// fixture data and the upstream key comes from the environment.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
)

func stackConfigYAML(failURL, okURL, keyFile string) string {
	return fmt.Sprintf(`listen: 127.0.0.1:0
data_dir: data
pricing_file: pricing.yaml
control: {require_auth: true}
clients:
  - {name: ide, class: interactive, key_file: %s}
accounts:
  - {id: a, provider: openai_compat, base_url: %s/v1, api_key_env: HR_UPSTREAM_KEY}
  - {id: b, provider: openai_compat, base_url: %s/v1, api_key_env: HR_UPSTREAM_KEY}
routes:
  - name: r
    models: [client-model]
    interactive: [a, b]
    background: [a, b]
    upstreams:
      a: {upstream_model: backend-a, protocols: [responses], input_modalities: [text]}
      b: {upstream_model: backend-b, protocols: [responses], input_modalities: [text]}
budgets:
  reserve_usd: "0.25"
  clients:
    ide: {daily_usd: "5.00"}
  accounts:
    a: {daily_usd: "5.00"}
    b: {daily_usd: "5.00"}
`, keyFile, failURL, okURL)
}

// stackReportRow is the subset of a /control/v1/budgets row this test reads.
type stackReportRow struct {
	Period          string `json:"period"`
	EstimatedMicros int64  `json:"estimated_micros"`
	UnknownMicros   int64  `json:"unknown_micros"`
	ReservedMicros  int64  `json:"reserved_micros"`
}

func stackReportDay(t *testing.T, e *hrEnv, key, scope, name string) stackReportRow {
	t.Helper()
	r := e.do("GET", "/control/v1/budgets?scope="+scope+"&key="+name, key, "")
	if r.status != http.StatusOK {
		t.Fatalf("budget report %s/%s = %d %s", scope, name, r.status, r.body)
	}
	var doc struct {
		Rows []stackReportRow `json:"rows"`
	}
	if err := json.Unmarshal(r.body, &doc); err != nil {
		t.Fatalf("decode budget report: %v (%s)", err, r.body)
	}
	for _, row := range doc.Rows {
		if row.Period == "day" {
			return row
		}
	}
	t.Fatalf("budget report %s/%s has no day row: %s", scope, name, r.body)
	return stackReportRow{}
}

func TestStackAliasFailoverChargesEachBackendThroughConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	key := "stack-client-key-0123456789"
	hrWriteFile(t, filepath.Join(dir, "a.key"), key)
	// Per 1M input tokens: the client alias is the most expensive, so a cost
	// booked at the alias instead of the backend is visible. hrNewUpstream
	// answers 1000 input tokens: backend-b = $0.005 = 5000 micros.
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"),
		"models:\n  client-model: {input: 50, output: 0}\n  backend-a: {input: 2, output: 0}\n  backend-b: {input: 5, output: 0}\n")

	var mu sync.Mutex
	var failSent []string
	fail := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var doc struct{ Model string }
		_ = json.Unmarshal(b, &doc)
		mu.Lock()
		failSent = append(failSent, doc.Model)
		mu.Unlock()
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(fail.Close)
	ok := hrNewUpstream(t)

	cfgPath := filepath.Join(dir, "config.yaml")
	hrWriteFile(t, cfgPath, stackConfigYAML(fail.URL, ok.srv.URL, "a.key"))
	e := hrBuild(t, dir, cfgPath, ok)

	if r := e.responses(key, "client-model"); r.status != http.StatusOK {
		t.Fatalf("status = %d %s, want 200 from the failover backend", r.status, r.body)
	}
	mu.Lock()
	sentA := append([]string(nil), failSent...)
	mu.Unlock()
	hits := ok.hitsCopy()
	if len(sentA) != 1 || sentA[0] != "backend-a" || len(hits) != 1 || capUpstreamModel(t, hits[0].Body) != "backend-b" {
		t.Fatalf("upstream models: a=%v b=%d hits; want backend-a then backend-b", sentA, len(hits))
	}

	const hold, costB = int64(250_000), int64(5_000)
	checks := []struct {
		scope, name          string
		estimated, unknownMu int64
	}{
		{"client", "ide", costB, hold},
		{"account", "a", 0, hold},
		{"account", "b", costB, 0},
	}
	for _, c := range checks {
		got := stackReportDay(t, e, key, c.scope, c.name)
		if got.EstimatedMicros != c.estimated || got.UnknownMicros != c.unknownMu || got.ReservedMicros != 0 {
			t.Fatalf("%s %s day = %+v, want estimated %d unknown %d reserved 0", c.scope, c.name, got, c.estimated, c.unknownMu)
		}
	}

	sum := e.summary()
	if c := sum["b"].CostUSD; c == nil || *c != 0.005 {
		t.Fatalf("ledger cost for b = %v, want 0.005 (backend-b price)", c)
	}
	if c := sum["a"].CostUSD; c != nil {
		t.Fatalf("ledger cost for failed attempt = %v, want NULL", *c)
	}
}
