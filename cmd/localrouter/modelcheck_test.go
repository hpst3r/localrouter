package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hpst3r/localrouter/internal/modelcheck"
)

const mcUpstreamKey = "sk-probe-upstream-secret-0123"

// mcUpstream serves a llama.cpp router-mode listing under /lab and a
// single-model listing under /single, and counts /models calls.
func mcUpstream(t *testing.T) (*httptest.Server, func() map[string]int) {
	var mu sync.Mutex
	calls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prefix, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		mu.Lock()
		calls[prefix]++
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer "+mcUpstreamKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch prefix {
		case "lab":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"unsloth/Qwen3.5-9B-GGUF:Q4_K_M","object":"model","owned_by":"llamacpp","status":{"value":"loaded"}},{"id":"ggml-org/gemma-3-1b-it-GGUF","object":"model","status":{"value":"unloaded"}}]}`)
		case "single":
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gemma-3-27b","object":"model","owned_by":"llamacpp","meta":{"n_ctx_train":32768}}]}`)
		default:
			http.Error(w, "boom "+mcUpstreamKey, http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for k, v := range calls {
			out[k] = v
		}
		return out
	}
}

// mcConfig writes a config whose routes are given; accounts lab, single and
// broken point at upstreamURL, cx is codex.
func mcConfig(t *testing.T, upstreamURL, routes string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range map[string]string{"c.key": "lr-client-key-0123456789abcdef\n", "up.key": mcUpstreamKey + "\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`listen: 127.0.0.1:0
data_dir: data
clients:
  - {name: c, class: interactive, key_file: c.key}
accounts:
  - {id: lab, provider: openai_compat, base_url: "%[1]s/lab/v1", api_key_file: up.key}
  - {id: single, provider: openai_compat, base_url: "%[1]s/single/v1", api_key_file: up.key}
  - {id: broken, provider: openai_compat, base_url: "%[1]s/broken/v1", api_key_file: up.key}
  - {id: cx, provider: codex}
routes:
%[2]s`, upstreamURL, routes)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// MC9: `check -probe` prints one line per non-ok verdict plus a summary and
// exits 1 on a missing id (both incidents); -json prints the structured
// result; plain `check` stays offline and unchanged; nothing printed carries
// the key, URL or upstream body.
func TestMC9CheckProbeCLI(t *testing.T) {
	srv, calls := mcUpstream(t)
	cfg := mcConfig(t, srv.URL, `  - {name: qwen, models: [qwen3.5-9b], upstream_model: unsloth/Qwen3.5-9B-GGUF, interactive: [lab]}
  - {name: gemma, models: [ggml-org/gemma-3-1b-it-GGUF], interactive: [lab]}
  - {name: old, models: [qwen3-32b], interactive: [single]}
  - {name: gpt, models: [gpt-5], interactive: [cx]}
`)
	var out bytes.Buffer
	if err := runCheck([]string{"-config", cfg}, &out); err != nil {
		t.Fatalf("plain check: %v", err)
	}
	if want := "config OK: 1 clients, 4 accounts, 4 routes; data_dir=" + filepath.Join(filepath.Dir(cfg), "data") + "\n"; out.String() != want {
		t.Fatalf("plain check output %q, want %q", out.String(), want)
	}
	if n := len(calls()); n != 0 {
		t.Fatalf("plain check made %d upstream calls", n)
	}

	out.Reset()
	err := runCheck([]string{"-config", cfg, "-probe"}, &out)
	if exitCode(err) != 1 {
		t.Fatalf("probe exit %v, want code 1", err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	want := []string{
		lines[0],
		"missing    route=qwen account=lab expected=unsloth/Qwen3.5-9B-GGUF suggestions=unsloth/Qwen3.5-9B-GGUF:Q4_K_M",
		"missing    route=old account=single expected=qwen3-32b single_model=true loaded=gemma-3-27b",
		"ineligible route=gpt account=cx expected=gpt-5 provider=codex",
		"model check: 1 ok, 2 missing (1 single_model), 0 unchecked, 1 ineligible; 2 accounts probed",
	}
	if !strings.HasPrefix(lines[0], "config OK: ") || strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Fatalf("probe output:\n%s\nwant:\n%s", out.String(), strings.Join(want, "\n"))
	}
	if c := calls(); c["lab"] != 1 || c["single"] != 1 || c["broken"] != 0 {
		t.Fatalf("upstream calls %v, want one per probed account", c)
	}

	out.Reset()
	err = runCheck([]string{"-config", cfg, "-probe", "-json"}, &out)
	var res modelcheck.Result
	if exitCode(err) != 1 || json.Unmarshal(out.Bytes(), &res) != nil {
		t.Fatalf("-json: exit %v, output %q", err, out.String())
	}
	if res.Summary != (modelcheck.Summary{OK: 1, Missing: 2, SingleModel: 1, Ineligible: 1}) || len(res.Verdicts) != 4 || res.CheckedAt == nil {
		t.Fatalf("-json result %+v", res)
	}
	for _, leak := range []string{mcUpstreamKey, srv.URL, "boom"} {
		if strings.Contains(out.String(), leak) {
			t.Fatalf("-json output leaks %q", leak)
		}
	}

	if err := runCheck([]string{"-config", cfg, "-json"}, io.Discard); exitCode(err) != 2 {
		t.Fatalf("-json without -probe: %v, want usage error", err)
	}
}

// MC9: unchecked alone exits 0 (printed), and non-zero with -strict.
func TestMC9CheckProbeUncheckedStrict(t *testing.T) {
	srv, _ := mcUpstream(t)
	cfg := mcConfig(t, srv.URL, `  - {name: gemma, models: [ggml-org/gemma-3-1b-it-GGUF], interactive: [lab]}
  - {name: down, models: [m], interactive: [broken]}
`)
	var out bytes.Buffer
	if err := runCheck([]string{"-config", cfg, "-probe"}, &out); err != nil {
		t.Fatalf("unchecked without -strict: %v", err)
	}
	if !strings.Contains(out.String(), "unchecked  route=down account=broken expected=m error_class=status\n") ||
		!strings.Contains(out.String(), "model check: 1 ok, 0 missing (0 single_model), 1 unchecked, 0 ineligible; 2 accounts probed\n") {
		t.Fatalf("output %q", out.String())
	}
	if strings.Contains(out.String(), mcUpstreamKey) || strings.Contains(out.String(), "boom") {
		t.Fatalf("output leaks the key or body: %q", out.String())
	}
	if err := runCheck([]string{"-config", cfg, "-probe", "-strict"}, io.Discard); exitCode(err) != 1 {
		t.Fatalf("-strict: %v, want code 1", err)
	}
}
