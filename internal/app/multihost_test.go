package app_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/core"
)

// mhEnv is a network-mode server (allowed_hosts, require_auth) with an
// interactive client on host "laptop", an ingest client for host "vm1", an
// agent-sourced claude account and a fake ollama upstream.
type mhEnv struct {
	app               *app.App
	srv               *httptest.Server
	laptopKey, vm1Key string
}

func newMHEnv(t *testing.T) *mhEnv {
	t.Helper()
	dir := t.TempDir()
	up := http.NewServeMux()
	up.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		writeJSONResp(w, map[string]any{"id": "c1", "object": "chat.completion",
			"usage": map[string]any{"prompt_tokens": 20, "completion_tokens": 5}})
	})
	up.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", http.StatusNotFound) })
	upstream := httptest.NewServer(up)
	t.Cleanup(upstream.Close)

	e := &mhEnv{laptopKey: "lr-laptop-SECRET-0123456789abcdef", vm1Key: "lr-vm1-SECRET-0123456789abcdef"}
	writeSecret(t, filepath.Join(dir, "laptop.key"), e.laptopKey)
	writeSecret(t, filepath.Join(dir, "vm1.key"), e.vm1Key)
	writeSecret(t, filepath.Join(dir, "ollama.key"), "ollama-SECRETAPIKEY-0123456789")
	yaml := `listen: 100.64.0.10:8787
allow_non_loopback: true
allowed_hosts: [Router.Tail, 100.64.0.10]
control: {require_auth: true}
data_dir: data
clients:
  - {name: laptop, class: interactive, key_file: laptop.key, host: laptop}
  - {name: vm1, class: background, key_file: vm1.key, host: vm1, ingest: true}
accounts:
  - {id: claude-max, provider: claude, quota_source: agent, credentials_file: missing.json}
  - {id: ol, provider: ollama, base_url: ` + upstream.URL + `/v1, api_key_file: ollama.key}
routes:
  - {name: ollama, models: [m], interactive: [ol], background: [ol]}
`
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	e.app, err = app.Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Overrides{
		OllamaUsageURL: upstream.URL + "/usage",
		ClaudeUsageURL: upstream.URL + "/usage",
	})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = httptest.NewServer(e.app.Handler)
	t.Cleanup(func() {
		e.srv.Close()
		_ = e.app.Close()
	})
	return e
}

func (e *mhEnv) do(t *testing.T, method, path, host, key, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(b), "SECRET") {
		t.Fatalf("%s %s leaks a secret: %s", method, path, b)
	}
	return resp.StatusCode, string(b)
}

// MH2: allowed_hosts entry accepted, unknown host 403, loopback ok.
func TestMHHostGuard(t *testing.T) {
	e := newMHEnv(t)
	for host, want := range map[string]int{
		"router.tail:8787": 200, "ROUTER.TAIL": 200, "100.64.0.10:8787": 200,
		"127.0.0.1:8787": 200, "localhost": 200, "[::1]:8787": 200,
		"evil.example": 403, "100.64.0.11:8787": 403, "router.tail.evil": 403,
	} {
		if code, _ := e.do(t, "GET", "/healthz", host, "", ""); code != want {
			t.Errorf("host %q: %d want %d", host, code, want)
		}
	}
	// Data endpoints still need a key in network mode.
	if code, _ := e.do(t, "GET", "/control/v1/status", "router.tail", "", ""); code != 401 {
		t.Fatalf("status without key: %d", code)
	}
}

// MH3 / MH4 / MH5 / MH8 through the real object graph.
func TestMHIngestAndHostUsage(t *testing.T) {
	e := newMHEnv(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rec := `{"id":"claude:msg_1","started_at":"` + now + `","model":"claude-opus","account_id":"claude-max",` +
		`"usage":{"input_tokens":100,"output_tokens":10},"usage_known":true,"client":"x","provider":"codex","host":"spoof"}`
	body := `{"schema_version":1,"host":"vm1","records":[` + rec + `]}`

	if code, _ := e.do(t, "POST", "/control/v1/ingest", "", "", body); code != 401 {
		t.Fatalf("no key: %d", code)
	}
	if code, _ := e.do(t, "POST", "/control/v1/ingest", "", e.laptopKey, body); code != 403 {
		t.Fatalf("non-ingest key: %d", code)
	}
	bad := strings.Replace(body, `"account_id":"claude-max"`, `"account_id":"ol"`, 1)
	if code, _ := e.do(t, "POST", "/control/v1/ingest", "", e.vm1Key, bad); code != 400 {
		t.Fatalf("unknown account: %d", code)
	}
	if rows := e.summary(t, "host"); len(rows) != 0 {
		t.Fatalf("rows after rejected ingest: %+v", rows)
	}
	for i := range 2 {
		code, out := e.do(t, "POST", "/control/v1/ingest", "", e.vm1Key, body)
		if code != 200 || !strings.Contains(out, `"records_accepted":1`) {
			t.Fatalf("ingest %d: %d %s", i, code, out)
		}
	}

	code, out := e.do(t, "POST", "/v1/chat/completions", "", e.laptopKey, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if code != 200 {
		t.Fatalf("proxy: %d %s", code, out)
	}

	deadline := time.Now().Add(2 * time.Second)
	var hosts map[string]core.UsageRow
	for {
		hosts = e.summary(t, "host")
		if len(hosts) == 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if hosts["vm1"].Requests != 1 || hosts["vm1"].InputTokens != 100 || hosts["laptop"].Requests != 1 {
		t.Fatalf("by host: %+v", hosts)
	}
	clients := e.summary(t, "client")
	if clients["claude-code"].Requests != 1 || clients["x"].Requests != 0 {
		t.Fatalf("by client: %+v", clients)
	}
	code, out = e.do(t, "GET", "/control/v1/usage?group=host", "router.tail", e.laptopKey, "")
	if code != 200 || !strings.Contains(out, `"key":"vm1"`) || !strings.Contains(out, `"key":"laptop"`) {
		t.Fatalf("usage group=host: %d %s", code, out)
	}
}

func (e *mhEnv) summary(t *testing.T, group string) map[string]core.UsageRow {
	t.Helper()
	rows, err := e.app.Ledger.Summary(t.Context(), time.Now().Add(-time.Hour), group)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]core.UsageRow{}
	for _, r := range rows {
		out[r.Key] = r
	}
	return out
}
