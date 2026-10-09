package app_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/auth"
	"github.com/hpst3r/localrouter/internal/config"
)

// A synthetic, non-functional placeholder key: no real credential is used or
// needed by this test.
const orCostKey = "fixture-openrouter-key-not-a-real-credential"

const orCostModel = "anthropic/claude-sonnet-4.5"

// orCostSSE is a streamed OpenRouter completion whose final usage chunk carries
// the provider-reported cost.
const orCostSSE = `data: {"id":"gen-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n" +
	": OPENROUTER PROCESSING\n\n" +
	`data: {"id":"gen-1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":42,"completion_tokens":8,"total_tokens":50,"cost":0.0003,"prompt_tokens_details":{"cached_tokens":6},"completion_tokens_details":{"reasoning_tokens":2}}}` + "\n\n" +
	"data: [DONE]\n\n"

// orCostJSON is a non-streamed OpenRouter completion with a top-level usage
// object that reports the cost.
const orCostJSON = `{"id":"gen-2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":21,"completion_tokens":128,"total_tokens":149,"cost":0.000123}}`

// orCostEnv is a real app wired to a fake OpenRouter that replies SSE for
// streamed requests and plain JSON otherwise, with no pricing table loaded.
type orCostEnv struct {
	t      *testing.T
	appURL string
	client string
	logs   *lockedBuffer

	mu     sync.Mutex
	bodies [][]byte
}

func (e *orCostEnv) upstreamBodies() [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]byte, len(e.bodies))
	copy(out, e.bodies)
	return out
}

func newORCostEnv(t *testing.T) *orCostEnv {
	t.Helper()
	dir := t.TempDir()
	e := &orCostEnv{t: t, logs: &lockedBuffer{}}
	var err error
	if e.client, err = auth.GenerateKey(); err != nil {
		t.Fatal(err)
	}
	writeSecret(t, filepath.Join(dir, "client.key"), e.client)
	writeSecret(t, filepath.Join(dir, "or.key"), orCostKey)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/credits", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"total_credits":20,"total_usage":5}}`)
	})
	mux.HandleFunc("GET /api/v1/key", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"data":{"label":"sk-or-v1-abc...","limit":null,"limit_remaining":null,"limit_reset":null,
			"include_byok_in_limit":false,"usage":0,"usage_daily":0,"usage_weekly":0,"usage_monthly":0,
			"byok_usage":0,"byok_usage_daily":0,"byok_usage_weekly":0,"byok_usage_monthly":0,"is_free_tier":false}}`)
	})
	mux.HandleFunc("POST /api/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		e.mu.Lock()
		e.bodies = append(e.bodies, b)
		e.mu.Unlock()
		var req struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(b, &req)
		if !req.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, orCostJSON)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, orCostSSE)
	})
	up := httptest.NewServer(mux)
	t.Cleanup(up.Close)

	// No pricing_file: the ledger has an empty price table, so any cost in the
	// ledger can only have come from the provider's usage.cost.
	yaml := fmt.Sprintf(`data_dir: data
clients:
  - {name: ide, class: interactive, key_file: client.key}
accounts:
  - id: or
    provider: openrouter
    base_url: %s/api/v1
    api_key_file: or.key
routes:
  - name: openrouter
    models: ["%s"]
    interactive: [or]
`, up.URL, orCostModel)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.PricingFile); !os.IsNotExist(err) {
		t.Fatalf("test must run without a pricing table, %q: %v", cfg.PricingFile, err)
	}
	a, err := app.Build(cfg, slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		app.Overrides{HTTPClient: up.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Start(ctx)
	srv := httptest.NewServer(a.Handler)
	t.Cleanup(srv.Close)
	e.appURL = srv.URL
	return e
}

// waitAdmissible blocks until the openrouter account is routing-ready.
func (e *orCostEnv) waitAdmissible() {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(e.appURL + "/control/v1/status")
		if err == nil {
			var doc struct {
				Accounts []orAccountStatus `json:"accounts"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&doc)
			resp.Body.Close()
			for _, a := range doc.Accounts {
				if a.ID == "or" && a.Credits != nil && a.Credits.Available {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			e.t.Fatal("openrouter account never became admissible")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *orCostEnv) chat(stream bool) (int, string) {
	e.t.Helper()
	body := fmt.Sprintf(`{"model":%q,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, orCostModel, stream)
	req, _ := http.NewRequest(http.MethodPost, e.appURL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+e.client)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *orCostEnv) accountRow() (map[string]any, bool) {
	e.t.Helper()
	resp, err := http.Get(e.appURL + "/control/v1/usage?group=account")
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		e.t.Fatal(err)
	}
	for _, r := range doc.Rows {
		if r["key"] == "or" {
			return r, true
		}
	}
	return nil, false
}

func (e *orCostEnv) waitAccountRow(cond func(map[string]any) bool) map[string]any {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if row, ok := e.accountRow(); ok && cond(row) {
			return row
		}
		if time.Now().After(deadline) {
			row, _ := e.accountRow()
			e.t.Fatalf("timed out waiting for ledger row; last %v", row)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func num(v any) float64 { f, _ := v.(float64); return f }

// TestE2EOpenRouterReportedCost drives the real app end to end: a non-streamed
// and a streamed OpenRouter completion each carry usage.cost, and with no
// pricing table loaded the ledger must record exactly that provider-reported
// cost (basis provider_reported) and count zero unpriced requests, while the
// upstream request body and the downstream response bytes are forwarded intact.
func TestE2EOpenRouterReportedCost(t *testing.T) {
	e := newORCostEnv(t)
	e.waitAdmissible()

	// Non-streamed request.
	code, body := e.chat(false)
	if code != 200 || body != orCostJSON {
		t.Fatalf("non-stream response %d %q", code, body)
	}
	bodies := e.upstreamBodies()
	if len(bodies) != 1 || !strings.Contains(string(bodies[0]), `"model":"`+orCostModel+`"`) {
		t.Fatalf("upstream bodies %q", bodies)
	}
	if strings.Contains(string(bodies[0]), "include_usage") {
		t.Errorf("non-stream request must not add stream_options: %s", bodies[0])
	}
	row := e.waitAccountRow(func(r map[string]any) bool { return num(r["requests"]) == 1 })
	if row["cost_usd"] == nil || math.Abs(num(row["cost_usd"])-0.000123) > 1e-12 {
		t.Fatalf("non-stream cost_usd = %v", row["cost_usd"])
	}
	if num(row["unpriced_requests"]) != 0 || num(row["unknown_usage_requests"]) != 0 {
		t.Errorf("non-stream counts %v", row)
	}
	if num(row["input_tokens"]) != 21 || num(row["output_tokens"]) != 128 {
		t.Errorf("non-stream tokens %v", row)
	}

	// Streamed request.
	code, body = e.chat(true)
	if code != 200 || body != orCostSSE {
		t.Fatalf("stream response %d %q", code, body)
	}
	bodies = e.upstreamBodies()
	if len(bodies) != 2 || !strings.Contains(string(bodies[1]), `"include_usage":true`) {
		t.Fatalf("streamed upstream body %q", bodies)
	}
	row = e.waitAccountRow(func(r map[string]any) bool {
		return num(r["requests"]) == 2 && num(r["input_tokens"]) == 63
	})
	if row["cost_usd"] == nil || math.Abs(num(row["cost_usd"])-0.000423) > 1e-12 {
		t.Fatalf("combined cost_usd = %v (want 0.000423)", row["cost_usd"])
	}
	if num(row["unpriced_requests"]) != 0 || num(row["unknown_usage_requests"]) != 0 {
		t.Errorf("stream counts %v", row)
	}
	if num(row["cached_input_tokens"]) != 6 || num(row["reasoning_tokens"]) != 2 {
		t.Errorf("stream tokens %v", row)
	}
	if strings.Contains(e.logs.String(), orCostKey) {
		t.Errorf("upstream key leaked into logs")
	}
}
