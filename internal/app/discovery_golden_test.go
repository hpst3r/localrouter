package app_test

// D1 (flags-off) golden for the wired stack: a config.example.yaml-like config
// with no discovering route must produce byte-identical /v1/models, diagnostics
// and check_admission (POST /control/v1/admit) documents, plus the same routing
// decisions, as the build that predates model discovery. The goldens were
// captured before discovery existed; -update rewrites them.

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/config"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files in testdata")

type goldenClock struct{}

func (goldenClock) Now() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }

const goldenKey = "golden-client-key-0123456789"

func goldenConfigYAML(upstreamURL string) string {
	return fmt.Sprintf(`listen: 127.0.0.1:0
data_dir: data
policy: {max_failovers: 0}
clients:
  - {name: me, class: interactive, key_file: me.key}
accounts:
  - id: codex-primary
    provider: codex
    reserve: { 5h: 0.10, weekly: 0.10 }
  - id: claude-max
    provider: claude
  - id: ollama-cloud
    provider: ollama
    base_url: %[1]s/ollama/v1
    api_key_env: HR_UPSTREAM_KEY
    reserve: { 5h: 0.10, weekly: 0.10 }
  - id: compat-a
    provider: openai_compat
    base_url: %[1]s/a/v1
    api_key_env: HR_UPSTREAM_KEY
routes:
  - name: gpt
    models: [gpt-5.6-sol, gpt-5.6-luna]
    interactive: [codex-primary]
  - name: ollama
    models: [deepseek-v4.1-flash, kimi-k3]
    interactive: [ollama-cloud]
    background:  [ollama-cloud]
  - name: alias
    models: [alias-model]
    upstream_model: backend-alias
    interactive: [compat-a, ollama-cloud]
    background: [compat-a]
`, upstreamURL)
}

type goldenEnv struct {
	t   *testing.T
	a   *app.App
	srv *httptest.Server

	mu   sync.Mutex
	hits []string // "path model"
	up   *httptest.Server
}

func newGoldenEnv(t *testing.T, cfgYAML func(upstreamURL string) string) *goldenEnv {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	hrWriteFile(t, filepath.Join(dir, "me.key"), goldenKey)
	e := &goldenEnv{t: t}
	e.up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var doc struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &doc)
		e.mu.Lock()
		e.hits = append(e.hits, r.URL.Path+" "+doc.Model)
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"r","object":"response","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	t.Cleanup(e.up.Close)
	cfgPath := filepath.Join(dir, "config.yaml")
	hrWriteFile(t, cfgPath, cfgYAML(e.up.URL))
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	e.a, err = app.Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Overrides{Clock: goldenClock{}})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() { _ = e.a.Close() })
	e.srv = httptest.NewServer(e.a.Handler)
	t.Cleanup(e.srv.Close)
	return e
}

func (e *goldenEnv) do(method, path, body string) (int, []byte) {
	e.t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+goldenKey)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (e *goldenEnv) takeHits() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	h := e.hits
	e.hits = nil
	return h
}

func checkAppGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from golden:\n got: %s\nwant: %s", name, got, want)
	}
}

func TestD1GoldenFlagsOffAppSurfaces(t *testing.T) {
	e := newGoldenEnv(t, goldenConfigYAML)
	_, models := e.do("GET", "/v1/models", "")
	checkAppGolden(t, "d1_models.json", models)
	_, diag := e.do("GET", "/control/v1/diagnostics", "")
	checkAppGolden(t, "d1_diagnostics.json", diag)

	var table bytes.Buffer
	for _, m := range []string{"gpt-5.6-sol", "deepseek-v4.1-flash", "kimi-k3", "alias-model", "unknown-model", "unsloth/qwen3"} {
		for _, class := range []string{"interactive", "background"} {
			_, admit := e.do("POST", "/control/v1/admit", fmt.Sprintf(`{"class":%q,"model":%q}`, class, m))
			fmt.Fprintf(&table, "admit %s %s -> %s", class, m, admit)
		}
		st, body := e.do("POST", "/v1/chat/completions", fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, m))
		if st != http.StatusOK {
			fmt.Fprintf(&table, "chat %s -> %d %s", m, st, body)
		} else {
			fmt.Fprintf(&table, "chat %s -> 200\n", m)
		}
		fmt.Fprintf(&table, "  hits %q\n", e.takeHits())
	}
	checkAppGolden(t, "d1_decisions.txt", table.Bytes())
}
