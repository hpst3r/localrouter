package app_test

// Backend model check acceptance tests (docs/SPEC.md "Backend model check")
// through the real App, reusing the discovery E2E upstream fake
// (discovery_e2e_test.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/modelcheck"
)

type mcLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *mcLogBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *mcLogBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// mcConfigYAML: a static route sending the bare repo id to the llama.cpp
// router account (also polled by the discovering route "lab"), and a static
// route for a single-model server whose loaded model was swapped.
func mcConfigYAML(upstreamURL, qwenUpstream string) string {
	return fmt.Sprintf(`listen: 127.0.0.1:0
data_dir: data
model_check: {interval: 1h}
clients:
  - {name: me, class: interactive, key_file: me.key}
accounts:
  - id: llama-lab
    provider: openai_compat
    base_url: %[1]s/lab/v1
    api_key_env: HR_UPSTREAM_KEY
  - id: llama-two
    provider: openai_compat
    base_url: %[1]s/two/v1
    api_key_env: HR_UPSTREAM_KEY
routes:
  - {name: qwen, models: [qwen], upstream_model: %[2]s, interactive: [llama-lab]}
  - {name: old, models: [qwen-old], interactive: [llama-two]}
  - name: lab
    discover_models: true
    discover_match: ["ggml-org/*"]
    interactive: [llama-lab]
`, upstreamURL, qwenUpstream)
}

func mcLoad(t *testing.T, dir, body string) *config.Config {
	t.Helper()
	p := filepath.Join(dir, "config.yaml")
	hrWriteFile(t, p, body)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return cfg
}

func (e *discEnv) modelCheck() (modelcheck.Diagnostics, bool, string) {
	e.t.Helper()
	_, b := e.do("GET", "/control/v1/diagnostics", "")
	var doc struct {
		ModelCheck *modelcheck.Diagnostics `json:"model_check"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		e.t.Fatalf("diagnostics: %v %s", err, b)
	}
	if doc.ModelCheck == nil {
		return modelcheck.Diagnostics{}, false, string(b)
	}
	return *doc.ModelCheck, true, string(b)
}

func (e *discEnv) waitModelCheck(what string, cond func(modelcheck.Diagnostics) bool) (modelcheck.Diagnostics, string) {
	e.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		d, ok, raw := e.modelCheck()
		if ok && cond(d) {
			return d, raw
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("%s: model_check never matched: %s", what, raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func verdict(d modelcheck.Diagnostics, route string) modelcheck.Verdict {
	for _, v := range d.Verdicts {
		if v.Route == route {
			return v
		}
	}
	return modelcheck.Verdict{}
}

// MC10: with model_check enabled, the App reports both incidents in the
// diagnostics model_check block, reads the account discovery already polls
// from discovery (one /models call total), logs one warn per change into
// missing/single_model and one info on return to ok, re-checks on reload, and
// never changes routing.
func TestMC10AppModelCheckE2E(t *testing.T) {
	up := newDiscUpstream(t)
	up.setRaw("lab", llamaRouterModels)
	up.setListing("two", "qwen-new")
	dir := t.TempDir()
	t.Setenv("HR_UPSTREAM_KEY", hrUpstreamKey)
	hrWriteFile(t, filepath.Join(dir, "me.key"), goldenKey)
	logs := &mcLogBuffer{}
	a, err := app.Build(mcLoad(t, dir, mcConfigYAML(up.srv.URL, "unsloth/Qwen3-8B-GGUF")), slog.New(slog.NewTextHandler(logs, nil)), app.Overrides{})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if a.ModelCheck == nil {
		t.Fatal("model_check enabled but App.ModelCheck is nil")
	}
	e := &discEnv{t: t, up: up, a: a, srv: httptest.NewServer(a.Handler), dir: dir}
	t.Cleanup(e.srv.Close)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Start(ctx)

	// The shared lab listing may still be pending at the first check; a
	// reload (same config) re-checks without re-fetching.
	e.waitModels("ggml-org/gemma-3-1b-it-GGUF")
	if _, err := a.ReloadConfig(mcLoad(t, dir, mcConfigYAML(up.srv.URL, "unsloth/Qwen3-8B-GGUF"))); err != nil {
		t.Fatalf("reload: %v", err)
	}
	d, raw := e.waitModelCheck("incidents", func(d modelcheck.Diagnostics) bool {
		return verdict(d, "qwen").Status == modelcheck.StatusMissing && verdict(d, "old").Status == modelcheck.StatusMissing
	})
	if v := verdict(d, "qwen"); v.Account != "llama-lab" || v.Expected != "unsloth/Qwen3-8B-GGUF" ||
		!slices.Equal(v.Suggestions, []string{"unsloth/Qwen3-8B-GGUF:Q4_K_M"}) || v.SingleModel {
		t.Fatalf("incident 1 verdict %+v", v)
	}
	if v := verdict(d, "old"); !v.SingleModel || v.Loaded != "qwen-new" || v.Expected != "qwen-old" {
		t.Fatalf("incident 2 verdict %+v", v)
	}
	if d.Interval != "1h0m0s" || d.CheckedAt == nil || len(d.Verdicts) != 2 || d.Summary.Missing != 2 {
		t.Fatalf("model_check block %+v", d)
	}
	srcs := map[string]string{}
	for _, ar := range d.Accounts {
		srcs[ar.Account] = ar.Source
	}
	if srcs["llama-lab"] != "discovery" || srcs["llama-two"] != "fetch" {
		t.Fatalf("account sources %v", srcs)
	}
	for _, leak := range []string{hrUpstreamKey, up.srv.URL, "architecture"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("diagnostics leaks %q", leak)
		}
	}
	if up.calls("lab") != 1 || up.calls("two") != 1 {
		t.Fatalf("/models calls lab=%d two=%d, want 1 each (shared listing, cached on reload)", up.calls("lab"), up.calls("two"))
	}

	// Routing is unchanged: the missing route still forwards its rewrite.
	if st, b := e.do("POST", "/v1/chat/completions", chatBody("qwen")); st != 200 {
		t.Fatalf("missing route no longer served: %d %s", st, b)
	}
	if h := up.takeHits(); len(h) != 1 || !strings.Contains(string(h[0].body), `"model":"unsloth/Qwen3-8B-GGUF"`) {
		t.Fatalf("forwarded hits %+v", h)
	}

	// An accepted reload fixing the id re-checks at once and logs recovery.
	if _, err := a.ReloadConfig(mcLoad(t, dir, mcConfigYAML(up.srv.URL, "unsloth/Qwen3-8B-GGUF:Q4_K_M"))); err != nil {
		t.Fatalf("reload: %v", err)
	}
	e.waitModelCheck("fixed", func(d modelcheck.Diagnostics) bool { return verdict(d, "qwen").Status == modelcheck.StatusOK })
	waitLog := func(substr string) string {
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(logs.String(), substr) {
			if time.Now().After(deadline) {
				t.Fatalf("log lacks %q:\n%s", substr, logs.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		return logs.String()
	}
	out := waitLog(`msg="backend model check ok" route=qwen account=llama-lab expected=unsloth/Qwen3-8B-GGUF`)
	if n := strings.Count(out, `msg="backend model missing" route=qwen`); n != 1 {
		t.Fatalf("%d missing warnings for qwen, want 1:\n%s", n, out)
	}
	if n := strings.Count(out, `msg="backend serves a different single model" route=old account=llama-two expected=qwen-old loaded=qwen-new`); n != 1 {
		t.Fatalf("%d single_model warnings, want 1:\n%s", n, out)
	}
	for _, leak := range []string{hrUpstreamKey, up.srv.URL} {
		if strings.Contains(out, leak) {
			t.Fatalf("logs leak %q", leak)
		}
	}

	// MC2: model_check is restart-only.
	changed := strings.Replace(mcConfigYAML(up.srv.URL, "x"), "interval: 1h", "interval: 2h", 1)
	st, err := a.ReloadConfig(mcLoad(t, dir, changed))
	if !errors.Is(err, app.ErrRestartRequired) || !slices.Equal(st.RestartOnly, []string{"model_check"}) {
		t.Fatalf("model_check reload: %v %+v, want restart required", err, st)
	}
}

// MC1: without model_check the App builds no checker and the diagnostics
// document has no model_check block (the D1 goldens cover byte identity).
func TestMC1FlagsOffNoModelCheck(t *testing.T) {
	up := newDiscUpstream(t)
	e := newDiscEnv(t, up, discConfigYAML(up.srv.URL), true)
	if e.a.ModelCheck != nil {
		t.Fatal("App.ModelCheck built without model_check")
	}
	if _, ok, raw := e.modelCheck(); ok || strings.Contains(raw, "model_check") {
		t.Fatalf("diagnostics has a model_check block: %s", raw)
	}
}
