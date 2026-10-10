package quota

import (
	"context"
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

	"github.com/hpst3r/localrouter/internal/auth"
	"github.com/hpst3r/localrouter/internal/core"
)

// CRED-2: an ollama account whose base_url host differs from the usage URL
// host must not send its key to the usage URL.
func TestSecOllamaKeyNotSentToForeignUsageHost(t *testing.T) {
	var mu sync.Mutex
	var gotAuth string
	usageHost := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		w.Write([]byte(`{"limits":{}}`))
	}))
	defer usageHost.Close()
	configuredBase := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("configured base_url host hit for quota: %s", r.URL.Path)
	}))
	defer configuredBase.Close()

	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("sk-other-service-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Both test servers listen on 127.0.0.1; "localhost" makes the base_url
	// host differ from the usage host as ollama-proxy.internal would differ
	// from ollama.com.
	base := strings.Replace(configuredBase.URL, "127.0.0.1", "localhost", 1) + "/v1"
	accts := []core.Account{{ID: "ol", Provider: core.ProviderOllama, BaseURL: base}}
	creds := auth.New(accts, map[string]auth.StaticKey{"ol": {File: keyFile}}, nil, auth.Options{})
	m := New(accts, creds, Options{
		HTTPClient:     usageHost.Client(),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		OllamaUsageURL: usageHost.URL + "/api/usage",
		PollInterval:   time.Hour,
	})
	m.refresh(context.Background(), "ol", true)
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "" {
		t.Fatalf("key sent to foreign usage host: %q", gotAuth)
	}
	s, ok := m.Latest("ol")
	if !ok || !strings.Contains(s.Err, "usage polling unavailable") {
		t.Fatalf("want usage-polling-unavailable Err, got %+v ok=%v", s, ok)
	}
	if strings.Contains(s.Err, "secret") || strings.Contains(s.Err, "localhost") {
		t.Fatalf("Err leaks detail: %q", s.Err)
	}
}

func TestSecOllamaUsageHostMatch(t *testing.T) {
	for _, c := range []struct {
		base, usage string
		want        bool
	}{
		{"https://ollama.com/v1", DefaultOllamaUsageURL, true},
		{"https://OLLAMA.com:443/v1", DefaultOllamaUsageURL, true},
		{"https://ollama.com./v1", DefaultOllamaUsageURL, false},
		{"https://api.ollama.com/v1", DefaultOllamaUsageURL, false},
		{"https://ollama-proxy.internal/v1", DefaultOllamaUsageURL, false},
		{"https://ollama.com.evil.example/v1", DefaultOllamaUsageURL, false},
		{"", DefaultOllamaUsageURL, false},
		{"://bad", DefaultOllamaUsageURL, false},
		{"http://127.0.0.1:1234/v1", "http://127.0.0.1:5678/api/usage", true},
	} {
		if got := sameHost(c.base, c.usage); got != c.want {
			t.Errorf("sameHost(%q, %q) = %v, want %v", c.base, c.usage, got, c.want)
		}
	}
}

// CRED-2/CRED-3: usage fetches must not follow redirects (net/http keeps
// Authorization on same-host redirects regardless of port).
func TestSecUsageFetchesDoNotFollowRedirects(t *testing.T) {
	var mu sync.Mutex
	var leaked []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		leaked = append(leaked, r.URL.Path+" "+r.Header.Get("Authorization"))
		mu.Unlock()
		w.Write([]byte(`{}`))
	}))
	defer other.Close()
	for _, acct := range [][]core.Account{codexAcct(), ollamaAcct()} {
		h := newHarness(t, acct, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
		})
		id := acct[0].ID
		h.m.refresh(context.Background(), id, true)
		s, _ := h.m.Latest(id)
		if !strings.Contains(s.Err, "http 307") {
			t.Errorf("%s: want http 307 error, got %q", id, s.Err)
		}
	}

	// Claude (also used by the agent through FetchClaudeSnapshot).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	_, err := FetchClaudeSnapshot(context.Background(), srv.Client(), srv.URL+"/api/oauth/usage", "ua",
		ClaudeCredential{AccessToken: "sk-ant-oat-secret"}, "cl", time.Now())
	if err == nil || !strings.Contains(err.Error(), "http 307") {
		t.Errorf("claude: want http 307 error, got %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(leaked) != 0 {
		t.Fatalf("redirect followed with credentials: %q", leaked)
	}
}
