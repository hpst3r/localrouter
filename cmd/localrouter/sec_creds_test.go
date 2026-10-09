package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// secConfig writes a minimal config whose client key is 0600 and whose
// upstream api_key_file has the given mode. The upstream base_url is a
// refused loopback port so nothing leaves the host.
func secConfig(t *testing.T, apiKeyMode os.FileMode) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits not enforced on windows")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "c.key"), []byte("lr-client-key-0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	up := filepath.Join(dir, "up.key")
	if err := os.WriteFile(up, []byte("sk-upstream-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(up, apiKeyMode); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.yaml")
	body := `listen: 127.0.0.1:0
data_dir: data
clients:
  - {name: c, class: interactive, key_file: c.key}
accounts:
  - {id: oc, provider: openai_compat, base_url: "http://127.0.0.1:1/v1", api_key_file: up.key}
routes:
  - {name: r, models: [m], interactive: [oc]}
`
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// CRED-1: `localrouter check` fails on a group/world-readable upstream key.
func TestSecCheckRejectsLooseKeyFile(t *testing.T) {
	if err := cmdCheck([]string{"-config", secConfig(t, 0o600)}); err != nil {
		t.Fatalf("check rejected a valid config: %v", err)
	}
	err := cmdCheck([]string{"-config", secConfig(t, 0o644)})
	if err == nil || !strings.Contains(err.Error(), "api_key_file") || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("check accepted 0644 api_key_file: %v", err)
	}
}

// CRED-1: `localrouter serve` refuses to start (fails closed) on the same.
func TestSecServeRejectsLooseKeyFile(t *testing.T) {
	p := secConfig(t, 0o644)
	done := make(chan error, 1)
	go func() { done <- cmdServe([]string{"-config", p}) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "api_key_file") {
			t.Fatalf("serve error = %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve started with a 0644 api_key_file")
	}
}

// COL-3: --key-file over plain http to a non-loopback host warns.
func TestSecAdmitWarnsOnPlainHTTPKey(t *testing.T) {
	kf := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(kf, []byte("lr-client-key-0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv, _ := fakeControl(t, 200, `{"decision":"allow"}`)
	for _, c := range []struct {
		url  string
		warn bool
	}{
		{"http://192.0.2.1:9", true}, // TEST-NET-1; the 1ns timeout fails before any dial completes
		{"https://192.0.2.1:9", false},
		{srv.URL, false},
		{strings.Replace(srv.URL, "127.0.0.1", "localhost", 1), false},
	} {
		var stderr bytes.Buffer
		runAdmit([]string{"--account", "a", "--url", c.url, "--key-file", kf, "--timeout", "1ns"}, io.Discard, &stderr)
		got := strings.Contains(stderr.String(), "unencrypted")
		if got != c.warn {
			t.Errorf("%s: warned=%v, want %v (stderr %q)", c.url, got, c.warn, stderr.String())
		}
		if strings.Contains(stderr.String(), "lr-client-key") {
			t.Fatal("key printed")
		}
	}
	// No key file: nothing to warn about.
	var stderr bytes.Buffer
	runAdmit([]string{"--account", "a", "--url", "http://192.0.2.1:9", "--timeout", "1ns"}, io.Discard, &stderr)
	if strings.Contains(stderr.String(), "unencrypted") {
		t.Errorf("warned without --key-file: %q", stderr.String())
	}
}

// COL-3: admit must not follow a redirect with the client key.
func TestSecAdmitDoesNotFollowRedirect(t *testing.T) {
	kf := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(kf, []byte("lr-client-key-0123456789abcdef\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var leaked string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization")
		w.Write([]byte(`{"decision":"allow"}`))
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	err := runAdmit([]string{"--account", "a", "--url", srv.URL, "--key-file", kf}, io.Discard, io.Discard)
	if leaked != "" {
		t.Fatalf("redirect followed with key: %q", leaked)
	}
	if exitCode(err) != 2 {
		t.Fatalf("want exit 2 on redirect, got %v", err)
	}
}
