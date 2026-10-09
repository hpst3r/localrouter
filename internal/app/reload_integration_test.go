package app_test

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/config"
)

func TestReloadRotationRollbackAndSharedState(t *testing.T) {
	dir := t.TempDir()
	key1 := "reload-old-client-key-0123456789"
	key2 := "reload-new-client-key-0123456789"
	for n, k := range map[string]string{"old.key": key1, "new.key": key2} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(k), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("RELOAD_TEST_UPSTREAM_KEY", "fake-upstream-key")
	path := filepath.Join(dir, "config.yaml")
	write := func(keys, model, extra string) {
		t.Helper()
		text := fmt.Sprintf(`listen: 127.0.0.1:0
data_dir: %s/data
control: {require_auth: true}
clients:
 - name: tester
   class: interactive
   %s
accounts:
 - {id: upstream, provider: openai_compat, base_url: http://127.0.0.1:1/v1, api_key_env: RELOAD_TEST_UPSTREAM_KEY}
routes:
 - {name: test, models: [%s], interactive: [upstream], background: [upstream]}
%s
`, dir, keys, model, extra)
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("key_file: old.key", "old-model", "")
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Build(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	auth, quota, pol, lim, led := a.Auth, a.Quota, a.Policy, a.Limiter, a.Ledger
	get := func(key string) (int, string) {
		r := httptest.NewRequest("GET", "http://localhost/v1/models", nil)
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		a.Handler.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	if a.ReloadStatus().Generation != 1 {
		t.Fatal("initial generation")
	}
	write("key_files: [old.key, new.key]", "new-model", "limits: {max_concurrent: 3}")
	st, err := a.Reload(path)
	if err != nil || st.Generation != 2 || !st.OK {
		t.Fatalf("reload %#v %v", st, err)
	}
	for _, key := range []string{key1, key2} {
		code, b := get(key)
		if code != 200 || !strings.Contains(b, "new-model") {
			t.Fatalf("models %d %s", code, b)
		}
	}
	write("key_file: new.key", "final-model", "")
	if _, err = a.Reload(path); err != nil {
		t.Fatal(err)
	}
	if code, _ := get(key1); code != 401 {
		t.Fatalf("removed key accepted %d", code)
	}
	if auth != a.Auth || quota != a.Quota || pol != a.Policy || lim != a.Limiter || led != a.Ledger {
		t.Fatal("persistent state rebuilt")
	}
	generation := a.ReloadStatus().Generation
	write("key_file: missing-secret.key", "bad-model", "")
	st, err = a.Reload(path)
	if err == nil || st.OK || st.Generation != generation || strings.Contains(st.Reason, dir) {
		t.Fatalf("bad key rollback %#v %v", st, err)
	}
	if code, b := get(key2); code != 200 || !strings.Contains(b, "final-model") {
		t.Fatalf("last good lost %d %s", code, b)
	}
	write("key_file: new.key", "bad-model", "timeouts: {body: 2s}")
	if st, err = a.Reload(path); err == nil || st.Generation != generation || len(st.RestartOnly) == 0 {
		t.Fatalf("structural change accepted %#v %v", st, err)
	}
	write("key_file: new.key", "good-model", "")
	candidate, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.ReloadConfig(candidate); err != nil {
		t.Fatal(err)
	}
	candidate.Routes[0].Models[0] = "MUTATED"
	if _, b := get(key2); strings.Contains(b, "MUTATED") || !strings.Contains(b, "good-model") {
		t.Fatal("caller input aliases runtime")
	}
	r := httptest.NewRequest(http.MethodGet, "http://localhost/control/v1/diagnostics", nil)
	r.Header.Set("Authorization", "Bearer "+key2)
	w := httptest.NewRecorder()
	a.Handler.ServeHTTP(w, r)
	var doc map[string]any
	if err = json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["reload"] == nil {
		t.Fatal("reload diagnostics not wired")
	}
}
