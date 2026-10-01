package auth

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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

	"github.com/hpst3r/localrouter/internal/core"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// fixedClock returns t until advanced.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fixedClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// makeJWT builds an unsigned JWT; tag makes the token string unique.
func makeJWT(exp time.Time, acct, tag string) string {
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	claims := map[string]any{"exp": exp.Unix(), "tag": tag}
	if acct != "" {
		claims["https://api.openai.com/auth"] = map[string]any{"chatgpt_account_id": acct}
	}
	b, _ := json.Marshal(claims)
	return hdr + "." + base64.RawURLEncoding.EncodeToString(b) + ".sig-" + tag
}

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// fakeIssuer emulates auth.openai.com.
type fakeIssuer struct {
	srv *httptest.Server

	mu            sync.Mutex
	refreshCalls  int
	refreshDelay  time.Duration
	refreshStatus int    // 0 => 200
	refreshBody   string // body for non-200
	gotRefresh    []string
	newAccess     string
	newRefresh    string
	newID         string
	pending       int // poll responses (403/404 alternating) before success
	polls         int
	loginAccess   string
	loginRefresh  string
	loginID       string
	exchangeForm  map[string]string
	usercodeBody  map[string]string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	f := &fakeIssuer{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", 400)
			return
		}
		if r.Form.Get("client_id") != ClientID {
			http.Error(w, `{"error":"invalid_client"}`, 400)
			return
		}
		switch r.Form.Get("grant_type") {
		case "refresh_token":
			f.mu.Lock()
			f.refreshCalls++
			f.gotRefresh = append(f.gotRefresh, r.Form.Get("refresh_token"))
			delay, status, body := f.refreshDelay, f.refreshStatus, f.refreshBody
			resp := map[string]string{"access_token": f.newAccess, "refresh_token": f.newRefresh, "id_token": f.newID}
			f.mu.Unlock()
			time.Sleep(delay)
			if status != 0 {
				w.WriteHeader(status)
				io.WriteString(w, body)
				return
			}
			json.NewEncoder(w).Encode(resp)
		case "authorization_code":
			f.mu.Lock()
			f.exchangeForm = map[string]string{}
			for k := range r.Form {
				f.exchangeForm[k] = r.Form.Get(k)
			}
			resp := map[string]string{"access_token": f.loginAccess, "refresh_token": f.loginRefresh, "id_token": f.loginID}
			f.mu.Unlock()
			json.NewEncoder(w).Encode(resp)
		default:
			http.Error(w, `{"error":"unsupported_grant_type"}`, 400)
		}
	})
	mux.HandleFunc("POST /api/accounts/deviceauth/usercode", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.usercodeBody = body
		f.mu.Unlock()
		io.WriteString(w, `{"device_auth_id":"dev-123","user_code":"ABCD-EFGH","interval":"5"}`)
	})
	mux.HandleFunc("POST /api/accounts/deviceauth/token", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if body["device_auth_id"] != "dev-123" || body["user_code"] != "ABCD-EFGH" {
			http.Error(w, "bad", 400)
			return
		}
		f.mu.Lock()
		f.polls++
		n := f.polls
		pending := f.pending
		f.mu.Unlock()
		if pending < 0 || n <= pending {
			if n%2 == 1 {
				w.WriteHeader(http.StatusForbidden)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}
		io.WriteString(w, `{"authorization_code":"authcode-1","code_verifier":"verifier-1"}`)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

type harness struct {
	m     *Manager
	store *Store
	iss   *fakeIssuer
	logs  *lockedBuf
	clock *fixedClock
}

func newHarness(t *testing.T, accounts ...core.Account) *harness {
	t.Helper()
	if len(accounts) == 0 {
		accounts = []core.Account{{ID: "a1", Provider: core.ProviderCodex}, {ID: "a2", Provider: core.ProviderCodex}}
	}
	store, err := NewStore(filepath.Join(t.TempDir(), "tokens"))
	if err != nil {
		t.Fatal(err)
	}
	iss := newFakeIssuer(t)
	logs := &lockedBuf{}
	clock := &fixedClock{t: t0}
	m := New(accounts, nil, store, Options{
		Issuer:     iss.srv.URL,
		HTTPClient: iss.srv.Client(),
		Clock:      clock,
		Logger:     slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	return &harness{m: m, store: store, iss: iss, logs: logs, clock: clock}
}

func (h *harness) seed(t *testing.T, id string, exp time.Time, acct, tag string) Token {
	t.Helper()
	tok := Token{
		AccessToken:  makeJWT(exp, acct, "access-"+tag),
		RefreshToken: "rt-" + tag,
		IDToken:      makeJWT(exp, acct, "id-"+tag),
		AccountID:    acct,
		ExpiresAt:    exp,
	}
	if err := h.store.Save(id, tok); err != nil {
		t.Fatal(err)
	}
	return tok
}

// Acceptance 7: 10 concurrent Credential with an expiring token -> exactly
// one refresh; all get the new token; store has the rotated refresh token.
func TestRefreshSingleFlight(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "a1", t0.Add(time.Minute), "chatgpt-1", "old")
	h.iss.newAccess = makeJWT(t0.Add(time.Hour), "chatgpt-1", "access-new")
	h.iss.newRefresh = "rt-new"
	h.iss.refreshDelay = 100 * time.Millisecond

	const n = 10
	var wg sync.WaitGroup
	creds := make([]core.Credential, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			creds[i], errs[i] = h.m.Credential(context.Background(), "a1")
		}()
	}
	close(start)
	wg.Wait()

	if h.iss.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d, want 1", h.iss.refreshCalls)
	}
	if h.iss.gotRefresh[0] != "rt-old" {
		t.Fatalf("refresh sent wrong refresh token")
	}
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("caller %d: %v", i, errs[i])
		}
		if got := creds[i].Headers.Get("Authorization"); got != "Bearer "+h.iss.newAccess {
			t.Fatalf("caller %d got stale access token", i)
		}
		if creds[i].Headers.Get("ChatGPT-Account-Id") != "chatgpt-1" || creds[i].Headers.Get("originator") != "codex_cli_rs" {
			t.Fatalf("caller %d: bad headers %v", i, creds[i].Headers)
		}
		if creds[i].Identity != "chatgpt-1" {
			t.Fatalf("identity = %q", creds[i].Identity)
		}
	}
	saved, err := h.store.Load("a1")
	if err != nil {
		t.Fatal(err)
	}
	if saved.RefreshToken != "rt-new" || saved.AccessToken != h.iss.newAccess {
		t.Fatalf("store not rotated: %v", saved)
	}
	if !saved.ExpiresAt.Equal(t0.Add(time.Hour).Truncate(time.Second)) || !saved.LastRefresh.Equal(t0) {
		t.Fatalf("expiry/last_refresh wrong: %v %v", saved.ExpiresAt, saved.LastRefresh)
	}
}

func TestCredentialFreshNoRefreshThenInvalidate(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "a1", t0.Add(time.Hour), "chatgpt-1", "old")
	h.iss.newAccess = makeJWT(t0.Add(2*time.Hour), "chatgpt-1", "access-new")
	h.iss.newRefresh = "rt-new"

	c, err := h.m.Credential(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if h.iss.refreshCalls != 0 || !strings.Contains(c.Headers.Get("Authorization"), "access-old") {
		t.Fatalf("unexpected refresh or token")
	}
	h.m.Invalidate("a1")
	c, err = h.m.Credential(context.Background(), "a1")
	if err != nil {
		t.Fatal(err)
	}
	if h.iss.refreshCalls != 1 || c.Headers.Get("Authorization") != "Bearer "+h.iss.newAccess {
		t.Fatalf("invalidate did not refresh")
	}
	// Subsequent calls use the cache.
	if _, err := h.m.Credential(context.Background(), "a1"); err != nil || h.iss.refreshCalls != 1 {
		t.Fatalf("unexpected second refresh: %v", err)
	}
}

func TestRefreshKeepsRefreshTokenWhenNotRotated(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "a1", t0.Add(-time.Minute), "chatgpt-1", "old")
	h.iss.newAccess = makeJWT(t0.Add(time.Hour), "", "access-new")
	if _, err := h.m.Credential(context.Background(), "a1"); err != nil {
		t.Fatal(err)
	}
	saved, _ := h.store.Load("a1")
	if saved.RefreshToken != "rt-old" || saved.AccountID != "chatgpt-1" {
		t.Fatalf("lost refresh token or account id: %v", saved)
	}
}

func TestRefreshInvalidGrant(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"invalid_grant", 400, `{"error":"invalid_grant","error_description":"bad rt-old"}`},
		{"unauthorized", 401, `{"error":{"code":"refresh_token_expired","message":"rt-old"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.seed(t, "a1", t0.Add(time.Minute), "chatgpt-1", "old")
			h.iss.refreshStatus, h.iss.refreshBody = tc.status, tc.body
			_, err := h.m.Credential(context.Background(), "a1")
			if err == nil {
				t.Fatal("expected error")
			}
			if !errors.Is(err, ErrLoginRequired) || !strings.Contains(err.Error(), "run: localrouter login a1") {
				t.Fatalf("error lacks login hint: %v", err)
			}
			if strings.Contains(err.Error(), "rt-old") {
				t.Fatalf("error leaks refresh token: %v", err)
			}
			if _, err := h.store.Load("a1"); err != nil {
				t.Fatalf("token file deleted: %v", err)
			}
		})
	}
}

func TestRefreshServerErrorNotLoginRequired(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "a1", t0.Add(time.Minute), "chatgpt-1", "old")
	h.iss.refreshStatus, h.iss.refreshBody = 500, "oops"
	_, err := h.m.Credential(context.Background(), "a1")
	if err == nil || errors.Is(err, ErrLoginRequired) || !strings.Contains(err.Error(), "status 500") {
		t.Fatalf("err = %v", err)
	}
}

func TestCredentialNoToken(t *testing.T) {
	h := newHarness(t)
	_, err := h.m.Credential(context.Background(), "a1")
	if !errors.Is(err, ErrLoginRequired) || !strings.Contains(err.Error(), "localrouter login a1") {
		t.Fatalf("err = %v", err)
	}
	if _, err := h.m.Credential(context.Background(), "nope"); err == nil {
		t.Fatal("unknown account should fail")
	}
}

func TestCredentialHonorsContext(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "a1", t0.Add(time.Minute), "chatgpt-1", "old")
	h.iss.newAccess = makeJWT(t0.Add(time.Hour), "chatgpt-1", "access-new")
	h.iss.refreshDelay = 300 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := h.m.Credential(ctx, "a1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	// The refresh is intentionally detached from the caller's context and keeps
	// running; wait for it to persist so TempDir cleanup doesn't race its write.
	if _, err := h.m.Credential(context.Background(), "a1"); err != nil {
		t.Fatalf("background refresh: %v", err)
	}
}

func TestDeviceLoginPendingThenSuccess(t *testing.T) {
	h := newHarness(t)
	h.iss.pending = 2
	h.iss.loginAccess = makeJWT(t0.Add(time.Hour), "", "access-login")
	h.iss.loginID = makeJWT(t0.Add(time.Hour), "chatgpt-9", "id-login")
	h.iss.loginRefresh = "rt-login"

	var out bytes.Buffer
	err := h.m.LoginWithOptions(context.Background(), "a1", &out, LoginOptions{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if h.iss.polls != 3 {
		t.Fatalf("polls = %d, want 3", h.iss.polls)
	}
	if !strings.Contains(out.String(), h.iss.srv.URL+"/codex/device") || !strings.Contains(out.String(), "ABCD-EFGH") {
		t.Fatalf("output missing url/code: %q", out.String())
	}
	if h.iss.usercodeBody["client_id"] != ClientID {
		t.Fatalf("usercode body = %v", h.iss.usercodeBody)
	}
	want := map[string]string{
		"grant_type":    "authorization_code",
		"code":          "authcode-1",
		"redirect_uri":  h.iss.srv.URL + "/deviceauth/callback",
		"client_id":     ClientID,
		"code_verifier": "verifier-1",
	}
	for k, v := range want {
		if h.iss.exchangeForm[k] != v {
			t.Fatalf("exchange %s = %q, want %q", k, h.iss.exchangeForm[k], v)
		}
	}
	saved, err := h.store.Load("a1")
	if err != nil {
		t.Fatal(err)
	}
	if saved.AccountID != "chatgpt-9" || saved.RefreshToken != "rt-login" {
		t.Fatalf("saved = %v", saved)
	}
	// Login result is usable immediately without refresh.
	c, err := h.m.Credential(context.Background(), "a1")
	if err != nil || c.Identity != "chatgpt-9" || h.iss.refreshCalls != 0 {
		t.Fatalf("credential after login: %v %v", c.Identity, err)
	}
}

func TestDeviceLoginContextCancel(t *testing.T) {
	h := newHarness(t)
	h.iss.pending = -1
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := h.m.LoginWithOptions(ctx, "a1", io.Discard, LoginOptions{PollInterval: 5 * time.Millisecond})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
}

func TestDeviceLoginRejectsNonCodex(t *testing.T) {
	h := newHarness(t, core.Account{ID: "o1", Provider: core.ProviderOllama})
	if err := h.m.Login(context.Background(), "o1", io.Discard); err == nil {
		t.Fatal("expected error")
	}
}

func TestDeviceLoginGuards(t *testing.T) {
	setup := func(t *testing.T) *harness {
		h := newHarness(t)
		h.iss.loginAccess = makeJWT(t0.Add(time.Hour), "chatgpt-new", "access-login")
		h.iss.loginRefresh = "rt-login"
		return h
	}
	opts := LoginOptions{PollInterval: time.Millisecond}

	t.Run("different account needs force", func(t *testing.T) {
		h := setup(t)
		h.seed(t, "a1", t0.Add(time.Hour), "chatgpt-old", "old")
		err := h.m.LoginWithOptions(context.Background(), "a1", io.Discard, opts)
		if err == nil || !strings.Contains(err.Error(), "refusing") {
			t.Fatalf("err = %v", err)
		}
		if saved, _ := h.store.Load("a1"); saved.AccountID != "chatgpt-old" {
			t.Fatal("token overwritten without force")
		}
		opts := opts
		opts.Force = true
		if err := h.m.LoginWithOptions(context.Background(), "a1", io.Discard, opts); err != nil {
			t.Fatal(err)
		}
		if saved, _ := h.store.Load("a1"); saved.AccountID != "chatgpt-new" {
			t.Fatal("force did not overwrite")
		}
	})
	t.Run("same account relogin ok", func(t *testing.T) {
		h := setup(t)
		h.seed(t, "a1", t0.Add(time.Hour), "chatgpt-new", "old")
		if err := h.m.LoginWithOptions(context.Background(), "a1", io.Discard, opts); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("duplicate under other id", func(t *testing.T) {
		h := setup(t)
		h.seed(t, "a2", t0.Add(time.Hour), "chatgpt-new", "other")
		for _, force := range []bool{false, true} {
			o := opts
			o.Force = force
			err := h.m.LoginWithOptions(context.Background(), "a1", io.Discard, o)
			if err == nil || !strings.Contains(err.Error(), "already configured as account a2") {
				t.Fatalf("force=%v err = %v", force, err)
			}
		}
		if _, err := h.store.Load("a1"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a1 should not be written: %v", err)
		}
	})
}

func TestStoreModesAndAtomicity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tokens")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(dir)
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o", fi.Mode().Perm())
	}
	tok := Token{AccessToken: "x", RefreshToken: "y", AccountID: "c", ExpiresAt: t0}
	for range 2 {
		if err := s.Save("acct-1", tok); err != nil {
			t.Fatal(err)
		}
	}
	fi, _ = os.Stat(filepath.Join(dir, "acct-1.json"))
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %o", fi.Mode().Perm())
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("leftover files: %v", ents)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "acct-1.json"))
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"access_token", "refresh_token", "id_token", "account_id", "expires_at", "last_refresh"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing field %s", k)
		}
	}
	got, err := s.Load("acct-1")
	if err != nil || got.RefreshToken != "y" || !got.ExpiresAt.Equal(t0) {
		t.Fatalf("load: %v %v", got, err)
	}
	for _, bad := range []string{"../x", "a/b", ".hidden", ""} {
		if err := s.Save(bad, tok); err == nil {
			t.Fatalf("accepted bad id %q", bad)
		}
	}
	ids, _ := s.Accounts()
	if len(ids) != 1 || ids[0] != "acct-1" {
		t.Fatalf("accounts = %v", ids)
	}
}

func TestStaticCredential(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	os.WriteFile(keyFile, []byte("  sk-file-1\n"), 0o600)
	t.Setenv("LR_TEST_KEY", "sk-env-1")
	m := New([]core.Account{
		{ID: "o1", Provider: core.ProviderOllama},
		{ID: "o2", Provider: core.ProviderOpenAICompat},
		{ID: "o3", Provider: core.ProviderOllama},
	}, map[string]StaticKey{
		"o1": {File: keyFile},
		"o2": {Env: "LR_TEST_KEY"},
		"o3": {File: filepath.Join(dir, "missing")},
	}, nil, Options{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})

	c, err := m.Credential(context.Background(), "o1")
	if err != nil || c.Headers.Get("Authorization") != "Bearer sk-file-1" || c.Identity != "o1" {
		t.Fatalf("o1: %v %v", c, err)
	}
	os.WriteFile(keyFile, []byte("sk-file-2-longer"), 0o600)
	os.Chtimes(keyFile, t0, t0)
	c, _ = m.Credential(context.Background(), "o1")
	if c.Headers.Get("Authorization") != "Bearer sk-file-2-longer" {
		t.Fatalf("key change not picked up: %v", c.Headers)
	}
	c, err = m.Credential(context.Background(), "o2")
	if err != nil || c.Headers.Get("Authorization") != "Bearer sk-env-1" || c.Identity != "o2" {
		t.Fatalf("o2: %v %v", c, err)
	}
	if _, err := m.Credential(context.Background(), "o3"); err == nil {
		t.Fatal("missing key file should fail")
	}
	t.Setenv("LR_TEST_KEY", "")
	if _, err := m.Credential(context.Background(), "o2"); err == nil {
		t.Fatal("empty env should fail")
	}
	m.Invalidate("o1") // must not panic; next call re-reads
	if _, err := m.Credential(context.Background(), "o1"); err != nil {
		t.Fatal(err)
	}
}

func TestJWTAccountIDPrecedence(t *testing.T) {
	id := makeJWT(t0, "from-id", "i")
	at := makeJWT(t0, "from-access", "a")
	if got := chatGPTAccountID(id, at); got != "from-id" {
		t.Fatalf("got %q", got)
	}
	if got := chatGPTAccountID("garbage", at); got != "from-access" {
		t.Fatalf("got %q", got)
	}
	if got := chatGPTAccountID("", "x.%%%.y"); got != "" {
		t.Fatalf("got %q", got)
	}
	if !jwtExpiry(at).Equal(t0) {
		t.Fatal("exp")
	}
}

func writeKey(t *testing.T, dir, name, key string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(key+"\n"), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClientKeys(t *testing.T) {
	dir := t.TempDir()
	k1, _ := GenerateKey()
	k2, _ := GenerateKey()
	ck, err := LoadClientKeys(map[string]string{
		"alice": writeKey(t, dir, "alice", k1, 0o600),
		"bob":   writeKey(t, dir, "bob", k2, 0o400),
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, ok := ck.Lookup(k1); !ok || n != "alice" {
		t.Fatalf("alice: %q %v", n, ok)
	}
	if n, ok := ck.Lookup(k2); !ok || n != "bob" {
		t.Fatalf("bob: %q %v", n, ok)
	}
	for _, bad := range []string{"", k1[:len(k1)-1], k1 + "x", "lr-nope"} {
		if _, ok := ck.Lookup(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
	var nilKeys *ClientKeys
	if _, ok := nilKeys.Lookup(k1); ok {
		t.Fatal("nil keys accepted")
	}

	cases := map[string]map[string]string{
		"group readable": {"c": writeKey(t, dir, "g", k1, 0o640)},
		"world readable": {"c": writeKey(t, dir, "w", k1, 0o604)},
		"short":          {"c": writeKey(t, dir, "s", "short-key", 0o600)},
		"empty":          {"c": writeKey(t, dir, "e", "   ", 0o600)},
		"missing":        {"c": filepath.Join(dir, "nope")},
		"duplicate":      {"a": writeKey(t, dir, "d1", k1, 0o600), "b": writeKey(t, dir, "d2", k1, 0o600)},
	}
	for name, files := range cases {
		_, err := LoadClientKeys(files)
		if err == nil {
			t.Fatalf("%s: expected error", name)
		}
		if strings.Contains(err.Error(), k1) {
			t.Fatalf("%s: error leaks key", name)
		}
	}
}

func TestGenerateKey(t *testing.T) {
	a, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateKey()
	if !strings.HasPrefix(a, "lr-") || len(a) != 3+43 || a == b {
		t.Fatalf("bad keys %q %q", a, b)
	}
	if _, err := base64.RawURLEncoding.DecodeString(a[3:]); err != nil {
		t.Fatal(err)
	}
}

// Acceptance 9: errors and logs never contain token strings.
func TestNoSecretsInLogsOrErrors(t *testing.T) {
	h := newHarness(t)
	old := h.seed(t, "a1", t0.Add(time.Minute), "chatgpt-1", "SENTINEL-OLD")
	h.iss.newAccess = makeJWT(t0.Add(time.Hour), "chatgpt-1", "SENTINEL-NEW-ACCESS")
	h.iss.newRefresh = "rt-SENTINEL-NEW-REFRESH"
	h.iss.newID = makeJWT(t0.Add(time.Hour), "chatgpt-1", "SENTINEL-NEW-ID")
	h.iss.loginAccess = makeJWT(t0.Add(time.Hour), "chatgpt-2", "SENTINEL-LOGIN-ACCESS")
	h.iss.loginRefresh = "rt-SENTINEL-LOGIN-REFRESH"

	var errs []error
	collect := func(_ any, err error) { errs = append(errs, err) }

	// Successful refresh.
	collect(h.m.Credential(context.Background(), "a1"))
	// Failed refreshes with tokens echoed in the error body. Advance the
	// clock past the stale-401 window and the transient backoff so each
	// attempt reaches the issuer.
	h.clock.advance(time.Minute)
	h.m.Invalidate("a1")
	h.iss.refreshStatus = 500
	h.iss.refreshBody = h.iss.newRefresh
	collect(h.m.Credential(context.Background(), "a1"))
	h.clock.advance(time.Minute)
	h.iss.refreshStatus = 400
	h.iss.refreshBody = fmt.Sprintf(`{"error":"invalid_grant","error_description":"%s %s"}`, h.iss.newRefresh, old.RefreshToken)
	collect(h.m.Credential(context.Background(), "a1"))
	// Login (success, then refused overwrite).
	var out bytes.Buffer
	o := LoginOptions{PollInterval: time.Millisecond}
	collect(nil, h.m.LoginWithOptions(context.Background(), "a2", &out, o))
	collect(nil, h.m.LoginWithOptions(context.Background(), "a1", &out, o))

	secrets := []string{
		old.AccessToken, old.RefreshToken, old.IDToken,
		h.iss.newAccess, h.iss.newRefresh, h.iss.newID,
		h.iss.loginAccess, h.iss.loginRefresh, "SENTINEL",
	}
	var all strings.Builder
	all.WriteString(h.logs.String())
	all.WriteString(out.String())
	for _, err := range errs {
		if err != nil {
			all.WriteString(err.Error())
		}
	}
	saved, _ := h.store.Load("a1")
	all.WriteString(fmt.Sprintf("%v %+v %#v %s", saved, saved, saved, saved))
	slog.New(slog.NewTextHandler(&all, nil)).Info("tok", "t", saved)
	text := all.String()
	if !strings.Contains(h.logs.String(), "codex token refreshed") || !strings.Contains(h.logs.String(), "refresh token rejected") {
		t.Fatalf("expected log lines missing:\n%s", h.logs.String())
	}
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Fatalf("secret %q leaked in:\n%s", s, text)
		}
	}
}
