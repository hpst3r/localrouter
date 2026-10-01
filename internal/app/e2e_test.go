package app_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
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

	_ "modernc.org/sqlite"

	"github.com/hpst3r/localrouter/internal/app"
	"github.com/hpst3r/localrouter/internal/auth"
	"github.com/hpst3r/localrouter/internal/config"
	"github.com/hpst3r/localrouter/internal/core"
)

// Account and identity names used throughout the scenarios.
const (
	primary     = "codex-primary"
	secondary   = "codex-secondary"
	ollamaAcct  = "ollama-main"
	primaryGPT  = "chatgpt-acct-primary"
	secondGPT   = "chatgpt-acct-secondary"
	codexModel  = "gpt-5-codex"
	pricedModel = "test-model-priced"
	unpriced    = "test-model-unpriced"
	ollamaModel = "gpt-oss:120b"
)

// codexSSE is the exact byte stream the fake Codex upstream returns.
const codexSSE = "event: response.created\r\n" +
	"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\r\n\r\n" +
	"event: response.output_text.delta\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n" +
	"event: response.completed\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\n" +
	"data: \"usage\":{\"input_tokens\":1200,\"input_tokens_details\":{\"cached_tokens\":200},\"output_tokens\":300,\"output_tokens_details\":{\"reasoning_tokens\":120},\"total_tokens\":1500}}}\n\n"

const ollamaChatSSE = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":7,\"total_tokens\":57,\"prompt_tokens_details\":{\"cached_tokens\":10},\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\n" +
	"data: [DONE]\n\n"

// codexQuota is what the fake wham/usage endpoint reports for one account.
type codexQuota struct {
	Used5h, UsedWeekly float64 // percent
	Allowed            bool
}

// upstreamHit is one request observed by a fake upstream.
type upstreamHit struct {
	Path   string
	Header http.Header
	Body   []byte
}

// fakes is a single httptest server standing in for every external endpoint.
type fakes struct {
	srv *httptest.Server

	mu            sync.Mutex
	quota         map[string]codexQuota // by chatgpt account id
	ollamaQuota   [2]float64
	codexHits     map[string][]upstreamHit // by account id (from path)
	ollamaHits    []upstreamHit
	issuerHits    int
	issuerRefresh []string // refresh tokens presented
	rotated       map[string]string
	primary429    bool // primary /responses answers 429 and then reports exhaustion
	nonce         int
}

func newFakes(t *testing.T) *fakes {
	f := &fakes{
		quota:       map[string]codexQuota{},
		codexHits:   map[string][]upstreamHit{},
		rotated:     map[string]string{},
		ollamaQuota: [2]float64{0.2, 0.1},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /auth/oauth/token", f.issuer)
	mux.HandleFunc("GET /wham/usage", f.whamUsage)
	mux.HandleFunc("GET /ollama/api/usage", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		q := f.ollamaQuota
		f.mu.Unlock()
		writeJSONResp(w, map[string]any{"limits": map[string]any{
			"session": map[string]any{"usage": q[0]},
			"weekly":  map[string]any{"usage": q[1]},
		}})
	})
	mux.HandleFunc("POST /codex/{acct}/responses", f.codexResponses)
	mux.HandleFunc("POST /ollama/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		f.recordOllama(r)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, ollamaChatSSE)
	})
	mux.HandleFunc("POST /ollama/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		f.recordOllama(r)
		writeJSONResp(w, map[string]any{"id": "resp_o", "object": "response",
			"usage": map[string]any{"input_tokens": 11, "output_tokens": 4}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func writeJSONResp(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakes) issuer(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	time.Sleep(50 * time.Millisecond) // widen the window for concurrent refreshers
	f.mu.Lock()
	f.issuerHits++
	f.nonce++
	n := f.nonce
	rt := r.PostForm.Get("refresh_token")
	f.issuerRefresh = append(f.issuerRefresh, rt)
	f.mu.Unlock()
	if r.PostForm.Get("grant_type") != "refresh_token" || r.PostForm.Get("client_id") != auth.ClientID {
		http.Error(w, `{"error":"invalid_request"}`, http.StatusBadRequest)
		return
	}
	newRT := fmt.Sprintf("rt-rotated-%d-SECRETREFRESH", n)
	f.mu.Lock()
	f.rotated[rt] = newRT
	f.mu.Unlock()
	writeJSONResp(w, map[string]any{
		"access_token":  makeJWT(primaryGPT, time.Now().Add(time.Hour), fmt.Sprintf("refreshed-%d", n)),
		"refresh_token": newRT,
		"expires_in":    3600,
	})
}

func (f *fakes) whamUsage(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	q, ok := f.quota[r.Header.Get("ChatGPT-Account-Id")]
	f.mu.Unlock()
	if !ok || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		http.Error(w, "unknown", http.StatusUnauthorized)
		return
	}
	writeJSONResp(w, map[string]any{
		"plan_type": "pro",
		"rate_limit": map[string]any{
			"allowed":       q.Allowed,
			"limit_reached": !q.Allowed,
			"primary_window": map[string]any{
				"used_percent": q.Used5h, "limit_window_seconds": 18000, "reset_after_seconds": 3600,
			},
			"secondary_window": map[string]any{
				"used_percent": q.UsedWeekly, "limit_window_seconds": 604800, "reset_after_seconds": 86400,
			},
		},
	})
}

func (f *fakes) codexResponses(w http.ResponseWriter, r *http.Request) {
	acct := r.PathValue("acct")
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.codexHits[acct] = append(f.codexHits[acct], upstreamHit{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	fail := acct == primary && f.primary429
	if fail {
		f.quota[primaryGPT] = codexQuota{Used5h: 100, UsedWeekly: 40, Allowed: false}
	}
	f.mu.Unlock()
	if fail {
		w.Header().Set("Retry-After", "120")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fl := w.(http.Flusher)
	// Write in pieces to exercise incremental relaying.
	for _, part := range strings.SplitAfter(codexSSE, "\n\n") {
		_, _ = io.WriteString(w, part)
		fl.Flush()
	}
}

func (f *fakes) recordOllama(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.ollamaHits = append(f.ollamaHits, upstreamHit{Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
	f.mu.Unlock()
}

func (f *fakes) setQuota(gptID string, q codexQuota) {
	f.mu.Lock()
	f.quota[gptID] = q
	f.mu.Unlock()
}

func (f *fakes) hits(acct string) []upstreamHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]upstreamHit(nil), f.codexHits[acct]...)
}

// makeJWT builds an unsigned JWT carrying exp and the ChatGPT account claim.
func makeJWT(gptID string, exp time.Time, jti string) string {
	enc := base64.RawURLEncoding
	h := enc.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	p, _ := json.Marshal(map[string]any{
		"exp":                         exp.Unix(),
		"jti":                         jti + "-SECRETACCESS",
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": gptID},
	})
	return h + "." + enc.EncodeToString(p) + ".c2ln"
}

// lockedBuffer is a concurrency-safe log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// env is a fully wired LocalRouter with fake upstreams.
type env struct {
	t       *testing.T
	fakes   *fakes
	app     *app.App
	srv     *httptest.Server
	cfg     *config.Config
	logs    *lockedBuffer
	dataDir string

	interactiveKey, backgroundKey, ollamaKey string
	access, refresh                          map[string]string // by account id
}

type envOpts struct {
	quota         map[string]codexQuota // by chatgpt id
	pricing       string                // pricing.yaml content; empty = no file
	expiredAccess bool                  // primary access token already expired
	noStart       bool                  // do not start quota polling
}

func writeSecret(t *testing.T, path, v string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(v+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newEnv(t *testing.T, o envOpts) *env {
	t.Helper()
	dir := t.TempDir()
	f := newFakes(t)
	for id, q := range o.quota {
		f.setQuota(id, q)
	}
	e := &env{t: t, fakes: f, logs: &lockedBuffer{}, dataDir: filepath.Join(dir, "data"),
		access: map[string]string{}, refresh: map[string]string{}}

	var err error
	if e.interactiveKey, err = auth.GenerateKey(); err != nil {
		t.Fatal(err)
	}
	if e.backgroundKey, err = auth.GenerateKey(); err != nil {
		t.Fatal(err)
	}
	e.ollamaKey = "ollama-SECRETAPIKEY-0123456789"
	writeSecret(t, filepath.Join(dir, "interactive.key"), e.interactiveKey)
	writeSecret(t, filepath.Join(dir, "background.key"), e.backgroundKey)
	writeSecret(t, filepath.Join(dir, "ollama.key"), e.ollamaKey)
	if o.pricing != "" {
		if err := os.WriteFile(filepath.Join(dir, "pricing.yaml"), []byte(o.pricing), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	yaml := fmt.Sprintf(`data_dir: data
pricing_file: pricing.yaml
clients:
  - {name: ide, class: interactive, key_file: interactive.key}
  - {name: batch, class: background, key_file: background.key}
accounts:
  - id: %[2]s
    provider: codex
    base_url: %[1]s/codex/%[2]s
    reserve: {"5h": 0.10}
  - id: %[3]s
    provider: codex
    base_url: %[1]s/codex/%[3]s
  - id: %[4]s
    provider: ollama
    base_url: %[1]s/ollama/v1/
    api_key_file: ollama.key
routes:
  - name: codex
    models: [%[5]s, %[6]s, %[7]s]
    interactive: [%[2]s, %[3]s]
    background: [%[2]s, %[3]s]
  - name: ollama
    models: ["%[8]s"]
    interactive: [%[4]s]
`, f.srv.URL, primary, secondary, ollamaAcct, codexModel, pricedModel, unpriced, ollamaModel)
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	if e.cfg, err = config.Load(cfgPath); err != nil {
		t.Fatal(err)
	}

	store, err := auth.NewStore(filepath.Join(e.dataDir, "tokens"))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct{ id, gpt string }{{primary, primaryGPT}, {secondary, secondGPT}} {
		exp := time.Now().Add(time.Hour)
		if a.id == primary && o.expiredAccess {
			exp = time.Now().Add(-time.Minute)
		}
		e.access[a.id] = makeJWT(a.gpt, exp, "seed-"+a.id)
		e.refresh[a.id] = "rt-seed-" + a.id + "-SECRETREFRESH"
		if err := store.Save(a.id, auth.Token{AccessToken: e.access[a.id], RefreshToken: e.refresh[a.id]}); err != nil {
			t.Fatal(err)
		}
	}

	logger := slog.New(slog.NewTextHandler(e.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.app, err = app.Build(e.cfg, logger, app.Overrides{
		Issuer:         f.srv.URL + "/auth",
		CodexUsageURL:  f.srv.URL + "/wham/usage",
		OllamaUsageURL: f.srv.URL + "/ollama/api/usage",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.srv = httptest.NewServer(e.app.Handler)
	t.Cleanup(func() {
		e.srv.Close()
		cancel()
		_ = e.app.Close()
	})
	if !o.noStart {
		e.app.Start(ctx)
		e.waitSnapshots(primary, secondary, ollamaAcct)
	}
	return e
}

// waitSnapshots polls Quota.Latest until each account has a successful fetch.
func (e *env) waitSnapshots(ids ...string) {
	e.t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for _, id := range ids {
		for {
			s, ok := e.app.Quota.Latest(id)
			if ok && !s.FetchedAt.IsZero() && s.Err == "" {
				break
			}
			if time.Now().After(deadline) {
				e.t.Fatalf("no quota snapshot for %s (ok=%v err=%q)", id, ok, s.Err)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

type result struct {
	status int
	header http.Header
	body   []byte
}

func (e *env) do(method, path, key string, body any, hdr map[string]string) result {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return result{resp.StatusCode, resp.Header, b}
}

func (e *env) responses(key, model string, hdr map[string]string) result {
	e.t.Helper()
	return e.do("POST", "/v1/responses", key, map[string]any{"model": model, "input": "hello", "stream": true}, hdr)
}

// statusDoc mirrors the parts of /control/v1/status the tests inspect.
type statusDoc struct {
	SchemaVersion int `json:"schema_version"`
	Accounts      []struct {
		ID                    string     `json:"id"`
		Healthy               bool       `json:"healthy"`
		CooldownUntil         *time.Time `json:"cooldown_until"`
		BackgroundAdmissible  bool       `json:"background_admissible"`
		InteractiveAdmissible bool       `json:"interactive_admissible"`
		Reason                string     `json:"reason"`
		Windows               []struct {
			Kind     string  `json:"kind"`
			UsedFrac float64 `json:"used_frac"`
		} `json:"windows"`
	} `json:"accounts"`
}

func (e *env) status() statusDoc {
	e.t.Helper()
	r := e.do("GET", "/control/v1/status", "", nil, nil)
	if r.status != 200 {
		e.t.Fatalf("status: %d %s", r.status, r.body)
	}
	var d statusDoc
	if err := json.Unmarshal(r.body, &d); err != nil {
		e.t.Fatal(err)
	}
	return d
}

func (e *env) summary(group string) map[string]core.UsageRow {
	e.t.Helper()
	rows, err := e.app.Ledger.Summary(context.Background(), time.Now().Add(-time.Hour), group)
	if err != nil {
		e.t.Fatal(err)
	}
	out := map[string]core.UsageRow{}
	for _, r := range rows {
		out[r.Key] = r
	}
	return out
}

type ledgerRow struct {
	ID, Account, Identity, FailoverOf string
	Status                            int
}

// ledgerRows reads the requests table directly.
func (e *env) ledgerRows() []ledgerRow {
	e.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(e.dataDir, "localrouter.db"))
	if err != nil {
		e.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, account_id, upstream_identity, COALESCE(failover_of,''), status FROM requests ORDER BY started_at, rowid`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []ledgerRow
	for rows.Next() {
		var r ledgerRow
		if err := rows.Scan(&r.ID, &r.Account, &r.Identity, &r.FailoverOf, &r.Status); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func healthyQuota() map[string]codexQuota {
	return map[string]codexQuota{
		primaryGPT: {Used5h: 30, UsedWeekly: 20, Allowed: true},
		secondGPT:  {Used5h: 54, UsedWeekly: 20, Allowed: true},
	}
}

// 1. Interactive request routed to primary; SSE relayed byte-identical; usage
// recorded; upstream credentials correct and client key not forwarded.
func TestE2EInteractiveCodexStream(t *testing.T) {
	e := newEnv(t, envOpts{quota: healthyQuota()})
	r := e.responses(e.interactiveKey, codexModel, map[string]string{"OpenAI-Beta": "responses=v1", "X-LocalRouter-Session": "s1"})
	if r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	if string(r.body) != codexSSE {
		t.Fatalf("SSE not byte-identical:\n got %q\nwant %q", r.body, codexSSE)
	}
	if ct := r.header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("content-type %q", ct)
	}
	hits := e.fakes.hits(primary)
	if len(hits) != 1 || len(e.fakes.hits(secondary)) != 0 {
		t.Fatalf("hits primary=%d secondary=%d", len(hits), len(e.fakes.hits(secondary)))
	}
	h := hits[0].Header
	if got := h.Get("Authorization"); got != "Bearer "+e.access[primary] {
		t.Errorf("upstream Authorization not the primary access token")
	}
	if got := h.Get("ChatGPT-Account-Id"); got != primaryGPT {
		t.Errorf("ChatGPT-Account-Id = %q", got)
	}
	if got := h.Get("originator"); got != "codex_cli_rs" {
		t.Errorf("originator = %q", got)
	}
	if got := h.Get("OpenAI-Beta"); got != "responses=v1" {
		t.Errorf("OpenAI-Beta not forwarded: %q", got)
	}
	if h.Get("X-LocalRouter-Session") != "" {
		t.Errorf("attribution header forwarded upstream")
	}
	for k, vs := range h {
		for _, v := range vs {
			if strings.Contains(v, e.interactiveKey) {
				t.Errorf("client key forwarded upstream in %s", k)
			}
		}
	}
	if !bytes.Contains(hits[0].Body, []byte(`"input":"hello"`)) {
		t.Errorf("body not forwarded verbatim: %s", hits[0].Body)
	}

	row, ok := e.summary("account")[primary]
	if !ok {
		t.Fatalf("no ledger row for %s", primary)
	}
	if row.Requests != 1 || row.InputTokens != 1200 || row.CachedInputTokens != 200 ||
		row.OutputTokens != 300 || row.ReasoningTokens != 120 || row.UnknownUsageRequests != 0 {
		t.Errorf("summary row = %+v", row)
	}
	rows := e.ledgerRows()
	if len(rows) != 1 || rows[0].Identity != primaryGPT || rows[0].Status != 200 {
		t.Errorf("ledger rows = %+v", rows)
	}
}

// 2. Primary at 91% with a 10% reserve: background goes to secondary; status
// explains why primary is not background-admissible.
func TestE2EBackgroundRoutedAroundReserve(t *testing.T) {
	e := newEnv(t, envOpts{quota: map[string]codexQuota{
		primaryGPT: {Used5h: 91, UsedWeekly: 20, Allowed: true},
		secondGPT:  {Used5h: 54, UsedWeekly: 20, Allowed: true},
	}})
	r := e.responses(e.backgroundKey, codexModel, nil)
	if r.status != 200 {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	if n := len(e.fakes.hits(primary)); n != 0 {
		t.Errorf("primary hit %d times", n)
	}
	if n := len(e.fakes.hits(secondary)); n != 1 {
		t.Errorf("secondary hit %d times", n)
	}
	rows := e.ledgerRows()
	if len(rows) != 1 || rows[0].Account != secondary || rows[0].Identity != secondGPT {
		t.Errorf("ledger rows = %+v", rows)
	}

	// An interactive-class key downgraded via header is treated the same way.
	r = e.responses(e.interactiveKey, codexModel, map[string]string{"X-LocalRouter-Class": "background"})
	if r.status != 200 || len(e.fakes.hits(primary)) != 0 {
		t.Errorf("downgraded request: status %d, primary hits %d", r.status, len(e.fakes.hits(primary)))
	}

	for _, a := range e.status().Accounts {
		switch a.ID {
		case primary:
			if a.BackgroundAdmissible || !a.InteractiveAdmissible {
				t.Errorf("primary admissible bg=%v int=%v", a.BackgroundAdmissible, a.InteractiveAdmissible)
			}
			if a.Reason == "" {
				t.Errorf("primary has no reason")
			}
		case secondary:
			if !a.BackgroundAdmissible {
				t.Errorf("secondary not background-admissible: %s", a.Reason)
			}
		}
	}
}

// 3. No codex account admits background: 429 quota_reserve with no upstream
// call; interactive still reaches primary.
func TestE2EBackgroundDeniedInteractiveAllowed(t *testing.T) {
	e := newEnv(t, envOpts{quota: map[string]codexQuota{
		primaryGPT: {Used5h: 91, UsedWeekly: 20, Allowed: true},
		secondGPT:  {Used5h: 100, UsedWeekly: 60, Allowed: false},
	}})
	r := e.responses(e.backgroundKey, codexModel, nil)
	if r.status != http.StatusTooManyRequests {
		t.Fatalf("status %d: %s", r.status, r.body)
	}
	var eb struct {
		Error struct{ Type, Message string } `json:"error"`
	}
	if err := json.Unmarshal(r.body, &eb); err != nil || eb.Error.Type != "quota_reserve" {
		t.Errorf("error body %s", r.body)
	}
	if n := len(e.fakes.hits(primary)) + len(e.fakes.hits(secondary)); n != 0 {
		t.Errorf("upstream hit %d times on denial", n)
	}
	for _, a := range e.status().Accounts {
		if (a.ID == primary || a.ID == secondary) && a.BackgroundAdmissible {
			t.Errorf("%s background-admissible", a.ID)
		}
	}

	r = e.responses(e.interactiveKey, codexModel, nil)
	if r.status != 200 || len(e.fakes.hits(primary)) != 1 {
		t.Errorf("interactive: status %d primary hits %d", r.status, len(e.fakes.hits(primary)))
	}
}

// 4. 429 from primary before any body: failover to secondary, two ledger
// rows, primary in cooldown.
func TestE2EFailoverOn429(t *testing.T) {
	e := newEnv(t, envOpts{quota: healthyQuota()})
	e.fakes.mu.Lock()
	e.fakes.primary429 = true
	e.fakes.mu.Unlock()

	r := e.responses(e.interactiveKey, codexModel, nil)
	if r.status != 200 || string(r.body) != codexSSE {
		t.Fatalf("status %d body %q", r.status, r.body)
	}
	if len(e.fakes.hits(primary)) != 1 || len(e.fakes.hits(secondary)) != 1 {
		t.Fatalf("hits primary=%d secondary=%d", len(e.fakes.hits(primary)), len(e.fakes.hits(secondary)))
	}
	sum := e.summary("account")
	if sum[primary].Requests != 1 || sum[secondary].Requests != 1 {
		t.Errorf("summary = %+v", sum)
	}
	if sum[secondary].InputTokens != 1200 {
		t.Errorf("secondary usage = %+v", sum[secondary])
	}
	rows := e.ledgerRows()
	if len(rows) != 2 {
		t.Fatalf("ledger rows = %+v", rows)
	}
	if rows[0].Account != primary || rows[0].Status != 429 || rows[1].Account != secondary || rows[1].FailoverOf != rows[0].ID {
		t.Errorf("ledger rows = %+v", rows)
	}

	var found bool
	for _, a := range e.status().Accounts {
		if a.ID != primary {
			continue
		}
		found = true
		if a.CooldownUntil == nil || !a.CooldownUntil.After(time.Now().Add(50*time.Second)) {
			t.Errorf("primary cooldown_until = %v", a.CooldownUntil)
		}
		if a.Healthy || a.InteractiveAdmissible {
			t.Errorf("primary healthy=%v interactive=%v", a.Healthy, a.InteractiveAdmissible)
		}
	}
	if !found {
		t.Fatal("primary missing from status")
	}

	// The next interactive request skips the cooling-down primary entirely.
	r = e.responses(e.interactiveKey, codexModel, nil)
	if r.status != 200 || len(e.fakes.hits(primary)) != 1 || len(e.fakes.hits(secondary)) != 2 {
		t.Errorf("after cooldown: status %d hits primary=%d secondary=%d", r.status, len(e.fakes.hits(primary)), len(e.fakes.hits(secondary)))
	}
}

// 5. Ollama chat streaming: include_usage injected, usage captured from the
// final chunk, visible in /control/v1/usage?group=model. Also /responses.
func TestE2EOllamaChatUsage(t *testing.T) {
	e := newEnv(t, envOpts{quota: healthyQuota()})
	r := e.do("POST", "/v1/chat/completions", e.interactiveKey, map[string]any{
		"model": ollamaModel, "stream": true, "stream_options": map[string]any{"foo": "bar"},
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
	}, nil)
	if r.status != 200 || string(r.body) != ollamaChatSSE {
		t.Fatalf("status %d body %q", r.status, r.body)
	}
	r2 := e.do("POST", "/v1/responses", e.interactiveKey, map[string]any{"model": ollamaModel, "input": "x"}, nil)
	if r2.status != 200 {
		t.Fatalf("ollama responses: %d %s", r2.status, r2.body)
	}

	e.fakes.mu.Lock()
	hits := append([]upstreamHit(nil), e.fakes.ollamaHits...)
	e.fakes.mu.Unlock()
	if len(hits) != 2 || hits[0].Path != "/ollama/v1/chat/completions" || hits[1].Path != "/ollama/v1/responses" {
		t.Fatalf("ollama hits = %d", len(hits))
	}
	var sent struct {
		StreamOptions map[string]any `json:"stream_options"`
	}
	if err := json.Unmarshal(hits[0].Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent.StreamOptions["include_usage"] != true || sent.StreamOptions["foo"] != "bar" {
		t.Errorf("stream_options upstream = %v", sent.StreamOptions)
	}
	if got := hits[0].Header.Get("Authorization"); got != "Bearer "+e.ollamaKey {
		t.Errorf("ollama Authorization header wrong")
	}

	u := e.do("GET", "/control/v1/usage?since=24h&group=model", "", nil, nil)
	if u.status != 200 {
		t.Fatalf("usage: %d %s", u.status, u.body)
	}
	var doc struct {
		Rows []core.UsageRow `json:"rows"`
	}
	if err := json.Unmarshal(u.body, &doc); err != nil {
		t.Fatal(err)
	}
	var row *core.UsageRow
	for i := range doc.Rows {
		if doc.Rows[i].Key == ollamaModel {
			row = &doc.Rows[i]
		}
	}
	if row == nil {
		t.Fatalf("no usage row for %s: %s", ollamaModel, u.body)
	}
	if row.Requests != 2 || row.InputTokens != 61 || row.CachedInputTokens != 10 ||
		row.OutputTokens != 11 || row.ReasoningTokens != 3 || row.UnknownUsageRequests != 0 {
		t.Errorf("row = %+v", *row)
	}
}

// 6. Pricing: priced model gets cost_usd; unpriced model has null cost.
func TestE2EPricing(t *testing.T) {
	e := newEnv(t, envOpts{quota: healthyQuota(), pricing: `models:
  test-model-priced:
    input: 2.0
    cached_input: 0.5
    output: 10.0
`})
	for _, m := range []string{pricedModel, unpriced} {
		if r := e.responses(e.interactiveKey, m, nil); r.status != 200 {
			t.Fatalf("%s: %d %s", m, r.status, r.body)
		}
	}
	u := e.do("GET", "/control/v1/usage?since=24h&group=model", "", nil, nil)
	var doc struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal(u.body, &doc); err != nil {
		t.Fatal(err)
	}
	rows := map[string]map[string]any{}
	for _, r := range doc.Rows {
		rows[r["key"].(string)] = r
	}
	// (1200-200)*2 + 200*0.5 + 300*10, per 1M tokens.
	want := (1000*2.0 + 200*0.5 + 300*10.0) / 1e6
	got, ok := rows[pricedModel]["cost_usd"].(float64)
	if !ok || math.Abs(got-want) > 1e-12 {
		t.Errorf("priced cost_usd = %v, want %v (%s)", rows[pricedModel]["cost_usd"], want, u.body)
	}
	if c, present := rows[unpriced]["cost_usd"]; !present || c != nil {
		t.Errorf("unpriced cost_usd = %v (present=%v), want null", c, present)
	}
}

// 7. Client auth and endpoint/provider mismatch errors.
func TestE2EClientErrors(t *testing.T) {
	e := newEnv(t, envOpts{quota: healthyQuota()})
	if r := e.responses("lr-not-a-real-key-0000000000", codexModel, nil); r.status != 401 {
		t.Errorf("unknown key: %d", r.status)
	}
	if r := e.responses("", codexModel, nil); r.status != 401 {
		t.Errorf("missing key: %d", r.status)
	}
	r := e.do("POST", "/v1/chat/completions", e.interactiveKey, map[string]any{"model": codexModel, "messages": []any{}}, nil)
	if r.status != 400 || !strings.Contains(string(r.body), "codex accounts only serve /v1/responses") {
		t.Errorf("codex on chat: %d %s", r.status, r.body)
	}
	if n := len(e.fakes.hits(primary)) + len(e.fakes.hits(secondary)); n != 0 {
		t.Errorf("upstream hit %d times", n)
	}
	m := e.do("GET", "/v1/models", e.interactiveKey, nil, nil)
	if m.status != 200 || !strings.Contains(string(m.body), ollamaModel) || !strings.Contains(string(m.body), codexModel) {
		t.Errorf("models: %d %s", m.status, m.body)
	}
}

// 8. Expired access token: five concurrent requests cause exactly one
// refresh, and the rotated refresh token is persisted.
func TestE2ERefreshSingleFlight(t *testing.T) {
	e := newEnv(t, envOpts{quota: healthyQuota(), expiredAccess: true, noStart: true})
	var wg sync.WaitGroup
	statuses := make([]int, 5)
	for i := range statuses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses[i] = e.responses(e.interactiveKey, codexModel, nil).status
		}()
	}
	wg.Wait()
	for i, s := range statuses {
		if s != 200 {
			t.Errorf("request %d: status %d", i, s)
		}
	}
	e.fakes.mu.Lock()
	issuerHits := e.fakes.issuerHits
	presented := append([]string(nil), e.fakes.issuerRefresh...)
	rotated := e.fakes.rotated[e.refresh[primary]]
	e.fakes.mu.Unlock()
	if issuerHits != 1 {
		t.Fatalf("issuer hits = %d, want 1", issuerHits)
	}
	if presented[0] != e.refresh[primary] {
		t.Errorf("refresh used wrong refresh token")
	}
	hits := e.fakes.hits(primary)
	if len(hits) != 5 {
		t.Fatalf("primary hits = %d", len(hits))
	}
	for _, h := range hits {
		if h.Header.Get("Authorization") == "Bearer "+e.access[primary] {
			t.Errorf("upstream received the expired access token")
		}
	}
	store, err := auth.NewStore(filepath.Join(e.dataDir, "tokens"))
	if err != nil {
		t.Fatal(err)
	}
	tok, err := store.Load(primary)
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshToken != rotated || rotated == "" {
		t.Errorf("rotated refresh token not persisted")
	}
	if "Bearer "+tok.AccessToken != hits[0].Header.Get("Authorization") {
		t.Errorf("persisted access token differs from the one used upstream")
	}
	fi, err := os.Stat(filepath.Join(e.dataDir, "tokens", primary+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode %v", fi.Mode().Perm())
	}
}

// 9. Widget, health, and status schema.
func TestE2EControlSurface(t *testing.T) {
	e := newEnv(t, envOpts{quota: healthyQuota()})
	w := e.do("GET", "/", "", nil, nil)
	if w.status != 200 || !strings.HasPrefix(w.header.Get("Content-Type"), "text/html") ||
		!strings.Contains(strings.ToLower(string(w.body)), "<html") {
		t.Errorf("widget: %d %q", w.status, w.header.Get("Content-Type"))
	}
	if h := e.do("GET", "/healthz", "", nil, nil); h.status != 200 || strings.TrimSpace(string(h.body)) != "ok" {
		t.Errorf("healthz: %d %q", h.status, h.body)
	}
	s := e.status()
	if s.SchemaVersion != 1 || len(s.Accounts) != 3 {
		t.Fatalf("status = %+v", s)
	}
	for _, a := range s.Accounts {
		if !a.Healthy || len(a.Windows) != 2 {
			t.Errorf("account %s healthy=%v windows=%d", a.ID, a.Healthy, len(a.Windows))
		}
	}
	adm := e.do("POST", "/control/v1/admit", "", map[string]string{"class": "background", "model": codexModel}, nil)
	if adm.status != 200 || !strings.Contains(string(adm.body), `"decision":"allow"`) {
		t.Errorf("admit: %d %s", adm.status, adm.body)
	}
}

// 10. No secret appears in logs, control API responses, or SQLite files.
func TestE2ESecretsNeverLeak(t *testing.T) {
	e := newEnv(t, envOpts{quota: healthyQuota(), expiredAccess: true, pricing: "models:\n  gpt-5-codex: {input: 1, output: 2}\n"})
	e.fakes.mu.Lock()
	e.fakes.primary429 = true // exercise failover and 429 logging
	e.fakes.mu.Unlock()

	e.responses(e.interactiveKey, codexModel, map[string]string{"X-LocalRouter-Task": "t"})
	e.responses(e.backgroundKey, codexModel, nil)
	e.do("POST", "/v1/chat/completions", e.interactiveKey, map[string]any{"model": ollamaModel, "stream": true, "messages": []any{}}, nil)
	e.do("POST", "/v1/chat/completions", e.interactiveKey, map[string]any{"model": codexModel}, nil)
	e.responses("lr-wrong-key-but-long-enough", codexModel, nil)

	var surfaces []string
	for _, p := range []string{
		"/control/v1/status", "/", "/healthz",
		"/control/v1/usage?group=account", "/control/v1/usage?group=model",
		"/control/v1/usage?group=class", "/control/v1/usage?group=client",
	} {
		surfaces = append(surfaces, string(e.do("GET", p, "", nil, nil).body))
	}
	surfaces = append(surfaces,
		string(e.do("POST", "/control/v1/admit", "", map[string]string{"class": "interactive", "model": codexModel}, nil).body),
		string(e.do("GET", "/v1/models", e.interactiveKey, nil, nil).body))

	// Close the ledger so all WAL content is visible, then read every file.
	_ = e.app.Close()
	matches, _ := filepath.Glob(filepath.Join(e.dataDir, "localrouter.db*"))
	if len(matches) == 0 {
		t.Fatal("no sqlite files")
	}
	for _, m := range matches {
		b, err := os.ReadFile(m)
		if err != nil {
			t.Fatal(err)
		}
		surfaces = append(surfaces, string(b))
	}
	surfaces = append(surfaces, e.logs.String())

	e.fakes.mu.Lock()
	secrets := []string{e.interactiveKey, e.backgroundKey, e.ollamaKey, "SECRETACCESS", "SECRETREFRESH"}
	for _, v := range e.fakes.rotated {
		secrets = append(secrets, v)
	}
	e.fakes.mu.Unlock()
	for _, id := range []string{primary, secondary} {
		secrets = append(secrets, e.access[id], e.refresh[id], strings.Split(e.access[id], ".")[1])
	}
	for i, s := range surfaces {
		for _, sec := range secrets {
			if strings.Contains(s, sec) {
				t.Errorf("surface %d contains secret %.12q...", i, sec)
			}
		}
	}
	if !strings.Contains(e.logs.String(), "account=") {
		t.Errorf("expected request logs; got %q", e.logs.String())
	}
}
