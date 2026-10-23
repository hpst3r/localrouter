package app_test

// P1 (pooled backends, flags off): with no route using `balance`, routing
// decisions, ledger rows, attempt events, policy-denied log lines,
// /control/v1/status, diagnostics and admit dry runs are byte-identical to
// the pre-pool base. testdata/pool_p1_golden.txt was rendered from the base
// commit (2d296aa, before any pool source edit) with -update-pool-golden and
// must never be regenerated to make a pool change pass.

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/config"
)

var updatePoolGolden = flag.Bool("update-pool-golden", false, "rewrite testdata/pool_p1_golden.txt")

// p1Upstream serves /a/v1/... and /b/v1/...; account a answers 502 before the
// body for an input containing "fail-a".
func p1Upstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		acct := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)[0]
		w.Header().Set("Content-Type", "application/json")
		if acct == "a" && bytes.Contains(body, []byte("fail-a")) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":{"message":"busy"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "resp-" + acct, "object": "response",
			"usage": map[string]any{"input_tokens": 10, "output_tokens": 5},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func p1ConfigYAML(upURL, deadURL, extra string) string {
	return fmt.Sprintf(`listen: 127.0.0.1:0
host_name: goldenhost
data_dir: data
pricing_file: pricing.yaml
control: {require_auth: true}
policy: {breaker: {threshold: 1, base: 10s, max: 40s}}
clients:
  - {name: ide, class: interactive, key_file: ide.key}
  - {name: bot, class: background, key_file: bot.key}
accounts:
  - {id: a, provider: openai_compat, base_url: %[1]s/a/v1, api_key_env: HR_UPSTREAM_KEY}
  - {id: b, provider: openai_compat, base_url: %[1]s/b/v1, api_key_env: HR_UPSTREAM_KEY}
  - {id: dead, provider: openai_compat, base_url: %[2]s/v1, api_key_env: HR_UPSTREAM_KEY}
routes:
  - name: r1
    models: [m1]
    upstream_model: up-m1
    interactive: [a, b]
    background: [b, a]
  - name: r2
    models: [m2]
    interactive: [a]
    upstreams:
      a: {protocols: [responses, chat], input_modalities: [text]}
  - name: r3
    models: [m3]
    interactive: [dead]
%[3]s`, upURL, deadURL, extra)
}

// p1Scenario drives the fixed flags-off scenario and renders every observable
// surface into one normalized transcript.
func p1Scenario(t *testing.T, extraYAML string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	hrWriteFile(t, filepath.Join(dir, "ide.key"), "ide-key-0123456789abcdef")
	hrWriteFile(t, filepath.Join(dir, "bot.key"), "bot-key-0123456789abcdef")
	hrWriteFile(t, filepath.Join(dir, "pricing.yaml"), "models:\n  up-m1:\n    input: 1\n    output: 2\n")
	up := p1Upstream(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	cfgPath := filepath.Join(dir, "config.yaml")
	hrWriteFile(t, cfgPath, p1ConfigYAML(up.URL, deadURL, extraYAML))
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var logs lockedBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		},
	}))
	clk := &brClock{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}
	a, err := app.Build(cfg, logger, app.Overrides{Clock: clk})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	e := &hrEnv{t: t, dir: dir, cfgPath: cfgPath, a: a, srv: httptest.NewServer(a.Handler),
		client: &http.Client{Timeout: 20 * time.Second}}
	t.Cleanup(e.srv.Close)

	var out strings.Builder
	call := func(label, method, path, key, body string, hdr map[string]string) {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, e.srv.URL+path, rd)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		if rd != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := e.client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Fprintf(&out, "== %s: %s %s\nstatus %d retry-after=%q\n%s\n", label, method, path,
			resp.StatusCode, resp.Header.Get("Retry-After"), strings.TrimSpace(string(b)))
	}
	ide, bot := "ide-key-0123456789abcdef", "bot-key-0123456789abcdef"
	resp := func(model, input string) string { return fmt.Sprintf(`{"model":%q,"input":%q}`, model, input) }

	call("interactive ok", "POST", "/v1/responses", ide, resp("m1", "hi"), nil)
	call("interactive failover", "POST", "/v1/responses", ide, resp("m1", "fail-a"), nil)
	call("background order", "POST", "/v1/responses", bot, resp("m1", "hi"), nil)
	call("session label", "POST", "/v1/responses", ide, resp("m1", "hi"), map[string]string{"X-LocalRouter-Session": "sess-1"})
	call("chat", "POST", "/v1/chat/completions", ide, `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`, nil)
	call("capability unsupported", "POST", "/v1/responses", ide, `{"model":"m2","input":"hi","tools":[{"type":"function","name":"f"}]}`, nil)
	call("capability ok", "POST", "/v1/responses", ide, resp("m2", "hi"), nil)
	call("dead upstream", "POST", "/v1/responses", ide, resp("m3", "hi"), nil)
	call("breaker refusal", "POST", "/v1/responses", ide, resp("m3", "hi"), nil)
	call("unknown model", "POST", "/v1/responses", ide, resp("nope", "hi"), nil)
	for _, body := range []string{
		`{"class":"interactive","model":"m1"}`,
		`{"class":"background","model":"m1"}`,
		`{"class":"interactive","model":"m3"}`,
		`{"class":"interactive","account":"b"}`,
		`{"class":"interactive","model":"m2","requirements":{"protocol":"responses","tools":true}}`,
		`{"class":"interactive","model":"m2","requirements":{"protocol":"responses","input_modalities":["text"]}}`,
	} {
		call("admit "+body, "POST", "/control/v1/admit", ide, body, nil)
	}
	call("status", "GET", "/control/v1/status", ide, "", nil)
	call("diagnostics", "GET", "/control/v1/diagnostics", ide, "", nil)

	out.WriteString("== ledger\n")
	db, err := sql.Open("sqlite", filepath.Join(dir, "data", "localrouter.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT * FROM requests ORDER BY started_at, rowid`)
	if err != nil {
		t.Fatal(err)
	}
	cols, _ := rows.Columns()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		parts := make([]string, len(cols))
		for i, c := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			parts[i] = fmt.Sprintf("%s=%v", c, v)
		}
		out.WriteString(strings.Join(parts, " ") + "\n")
	}
	rows.Close()

	out.WriteString("== log events\n")
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, `"msg":"attempt completed"`) || strings.Contains(line, `"msg":"policy denied"`) {
			out.WriteString(line + "\n")
		}
	}
	return p1Normalize(out.String(), up.URL, deadURL)
}

var p1HexID = regexp.MustCompile(`\b[0-9a-f]{32}\b`)

// p1Normalize replaces the ephemeral loopback URLs and the random record IDs
// (in order of first appearance) so the transcript is deterministic.
func p1Normalize(s, upURL, deadURL string) string {
	s = strings.ReplaceAll(s, upURL, "UPSTREAM")
	s = strings.ReplaceAll(s, deadURL, "DEAD")
	s = strings.ReplaceAll(s, strings.TrimPrefix(deadURL, "http://"), "DEADHOST")
	ids := map[string]string{}
	return p1HexID.ReplaceAllStringFunc(s, func(id string) string {
		if n, ok := ids[id]; ok {
			return n
		}
		ids[id] = fmt.Sprintf("ID%d", len(ids)+1)
		return ids[id]
	})
}

func TestP1FlagsOffGolden(t *testing.T) {
	got := p1Scenario(t, "")
	path := filepath.Join("testdata", "pool_p1_golden.txt")
	if *updatePoolGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := range max(len(gl), len(wl)) {
			var g, w string
			if i < len(gl) {
				g = gl[i]
			}
			if i < len(wl) {
				w = wl[i]
			}
			if g != w {
				t.Fatalf("flags-off transcript differs from the base golden at line %d:\n got: %s\nwant: %s", i+1, g, w)
			}
		}
	}
}
