package app_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/auth"
	"github.com/hpst3r/localrouter/internal/config"
)

const (
	orInferenceKey = "sk-or-v1-INFERENCE-SECRET-1111"
	orMgmtKey      = "sk-or-v1-MANAGEMENT-SECRET-2222"
	orModel        = "anthropic/claude-sonnet-4.5"
	orChatSSE      = "data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		": OPENROUTER PROCESSING\n\n" +
		"data: {\"id\":\"gen-1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":42,\"completion_tokens\":8,\"total_tokens\":50,\"cost\":0.0003,\"prompt_tokens_details\":{\"cached_tokens\":6},\"completion_tokens_details\":{\"reasoning_tokens\":2}}}\n\n" +
		"data: [DONE]\n\n"
)

// fakeOpenRouter serves /api/v1/{credits,key,chat/completions}.
type fakeOpenRouter struct {
	srv *httptest.Server

	mu          sync.Mutex
	credits     string // /credits body
	requireMgmt bool   // /credits answers 403 to any key but the management key
	chatStatus  int    // 0 = stream orChatSSE
	auths       map[string][]string
	chatBodies  [][]byte
}

func newFakeOpenRouter(t *testing.T) *fakeOpenRouter {
	f := &fakeOpenRouter{credits: `{"data":{"total_credits":20,"total_usage":5}}`, auths: map[string][]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/credits", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auths["credits"] = append(f.auths["credits"], r.Header.Get("Authorization"))
		body, requireMgmt := f.credits, f.requireMgmt
		f.mu.Unlock()
		if requireMgmt && r.Header.Get("Authorization") != "Bearer "+orMgmtKey {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":403,"message":"Only management keys can perform this operation"}}`)
			return
		}
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("GET /api/v1/key", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auths["key"] = append(f.auths["key"], r.Header.Get("Authorization"))
		f.mu.Unlock()
		_, _ = io.WriteString(w, `{"data":{"label":"sk-or-v1-abc...","limit":null,"limit_remaining":null,"limit_reset":null,
			"include_byok_in_limit":false,"usage":770.89,"usage_daily":1.25,"usage_weekly":7.5,"usage_monthly":30.25,
			"byok_usage":0,"byok_usage_daily":0,"byok_usage_weekly":0,"byok_usage_monthly":0,"is_free_tier":false}}`)
	})
	mux.HandleFunc("POST /api/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.auths["chat"] = append(f.auths["chat"], r.Header.Get("Authorization"))
		f.chatBodies = append(f.chatBodies, b)
		st := f.chatStatus
		f.mu.Unlock()
		if st != 0 {
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(st)
			_, _ = io.WriteString(w, `{"error":{"code":402,"message":"Insufficient credits"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, orChatSSE)
	})
	// A generic OpenAI-compatible fallback account.
	mux.HandleFunc("POST /fallback/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auths["fallback"] = append(f.auths["fallback"], r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c","object":"chat.completion","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOpenRouter) seen(k string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.auths[k]...)
}

func (f *fakeOpenRouter) set(fn func(*fakeOpenRouter)) {
	f.mu.Lock()
	fn(f)
	f.mu.Unlock()
}

type orEnv struct {
	t      *testing.T
	f      *fakeOpenRouter
	app    *app.App
	srv    *httptest.Server
	logs   *lockedBuffer
	client string
}

// newOREnv builds the real app with one openrouter account ("or") and,
// optionally, a fallback openai_compat account ("fb") second in the route.
//
// An optional setup callback mutates the fake upstream BEFORE the app is built
// and started, so a test can seed the state the very first quota poll observes.
// Mutating the fake after Start races that poll: an urgent RequestRefresh
// issued while the initial fetch is in flight is coalesced into it (single
// flight), so the published snapshot keeps the pre-mutation state and a later
// wait for the mutated state times out.
func newOREnv(t *testing.T, withMgmt, withFallback bool, setup ...func(*fakeOpenRouter)) *orEnv {
	t.Helper()
	dir := t.TempDir()
	f := newFakeOpenRouter(t)
	if len(setup) > 0 && setup[0] != nil {
		setup[0](f)
	}
	e := &orEnv{t: t, f: f, logs: &lockedBuffer{}}
	var err error
	if e.client, err = auth.GenerateKey(); err != nil {
		t.Fatal(err)
	}
	writeSecret(t, filepath.Join(dir, "client.key"), e.client)
	writeSecret(t, filepath.Join(dir, "or.key"), orInferenceKey)
	writeSecret(t, filepath.Join(dir, "or-mgmt.key"), orMgmtKey)
	writeSecret(t, filepath.Join(dir, "fb.key"), "fallback-SECRET-3333")
	mgmt, accts := "", "[or]"
	if withMgmt {
		mgmt = "    management_key_file: or-mgmt.key\n"
	}
	fb := ""
	if withFallback {
		fb = fmt.Sprintf("  - {id: fb, provider: openai_compat, base_url: %s/fallback/v1, api_key_file: fb.key}\n", f.srv.URL)
		accts = "[or, fb]"
	}
	yaml := fmt.Sprintf(`data_dir: data
clients:
  - {name: ide, class: interactive, key_file: client.key}
accounts:
  - id: or
    provider: openrouter
    base_url: %s/api/v1
    api_key_file: or.key
%s%sroutes:
  - name: openrouter
    models: ["%s"]
    interactive: %s
`, f.srv.URL, mgmt, fb, orModel, accts)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.app, err = app.Build(cfg, logger, app.Overrides{HTTPClient: f.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.app.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	e.app.Start(ctx)
	e.srv = httptest.NewServer(e.app.Handler)
	t.Cleanup(e.srv.Close)
	return e
}

type orAccountStatus struct {
	ID                    string     `json:"id"`
	Healthy               bool       `json:"healthy"`
	InteractiveAdmissible bool       `json:"interactive_admissible"`
	CooldownUntil         *time.Time `json:"cooldown_until"`
	Reason                string     `json:"reason"`
	Windows               []any      `json:"windows"`
	Credits               *struct {
		Available  bool     `json:"available"`
		BalanceUSD *float64 `json:"balance_usd"`
		Exhausted  bool     `json:"exhausted"`
		Error      *string  `json:"error"`
	} `json:"credits"`
	Key *struct {
		Available     bool     `json:"available"`
		Unlimited     bool     `json:"unlimited"`
		UsageDailyUSD *float64 `json:"usage_daily_usd"`
	} `json:"key"`
}

func (e *orEnv) status(id string) orAccountStatus {
	e.t.Helper()
	resp, err := http.Get(e.srv.URL + "/control/v1/status")
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc struct {
		Accounts []orAccountStatus `json:"accounts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		e.t.Fatal(err)
	}
	for _, a := range doc.Accounts {
		if a.ID == id {
			return a
		}
	}
	e.t.Fatalf("account %s not in status", id)
	return orAccountStatus{}
}

// waitStatus polls status until cond holds.
func (e *orEnv) waitStatus(id string, cond func(orAccountStatus) bool) orAccountStatus {
	e.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		st := e.status(id)
		if cond(st) {
			return st
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out; last status %+v credits=%+v key=%+v", st, st.Credits, st.Key)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *orEnv) chat(stream bool) (int, string) {
	e.t.Helper()
	body := fmt.Sprintf(`{"model":%q,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, orModel, stream)
	req, _ := http.NewRequest(http.MethodPost, e.srv.URL+"/v1/chat/completions", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+e.client)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (e *orEnv) usage(t *testing.T) map[string]any {
	t.Helper()
	resp, err := http.Get(e.srv.URL + "/control/v1/usage?group=account")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc struct {
		Rows []map[string]any `json:"rows"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	for _, r := range doc.Rows {
		if r["key"] == "or" {
			return r
		}
	}
	return nil
}

func TestE2EOpenRouterStreamBalanceAndExhaustion(t *testing.T) {
	e := newOREnv(t, true, false)
	st := e.waitStatus("or", func(s orAccountStatus) bool {
		return s.Credits != nil && s.Credits.Available && s.Key != nil && s.Key.Available
	})
	if math.Abs(*st.Credits.BalanceUSD-15) > 1e-9 || !st.InteractiveAdmissible || !st.Healthy || !st.Key.Unlimited || len(st.Windows) != 0 {
		t.Fatalf("initial status %+v credits=%+v", st, st.Credits)
	}

	code, body := e.chat(true)
	if code != 200 || !strings.Contains(body, "[DONE]") || !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("stream %d %s", code, body)
	}
	e.f.mu.Lock()
	sent := string(e.f.chatBodies[0])
	e.f.mu.Unlock()
	if !strings.Contains(sent, `"model":"`+orModel+`"`) || !strings.Contains(sent, `"include_usage":true`) {
		t.Errorf("upstream body %s", sent)
	}
	deadline := time.Now().Add(5 * time.Second)
	var row map[string]any
	for row == nil || row["input_tokens"] != float64(42) {
		if time.Now().After(deadline) {
			t.Fatalf("ledger row %v", row)
		}
		time.Sleep(10 * time.Millisecond)
		row = e.usage(t)
	}
	if row["output_tokens"] != float64(8) || row["cached_input_tokens"] != float64(6) || row["reasoning_tokens"] != float64(2) || row["requests"] != float64(1) {
		t.Errorf("ledger usage %v", row)
	}

	// The provider now reports the user's real overdrawn account.
	e.f.set(func(f *fakeOpenRouter) {
		f.credits = `{"data":{"total_credits":770.8176,"total_usage":770.893717902}}`
	})
	e.app.Quota.RequestRefresh("or", true)
	st = e.waitStatus("or", func(s orAccountStatus) bool {
		return s.Credits.BalanceUSD != nil && *s.Credits.BalanceUSD < 0
	})
	if math.Abs(*st.Credits.BalanceUSD-(-0.076117902)) > 1e-9 || !st.Credits.Exhausted || st.InteractiveAdmissible ||
		!strings.Contains(st.Reason, "-$0.08") {
		t.Fatalf("exhausted status %+v credits=%+v", st, st.Credits)
	}
	chats := len(e.f.seen("chat"))
	code, body = e.chat(false)
	if code != http.StatusTooManyRequests || !strings.Contains(body, "account balance -$0.08") {
		t.Fatalf("exhausted request: %d %s", code, body)
	}
	if len(e.f.seen("chat")) != chats {
		t.Error("exhausted account was sent a request")
	}

	// Credential isolation: management key only on /credits, inference key
	// on /key and inference, nothing leaks into logs.
	for _, a := range e.f.seen("credits") {
		if a != "Bearer "+orMgmtKey {
			t.Errorf("/credits auth %q", a)
		}
	}
	for _, k := range []string{"key", "chat"} {
		for _, a := range e.f.seen(k) {
			if a != "Bearer "+orInferenceKey {
				t.Errorf("/%s auth %q", k, a)
			}
		}
	}
	if logs := e.logs.String(); strings.Contains(logs, orInferenceKey) || strings.Contains(logs, orMgmtKey) {
		t.Error("key in logs")
	}
}

// Without a management key a 403 on /credits leaves the balance unavailable
// (not zero), /key data still shows, and inference still works.
func TestE2EOpenRouterCreditsForbiddenWithoutManagementKey(t *testing.T) {
	// Seed requireMgmt=true BEFORE Start so the app's first quota poll observes
	// it directly. Setting it after Start and then calling RequestRefresh(urgent)
	// is nondeterministic: if the initial /credits fetch is already in flight the
	// urgent request is coalesced into it, the pre-mutation "credits available"
	// snapshot is published, and the wait below can never be satisfied.
	e := newOREnv(t, false, false, func(f *fakeOpenRouter) { f.requireMgmt = true })
	st := e.waitStatus("or", func(s orAccountStatus) bool {
		return s.Credits != nil && s.Credits.Error != nil && s.Key != nil && s.Key.Available
	})
	if st.Credits.Available || st.Credits.BalanceUSD != nil || st.Healthy || !strings.Contains(*st.Credits.Error, "management key required") {
		t.Fatalf("status %+v credits=%+v", st, st.Credits)
	}
	if st.Key.UsageDailyUSD == nil || *st.Key.UsageDailyUSD != 1.25 || !st.InteractiveAdmissible {
		t.Fatalf("key-only data must stay usable: %+v %+v", st, st.Key)
	}
	if code, body := e.chat(true); code != 200 || !strings.Contains(body, "[DONE]") {
		t.Fatalf("inference %d %s", code, body)
	}
	for _, a := range e.f.seen("credits") {
		if a != "Bearer "+orInferenceKey {
			t.Errorf("/credits auth %q", a)
		}
	}
}

// A 402 from OpenRouter without proof of funds fails over to the next
// account, cools OpenRouter down and triggers an urgent quota refresh. (With
// a fresh positive balance the 402 is request-scoped and relayed instead.)
func TestE2EOpenRouter402Failover(t *testing.T) {
	e := newOREnv(t, false, true)
	e.f.set(func(f *fakeOpenRouter) { f.requireMgmt = true })
	e.app.Quota.RequestRefresh("or", true)
	e.waitStatus("or", func(s orAccountStatus) bool { return s.Credits != nil && s.Credits.Error != nil })
	creditsBefore := len(e.f.seen("credits"))
	e.f.set(func(f *fakeOpenRouter) { f.chatStatus = http.StatusPaymentRequired })

	code, body := e.chat(false)
	if code != 200 || !strings.Contains(body, `"chat.completion"`) {
		t.Fatalf("failover %d %s", code, body)
	}
	if len(e.f.seen("chat")) != 1 || len(e.f.seen("fallback")) != 1 {
		t.Errorf("chat=%d fallback=%d", len(e.f.seen("chat")), len(e.f.seen("fallback")))
	}
	st := e.waitStatus("or", func(s orAccountStatus) bool { return s.CooldownUntil != nil })
	if st.InteractiveAdmissible {
		t.Errorf("or should be cooling down: %+v", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(e.f.seen("credits")) <= creditsBefore {
		if time.Now().After(deadline) {
			t.Fatal("402 did not trigger a credits refresh")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Next request goes straight to the fallback.
	if code, _ := e.chat(false); code != 200 || len(e.f.seen("chat")) != 1 || len(e.f.seen("fallback")) != 2 {
		t.Errorf("code %d chat=%d fallback=%d", code, len(e.f.seen("chat")), len(e.f.seen("fallback")))
	}
}
