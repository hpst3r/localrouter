package quota

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const claudeToken = "sk-ant-oat01-SECRET-claude-token"

// Real /api/oauth/usage response shape (sanitized).
const claudeUsageBody = `{"five_hour":{"utilization":27.0,"resets_at":"2026-10-01T23:00:00.506990+00:00"},
 "seven_day":{"utilization":7.0,"resets_at":"2026-10-02T20:00:00.507011+00:00"},
 "seven_day_oauth_apps":null,"seven_day_opus":null,"seven_day_sonnet":null,
 "extra_usage":{"is_enabled":false},
 "limits":[{"kind":"session","group":"session","percent":27,"resets_at":"2026-10-01T23:00:00.506990+00:00"},
  {"kind":"weekly_all","group":"weekly","percent":7,"resets_at":"2026-10-02T20:00:00.507011+00:00"},
  {"kind":"weekly_scoped","group":"weekly","percent":0,"resets_at":"2026-10-02T20:00:00.507187+00:00",
   "scope":{"model":{"id":null,"display_name":"Fable"},"surface":null}}]}`

type claudeHarness struct {
	m     *Manager
	clock *fakeClock
	hits  atomic.Int32
	logs  *bytes.Buffer
	path  string
}

func writeClaudeCreds(t *testing.T, path string, expiresAt time.Time) {
	t.Helper()
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":%q,"refreshToken":"sk-ant-ort01-REFRESH","expiresAt":%d,"scopes":["user:inference"],"subscriptionType":"max","rateLimitTier":"default_claude_max_20x"}}`,
		claudeToken, expiresAt.UnixMilli())
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newClaudeHarness(t *testing.T, handler http.HandlerFunc) *claudeHarness {
	t.Helper()
	h := &claudeHarness{clock: &fakeClock{t: t0}, logs: &bytes.Buffer{}}
	h.path = filepath.Join(t.TempDir(), ".credentials.json")
	writeClaudeCreds(t, h.path, t0.Add(time.Hour))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	h.m = New([]core.Account{{ID: "cl", Provider: core.ProviderClaude}}, nil, Options{
		Clock:          h.clock,
		HTTPClient:     srv.Client(),
		Logger:         slog.New(slog.NewTextHandler(h.logs, nil)),
		ClaudeUsageURL: srv.URL + "/api/oauth/usage",
		ClaudeCredentialsFile: func(id string) string {
			if id != "cl" {
				t.Errorf("credentials requested for %q", id)
			}
			return h.path
		},
		PollInterval: time.Hour,
	})
	return h
}

func (h *claudeHarness) refresh(t *testing.T) core.Snapshot {
	t.Helper()
	h.m.refresh(context.Background(), "cl", true)
	s, ok := h.m.Latest("cl")
	if !ok {
		t.Fatal("no snapshot")
	}
	return s
}

func (h *claudeHarness) assertNoSecrets(t *testing.T, s core.Snapshot) {
	t.Helper()
	for _, secret := range []string{claudeToken, "REFRESH"} {
		if strings.Contains(s.Err, secret) || strings.Contains(h.logs.String(), secret) {
			t.Fatalf("secret leaked: err=%q logs=%q", s.Err, h.logs.String())
		}
	}
}

func TestClaudeMapping(t *testing.T) {
	h := newClaudeHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/oauth/usage" || r.Method != http.MethodGet {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+claudeToken {
			t.Errorf("auth header mismatch")
		}
		if got := r.Header.Get("anthropic-beta"); got != "oauth-2025-04-20" {
			t.Errorf("anthropic-beta %q", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("accept %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != DefaultClaudeUserAgent {
			t.Errorf("user-agent %q", got)
		}
		w.Write([]byte(claudeUsageBody))
	})
	s := h.refresh(t)
	if s.Err != "" || s.Plan != "max" || s.Source != SourceUsageAPI || !s.FetchedAt.Equal(t0) || s.Allowed != nil {
		t.Fatalf("snapshot %+v", s)
	}
	if len(s.Windows) != 3 {
		t.Fatalf("windows %+v", s.Windows)
	}
	w5 := findWindow(t, s, core.Window5h)
	if !approx(w5.UsedFrac, 0.27) || w5.WindowSeconds != 18000 {
		t.Fatalf("5h %+v", w5)
	}
	if want := time.Date(2026, 10, 1, 23, 0, 0, 506990000, time.UTC); !w5.ResetAt.Equal(want) {
		t.Fatalf("5h reset %v", w5.ResetAt)
	}
	wk := findWindow(t, s, core.WindowWeekly)
	if !approx(wk.UsedFrac, 0.07) || wk.WindowSeconds != 604800 {
		t.Fatalf("weekly %+v", wk)
	}
	if want := time.Date(2026, 10, 2, 20, 0, 0, 507011000, time.UTC); !wk.ResetAt.Equal(want) {
		t.Fatalf("weekly reset %v", wk.ResetAt)
	}
	wf := findWindow(t, s, "weekly_fable")
	if wf.UsedFrac != 0 || wf.WindowSeconds != 604800 || !wf.ResetAt.Equal(time.Date(2026, 10, 2, 20, 0, 0, 507187000, time.UTC)) {
		t.Fatalf("weekly_fable %+v", wf)
	}
	h.assertNoSecrets(t, s)
}

func TestClaudeExtraWindowsAndClamp(t *testing.T) {
	h := newClaudeHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"five_hour":{"utilization":150,"resets_at":null},
			"seven_day":{"utilization":-3,"resets_at":"bogus"},
			"seven_day_opus":{"utilization":100.0,"resets_at":"2026-10-02T20:00:00+00:00"},
			"seven_day_sonnet":null,
			"seven_day_extra":{"utilization":null},
			"limits":[{"kind":"weekly_scoped","percent":50,"scope":{"model":{"display_name":"Opus"}}},
				{"kind":"weekly_scoped","percent":12.5,"scope":{"model":{"display_name":"Big Model"}}},
				{"kind":"weekly_scoped","percent":40,"scope":{"model":{"display_name":null}}},
				{"kind":"weekly_scoped","percent":40}]}`))
	})
	s := h.refresh(t)
	if s.Err != "" {
		t.Fatalf("err %q", s.Err)
	}
	if w := findWindow(t, s, core.Window5h); w.UsedFrac != 1 || !w.ResetAt.IsZero() {
		t.Fatalf("5h %+v", w)
	}
	if w := findWindow(t, s, core.WindowWeekly); w.UsedFrac != 0 || !w.ResetAt.IsZero() {
		t.Fatalf("weekly %+v", w)
	}
	// seven_day_opus wins over the limits[] entry for the same kind.
	if w := findWindow(t, s, "weekly_opus"); w.UsedFrac != 1 || w.WindowSeconds != 604800 || w.ResetAt.IsZero() {
		t.Fatalf("weekly_opus %+v", w)
	}
	if w := findWindow(t, s, "weekly_big_model"); !approx(w.UsedFrac, 0.125) {
		t.Fatalf("weekly_big_model %+v", w)
	}
	if len(s.Windows) != 4 {
		t.Fatalf("windows %+v", s.Windows)
	}
}

func TestClaudeExpiredTokenSkipsNetwork(t *testing.T) {
	h := newClaudeHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(claudeUsageBody))
	})
	s := h.refresh(t)
	if s.Err != "" || len(s.Windows) != 3 || h.hits.Load() != 1 {
		t.Fatalf("first fetch %+v hits=%d", s, h.hits.Load())
	}
	// Exactly at expiresAt counts as expired.
	h.clock.Advance(time.Hour)
	s = h.refresh(t)
	if h.hits.Load() != 1 {
		t.Fatalf("expired token hit the network: hits=%d", h.hits.Load())
	}
	if s.Err != "claude token expired; run claude to refresh" {
		t.Fatalf("err %q", s.Err)
	}
	if len(s.Windows) != 3 || !s.FetchedAt.Equal(t0) {
		t.Fatalf("last-good not kept: %+v", s)
	}
	h.assertNoSecrets(t, s)
}

func TestClaudeExpiredFirstFetch(t *testing.T) {
	h := newClaudeHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("network hit")
	})
	writeClaudeCreds(t, h.path, t0.Add(-time.Minute))
	s := h.refresh(t)
	if h.hits.Load() != 0 || s.Err != "claude token expired; run claude to refresh" || !s.FetchedAt.IsZero() {
		t.Fatalf("snapshot %+v hits=%d", s, h.hits.Load())
	}
}

func TestClaude401(t *testing.T) {
	h := newClaudeHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid token ` + claudeToken + `"}`))
	})
	s := h.refresh(t)
	if s.Err != "claude token rejected; run claude to refresh" {
		t.Fatalf("err %q", s.Err)
	}
	if h.hits.Load() != 1 {
		t.Fatalf("401 retried: hits=%d", h.hits.Load())
	}
	h.assertNoSecrets(t, s)
}

func TestClaudeOtherHTTPError(t *testing.T) {
	h := newClaudeHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(claudeToken))
	})
	s := h.refresh(t)
	if s.Err != "usage api: http 429" {
		t.Fatalf("err %q", s.Err)
	}
	h.assertNoSecrets(t, s)
}

func TestClaudeCredentialsFileNeverModified(t *testing.T) {
	var n atomic.Int32
	h := newClaudeHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 2 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(claudeUsageBody))
	})
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(h.path, old, old); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(h.path)
	if err != nil {
		t.Fatal(err)
	}
	fi0, _ := os.Stat(h.path)
	h.refresh(t) // 200
	h.refresh(t) // 401
	h.clock.Advance(2 * time.Hour)
	h.refresh(t) // expired
	after, _ := os.ReadFile(h.path)
	fi1, _ := os.Stat(h.path)
	if !bytes.Equal(before, after) || !fi0.ModTime().Equal(fi1.ModTime()) || fi0.Mode() != fi1.Mode() {
		t.Fatal("credentials file modified")
	}
	entries, _ := os.ReadDir(filepath.Dir(h.path))
	if len(entries) != 1 {
		t.Fatalf("unexpected files in credentials dir: %v", entries)
	}
}

func TestClaudeCredentialsReloadedOnChange(t *testing.T) {
	var auth atomic.Value
	h := newClaudeHarness(t, func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		w.Write([]byte(claudeUsageBody))
	})
	h.refresh(t)
	body := fmt.Sprintf(`{"claudeAiOauth":{"accessToken":"new-token","expiresAt":%d,"subscriptionType":"pro"}}`, t0.Add(time.Hour).UnixMilli())
	if err := os.WriteFile(h.path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Minute)
	os.Chtimes(h.path, future, future)
	s := h.refresh(t)
	if auth.Load() != "Bearer new-token" || s.Plan != "pro" {
		t.Fatalf("credentials not reloaded: plan=%q", s.Plan)
	}
}

func TestClaudeMissingOrMalformedCredentials(t *testing.T) {
	h := newClaudeHarness(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("network hit")
	})
	h.path = filepath.Join(t.TempDir(), "secret-dir", "missing.json")
	s := h.refresh(t)
	if s.Err != "claude credentials: unavailable" || strings.Contains(s.Err, "secret-dir") {
		t.Fatalf("err %q", s.Err)
	}

	h.path = filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(h.path, []byte(`{"claudeAiOauth":{"accessToken":"`+claudeToken), 0o600)
	s = h.refresh(t)
	if s.Err != "claude credentials: malformed" {
		t.Fatalf("err %q", s.Err)
	}
	h.assertNoSecrets(t, s)

	m := New([]core.Account{{ID: "cl", Provider: core.ProviderClaude}}, nil, Options{Clock: h.clock, Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	m.refresh(context.Background(), "cl", true)
	if s, _ := m.Latest("cl"); s.Err != "claude credentials: unavailable" {
		t.Fatalf("nil func err %q", s.Err)
	}
}

func TestClaudeObserveHeadersNoop(t *testing.T) {
	h := newClaudeHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(claudeUsageBody))
	})
	before := h.refresh(t)
	hdr := http.Header{}
	hdr.Set("x-codex-primary-used-percent", "99")
	h.m.ObserveHeaders("cl", hdr)
	after, _ := h.m.Latest("cl")
	if !approx(findWindow(t, after, core.Window5h).UsedFrac, 0.27) || after.Source != before.Source {
		t.Fatalf("headers applied to claude account: %+v", after)
	}
}
