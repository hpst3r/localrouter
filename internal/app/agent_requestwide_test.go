package app_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/agent"
	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/claudelog"
	"github.com/hpst3r/localrouter/internal/config"
)

// A request-wide rejection (the agent's account is not configured on the
// server) must NOT be bisected into dropped records: the agent keeps its
// offsets and delivers everything once the server config is fixed.
func TestAgentDoesNotDropUsageOnServerConfigError(t *testing.T) {
	dir := t.TempDir()
	keys := filepath.Join(dir, "keys")
	if err := os.MkdirAll(keys, 0o700); err != nil {
		t.Fatal(err)
	}
	const key = "lr_test_ingest_key_0123456789abcdef"
	if err := os.WriteFile(filepath.Join(keys, "vm1.key"), []byte(key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeCfg := func(account string) *config.Config {
		body := "listen: 127.0.0.1:0\ndata_dir: " + dir + "\ncontrol: {require_auth: true}\n" +
			"clients:\n  - {name: vm1, class: background, host: vm1, key_file: keys/vm1.key, ingest: true}\n" +
			"accounts:\n  - {id: " + account + ", provider: claude, quota_source: agent}\n"
		p := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		c, err := config.Load(p)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// Transcripts with 3 valid messages for account "claude-max".
	projects := filepath.Join(dir, "projects", "-proj")
	if err := os.MkdirAll(projects, 0o700); err != nil {
		t.Fatal(err)
	}
	var lines []byte
	ts := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		b, _ := json.Marshal(map[string]any{
			"type": "assistant", "sessionId": "s", "requestId": "req_" + string(rune('a'+i)),
			"timestamp": ts.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
			"message": map[string]any{"id": "msg_" + string(rune('a'+i)), "model": "claude-opus-5-5",
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 5}},
		})
		lines = append(append(lines, b...), '\n')
	}
	f := filepath.Join(projects, "s.jsonl")
	if err := os.WriteFile(f, lines, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * time.Minute)
	_ = os.Chtimes(f, old, old)

	run := func(cfg *config.Config) (int, error) {
		a, err := app.Build(cfg, nil, app.Overrides{})
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		srv := httptest.NewServer(a.Handler)
		defer srv.Close()
		c := agent.NewClient(srv.URL, "vm1", key, srv.Client())
		col := claudelog.New(agent.NewRemoteLedger(c), claudelog.Options{
			Dir: filepath.Join(dir, "projects"), AccountID: "claude-max", Host: "vm1",
			StatePath: filepath.Join(dir, "agent-state.json"),
		})
		_, scanErr := col.ScanOnce(context.Background())
		rows, err := a.Ledger.Summary(context.Background(), time.Now().Add(-48*time.Hour), "host")
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, r := range rows {
			n += int(r.Requests)
		}
		return n, scanErr
	}

	// Server misconfigured: the account the agent reports to does not exist.
	n, err := run(writeCfg("some-other-claude"))
	if err == nil {
		t.Fatal("expected a transient error while the server rejects the whole request")
	}
	if n != 0 {
		t.Fatalf("rows while misconfigured = %d", n)
	}
	// Fix the server config: all 3 records must still arrive (nothing dropped).
	n, err = run(writeCfg("claude-max"))
	if err != nil {
		t.Fatalf("after fix: %v", err)
	}
	if n != 3 {
		t.Fatalf("rows after fix = %d, want 3 (valid usage was dropped)", n)
	}
	_ = http.StatusOK
}
