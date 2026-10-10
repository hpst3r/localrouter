package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// CRED-3: the ingest client must not follow a redirect (net/http re-sends
// Authorization to the same hostname on any port) and must report it.
func TestSecAgentDoesNotFollowRedirect(t *testing.T) {
	var got string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	}))
	defer other.Close()
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer router.Close()
	for _, hc := range []*http.Client{nil, {Timeout: 5 * time.Second}} {
		c := NewClient(router.URL, "h1", "lr-agent-secret-key-0123456789", hc)
		_, err := c.Ingest(context.Background(), core.IngestRequest{})
		if got != "" {
			t.Fatalf("redirect followed; key sent to other port: %q", got)
		}
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != http.StatusTemporaryRedirect || !strings.Contains(err.Error(), "redirect") {
			t.Fatalf("want http 307 redirect error, got %v", err)
		}
		if he.Permanent() {
			t.Fatal("redirect must be transient (records kept)")
		}
	}
}

// NewClient must not mutate the caller's http.Client.
func TestSecAgentClientNotMutated(t *testing.T) {
	hc := &http.Client{}
	NewClient("http://127.0.0.1:1", "h1", testKey, hc)
	if hc.CheckRedirect != nil {
		t.Fatal("caller's client was mutated")
	}
}

// CRED-1: the agent key file must not be group/world accessible.
func TestSecAgentReadKeyRejectsLooseMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits not enforced on windows")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.key")
	if err := os.WriteFile(p, []byte(testKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadKey(p); err != nil {
		t.Fatalf("0600 rejected: %v", err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadKey(p)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("0644 key accepted or unclear error: %v", err)
	}
	if strings.Contains(err.Error(), dir) || strings.Contains(err.Error(), testKey) {
		t.Fatalf("error leaks path or key: %v", err)
	}
	if _, err := ReadKey(dir); err == nil || strings.Contains(err.Error(), dir) {
		t.Fatalf("directory accepted or path leaked: %v", err)
	}
}

// CRED-3: plain http to a non-loopback host is flagged (warned, not refused).
func TestSecInsecureServerURL(t *testing.T) {
	for _, c := range []struct {
		url  string
		want bool
	}{
		{"https://router.example:8787", false},
		{"http://127.0.0.1:8787", false},
		{"http://127.1.2.3:8787", false},
		{"http://localhost:8787", false},
		{"http://LOCALHOST", false},
		{"http://[::1]:8787", false},
		{"http://router.tailnet.ts.net:8787", true},
		{"http://100.64.0.1:8787", true},
		{"http://localhost.evil.example", true},
		{"HTTP://10.0.0.1", true},
	} {
		if got := InsecureServerURL(c.url); got != c.want {
			t.Errorf("InsecureServerURL(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}
