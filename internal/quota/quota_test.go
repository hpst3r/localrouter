package quota

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const secretToken = "sk-SECRET-token-abc123"

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type fakeCreds struct {
	mu          sync.Mutex
	gen         int
	invalidated int
	err         error
}

func (f *fakeCreds) Credential(ctx context.Context, id string) (core.Credential, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return core.Credential{}, f.err
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+secretToken+"-"+string(rune('0'+f.gen)))
	h.Set("ChatGPT-Account-Id", "acct-"+id)
	return core.Credential{Headers: h, Identity: "acct-" + id}, nil
}

func (f *fakeCreds) Invalidate(id string) {
	f.mu.Lock()
	f.invalidated++
	f.gen++
	f.mu.Unlock()
}

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type harness struct {
	m     *Manager
	clock *fakeClock
	creds *fakeCreds
	hits  atomic.Int32
	logs  *bytes.Buffer
}

func newHarness(t *testing.T, accounts []core.Account, handler http.HandlerFunc) *harness {
	t.Helper()
	h := &harness{clock: &fakeClock{t: t0}, creds: &fakeCreds{}, logs: &bytes.Buffer{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	h.m = New(accounts, h.creds, Options{
		Clock:          h.clock,
		HTTPClient:     srv.Client(),
		Logger:         slog.New(slog.NewTextHandler(h.logs, nil)),
		CodexUsageURL:  srv.URL + "/backend-api/wham/usage",
		OllamaUsageURL: srv.URL + "/api/usage",
		PollInterval:   time.Hour,
	})
	return h
}

func codexAcct() []core.Account {
	return []core.Account{{ID: "cx", Provider: core.ProviderCodex, BaseURL: "https://chatgpt.com/backend-api/codex"}}
}

func ollamaAcct() []core.Account {
	return []core.Account{{ID: "ol", Provider: core.ProviderOllama, BaseURL: "https://ollama.com/v1"}}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func findWindow(t *testing.T, s core.Snapshot, kind string) core.Window {
	t.Helper()
	for _, w := range s.Windows {
		if w.Kind == kind {
			return w
		}
	}
	t.Fatalf("window %q missing in %+v", kind, s.Windows)
	return core.Window{}
}

// Acceptance test 8.
func TestOllamaFractions(t *testing.T) {
	h := newHarness(t, ollamaAcct(), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/usage" {
			t.Errorf("path %q", r.URL.Path)
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("missing auth")
		}
		w.Write([]byte(`{"limits":{"session":{"usage":0.484},"weekly":{"usage":1.7}}}`))
	})
	h.m.refresh(context.Background(), "ol", true)
	s, ok := h.m.Latest("ol")
	if !ok {
		t.Fatal("no snapshot")
	}
	if w := findWindow(t, s, core.Window5h); !approx(w.UsedFrac, 0.484) || w.WindowSeconds != 18000 || !w.ResetAt.IsZero() {
		t.Errorf("5h = %+v", w)
	}
	if w := findWindow(t, s, core.WindowWeekly); w.UsedFrac != 1 || w.WindowSeconds != 604800 {
		t.Errorf("weekly not clamped: %+v", w)
	}
	if s.Source != SourceUsageAPI || !s.FetchedAt.Equal(t0) || s.Err != "" {
		t.Errorf("snap = %+v", s)
	}
}

func TestOllamaMissingWindowOmitted(t *testing.T) {
	h := newHarness(t, ollamaAcct(), func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"limits":{"session":{"usage":0.2}}}`))
	})
	h.m.refresh(context.Background(), "ol", true)
	s, _ := h.m.Latest("ol")
	if len(s.Windows) != 1 || s.Windows[0].Kind != core.Window5h {
		t.Errorf("windows = %+v", s.Windows)
	}
}

func TestCodexMapping(t *testing.T) {
	resetAt := t0.Add(3 * time.Hour).Unix()
	h := newHarness(t, codexAcct(), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/backend-api/wham/usage" {
			t.Errorf("path %q", r.URL.Path)
		}
		if r.Header.Get("User-Agent") != "codex-cli" || r.Header.Get("Accept") == "" {
			t.Errorf("headers %v", r.Header)
		}
		if r.Header.Get("ChatGPT-Account-Id") != "acct-cx" || r.Header.Get("Authorization") == "" {
			t.Errorf("cred headers missing")
		}
		w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false,
			"primary_window":{"used_percent":91,"limit_window_seconds":18000,"reset_after_seconds":999,"reset_at":` +
			strconv.FormatInt(resetAt, 10) + `},
			"secondary_window":{"used_percent":54.5,"limit_window_seconds":604800,"reset_after_seconds":3600}}}`))
	})
	h.m.refresh(context.Background(), "cx", true)
	s, ok := h.m.Latest("cx")
	if !ok {
		t.Fatal("no snapshot")
	}
	p := findWindow(t, s, core.Window5h)
	if !approx(p.UsedFrac, 0.91) || p.WindowSeconds != 18000 || p.ResetAt.Unix() != resetAt {
		t.Errorf("primary = %+v (reset_at should win)", p)
	}
	sec := findWindow(t, s, core.WindowWeekly)
	if !approx(sec.UsedFrac, 0.545) || sec.WindowSeconds != 604800 || !sec.ResetAt.Equal(t0.Add(time.Hour)) {
		t.Errorf("secondary = %+v (reset_after relative to fetch time)", sec)
	}
	if s.Plan != "pro" || s.Allowed == nil || !*s.Allowed {
		t.Errorf("plan/allowed = %q %v", s.Plan, s.Allowed)
	}
}

func TestCodexLimitReached(t *testing.T) {
	h := newHarness(t, codexAcct(), func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"rate_limit":{"allowed":true,"limit_reached":true,"primary_window":{"used_percent":100}}}`))
	})
	h.m.refresh(context.Background(), "cx", true)
	s, _ := h.m.Latest("cx")
	if s.Allowed == nil || *s.Allowed {
		t.Errorf("allowed = %v, want false", s.Allowed)
	}
	if len(s.Windows) != 1 {
		t.Errorf("secondary should be omitted: %+v", s.Windows)
	}
}

func Test401RetryWithInvalidate(t *testing.T) {
	var calls atomic.Int32
	h := newHarness(t, codexAcct(), func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":10}}}`))
	})
	h.m.refresh(context.Background(), "cx", true)
	if h.creds.invalidated != 1 || h.hits.Load() != 2 {
		t.Fatalf("invalidated=%d hits=%d", h.creds.invalidated, h.hits.Load())
	}
	s, _ := h.m.Latest("cx")
	if s.Err != "" || len(s.Windows) != 1 {
		t.Errorf("snap = %+v", s)
	}
}

func Test401TwiceGivesUp(t *testing.T) {
	h := newHarness(t, codexAcct(), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	h.m.refresh(context.Background(), "cx", true)
	if h.creds.invalidated != 1 || h.hits.Load() != 2 {
		t.Fatalf("invalidated=%d hits=%d", h.creds.invalidated, h.hits.Load())
	}
	s, ok := h.m.Latest("cx")
	if !ok || s.Err != "usage api: http 401" || !s.FetchedAt.IsZero() || len(s.Windows) != 0 {
		t.Errorf("snap = %+v", s)
	}
}

func TestErrorKeepsLastGoodAndNoSecrets(t *testing.T) {
	var fail atomic.Bool
	h := newHarness(t, codexAcct(), func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			// Echo the credential back, as a hostile/buggy upstream might.
			w.Write([]byte("error for " + r.Header.Get("Authorization")))
			return
		}
		w.Write([]byte(`{"plan_type":"plus","rate_limit":{"allowed":true,"primary_window":{"used_percent":40}}}`))
	})
	h.m.refresh(context.Background(), "cx", true)
	fail.Store(true)
	h.clock.Advance(time.Minute)
	h.m.refresh(context.Background(), "cx", true)
	s, _ := h.m.Latest("cx")
	if s.Err != "usage api: http 500" {
		t.Errorf("err = %q", s.Err)
	}
	if !s.FetchedAt.Equal(t0) || len(s.Windows) != 1 || !approx(s.Windows[0].UsedFrac, 0.4) || s.Plan != "plus" {
		t.Errorf("last-good not kept: %+v", s)
	}
	// Malformed body and credential errors are also sanitized.
	fail.Store(false)
	h.creds.mu.Lock()
	h.creds.err = errors.New("refresh failed for " + secretToken)
	h.creds.mu.Unlock()
	h.m.refresh(context.Background(), "cx", true)
	s, _ = h.m.Latest("cx")
	if s.Err != "usage api: credential unavailable" {
		t.Errorf("err = %q", s.Err)
	}
	if strings.Contains(s.Err, secretToken) || strings.Contains(h.logs.String(), secretToken) {
		t.Errorf("secret leaked: err=%q logs=%q", s.Err, h.logs.String())
	}
	// A successful fetch clears Err.
	h.creds.mu.Lock()
	h.creds.err = nil
	h.creds.mu.Unlock()
	h.m.refresh(context.Background(), "cx", true)
	if s, _ := h.m.Latest("cx"); s.Err != "" {
		t.Errorf("err not cleared: %q", s.Err)
	}
}

func TestMalformed(t *testing.T) {
	h := newHarness(t, ollamaAcct(), func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json ` + secretToken))
	})
	h.m.refresh(context.Background(), "ol", true)
	s, _ := h.m.Latest("ol")
	if s.Err != "usage api: malformed response" {
		t.Errorf("err = %q", s.Err)
	}
}

func TestOpenAICompatHasNoSource(t *testing.T) {
	h := newHarness(t, []core.Account{{ID: "oc", Provider: core.ProviderOpenAICompat}}, func(w http.ResponseWriter, r *http.Request) {})
	h.m.RequestRefresh("oc", true)
	h.m.ObserveHeaders("oc", http.Header{"X-Codex-Primary-Used-Percent": {"50"}})
	ctx, cancel := context.WithCancel(context.Background())
	h.m.Start(ctx)
	cancel()
	h.m.wait()
	if _, ok := h.m.Latest("oc"); ok {
		t.Error("openai_compat should have no snapshot")
	}
	if h.hits.Load() != 0 {
		t.Errorf("hits = %d", h.hits.Load())
	}
}

func TestLatestIsDeepCopy(t *testing.T) {
	h := newHarness(t, codexAcct(), func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":40}}}`))
	})
	h.m.refresh(context.Background(), "cx", true)
	s, _ := h.m.Latest("cx")
	s.Windows[0].UsedFrac = 0.99
	*s.Allowed = false
	s2, _ := h.m.Latest("cx")
	if !approx(s2.Windows[0].UsedFrac, 0.4) || !*s2.Allowed {
		t.Errorf("internal state mutated: %+v", s2)
	}
}

func TestDebounce(t *testing.T) {
	h := newHarness(t, codexAcct(), func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":1}}}`))
	})
	h.m.RequestRefresh("cx", false) // first ever: fetches
	h.m.wait()
	h.m.RequestRefresh("cx", false) // within gap: dropped
	h.m.wait()
	h.clock.Advance(10 * time.Second)
	h.m.RequestRefresh("cx", false) // still within gap
	h.m.wait()
	if n := h.hits.Load(); n != 1 {
		t.Fatalf("hits = %d, want 1", n)
	}
	h.m.RequestRefresh("cx", true) // urgent ignores gap
	h.m.wait()
	if n := h.hits.Load(); n != 2 {
		t.Fatalf("hits = %d, want 2", n)
	}
	h.clock.Advance(21 * time.Second)
	h.m.RequestRefresh("cx", false)
	h.m.wait()
	if n := h.hits.Load(); n != 3 {
		t.Fatalf("hits = %d, want 3", n)
	}
}

func TestUrgentCoalesces(t *testing.T) {
	entered := make(chan struct{}, 10)
	release := make(chan struct{})
	h := newHarness(t, codexAcct(), func(w http.ResponseWriter, r *http.Request) {
		entered <- struct{}{}
		<-release
		w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":1}}}`))
	})
	h.m.RequestRefresh("cx", true)
	<-entered
	for range 5 {
		h.m.RequestRefresh("cx", true)
	}
	// Give the coalesced goroutines a chance to (wrongly) start fetches.
	time.Sleep(50 * time.Millisecond)
	close(release)
	h.m.wait()
	if n := h.hits.Load(); n != 1 {
		t.Fatalf("hits = %d, want 1", n)
	}
}

func TestStartPollsImmediately(t *testing.T) {
	h := newHarness(t, append(codexAcct(), ollamaAcct()...), func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/api/usage") {
			w.Write([]byte(`{"limits":{"session":{"usage":0.1}}}`))
			return
		}
		w.Write([]byte(`{"rate_limit":{"primary_window":{"used_percent":1}}}`))
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.m.Start(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, ok1 := h.m.Latest("cx")
		_, ok2 := h.m.Latest("ol")
		if ok1 && ok2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("poller did not fetch")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestObserveHeadersMerge(t *testing.T) {
	h := newHarness(t, codexAcct(), func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"plan_type":"plus","rate_limit":{"allowed":false,
			"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_after_seconds":100},
			"secondary_window":{"used_percent":30,"limit_window_seconds":604800,"reset_after_seconds":5000}}}`))
	})
	h.m.refresh(context.Background(), "cx", true)
	h.clock.Advance(time.Minute)
	now := h.clock.Now()

	// No used-percent header: ignored.
	h.m.ObserveHeaders("cx", http.Header{"X-Codex-Primary-Reset-After-Seconds": {"5"}})
	if s, _ := h.m.Latest("cx"); s.Source != SourceUsageAPI {
		t.Fatalf("snapshot changed without used-percent: %+v", s)
	}

	hdr := http.Header{}
	hdr.Set("x-codex-primary-used-percent", "12.5")
	hdr.Set("x-codex-primary-reset-after-seconds", "600")
	hdr.Set("x-codex-primary-window-minutes", "300")
	h.m.ObserveHeaders("cx", hdr)
	s, _ := h.m.Latest("cx")
	p := findWindow(t, s, core.Window5h)
	if !approx(p.UsedFrac, 0.125) || !p.ResetAt.Equal(now.Add(600*time.Second)) || p.WindowSeconds != 18000 {
		t.Errorf("primary = %+v", p)
	}
	sec := findWindow(t, s, core.WindowWeekly)
	if !approx(sec.UsedFrac, 0.3) || !sec.ResetAt.Equal(t0.Add(5000*time.Second)) {
		t.Errorf("secondary should be kept: %+v", sec)
	}
	if s.Source != SourceHeaders || !s.FetchedAt.Equal(now) || s.Plan != "plus" {
		t.Errorf("snap = %+v", s)
	}
	if s.Allowed == nil || !*s.Allowed {
		t.Errorf("allowed should flip to true when all observed windows < 100%%")
	}

	// Exhausted window flips Allowed to false.
	hdr = http.Header{}
	hdr.Set("x-codex-secondary-used-percent", "100")
	h.m.ObserveHeaders("cx", hdr)
	s, _ = h.m.Latest("cx")
	if s.Allowed == nil || *s.Allowed {
		t.Errorf("allowed = %v, want false", s.Allowed)
	}
	if w := findWindow(t, s, core.WindowWeekly); w.UsedFrac != 1 || !w.ResetAt.Equal(t0.Add(5000*time.Second)) {
		t.Errorf("weekly = %+v", w)
	}
}

func TestObserveHeadersWithoutPriorSnapshot(t *testing.T) {
	h := newHarness(t, codexAcct(), func(w http.ResponseWriter, r *http.Request) {})
	hdr := http.Header{}
	hdr.Set("x-codex-secondary-used-percent", "20")
	h.m.ObserveHeaders("cx", hdr)
	s, ok := h.m.Latest("cx")
	if !ok || len(s.Windows) != 1 || s.Allowed != nil {
		t.Fatalf("snap = %+v", s)
	}
	if w := s.Windows[0]; w.Kind != core.WindowWeekly || !approx(w.UsedFrac, 0.2) || w.WindowSeconds != 604800 || !w.ResetAt.IsZero() {
		t.Errorf("window = %+v", w)
	}
}

func TestObserveHeadersIgnoredForOllama(t *testing.T) {
	h := newHarness(t, ollamaAcct(), func(w http.ResponseWriter, r *http.Request) {})
	h.m.ObserveHeaders("ol", http.Header{"X-Codex-Primary-Used-Percent": {"50"}})
	if _, ok := h.m.Latest("ol"); ok {
		t.Error("ollama snapshot created from codex headers")
	}
}
