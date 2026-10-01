package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
	"golang.org/x/sync/singleflight"
)

// Codex OAuth constants (see SPEC "Auth").
const (
	ClientID      = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultIssuer = "https://auth.openai.com"
	originator    = "codex_cli_rs"
)

// refreshTimeout bounds one refresh request. The refresh runs detached from
// any single caller's context because its result is shared by all waiters.
const refreshTimeout = 30 * time.Second

// refreshBackoff is how long a transient refresh failure (network, 5xx,
// persist error) suppresses new refresh attempts for an account.
const refreshBackoff = 30 * time.Second

// staleInvalidateWindow: Invalidate is ignored this soon after a successful
// refresh or login, since the 401 almost certainly came from a request that
// was sent with the previous access token.
const staleInvalidateWindow = 30 * time.Second

// ErrLoginRequired is wrapped by errors returned when the stored refresh
// token is rejected or missing and the user must log in again.
var ErrLoginRequired = errors.New("login required")

// StaticKey locates the API key of a non-Codex account. File is preferred;
// Env is used when File is empty.
type StaticKey struct {
	File string
	Env  string
}

// Options configures a Manager. Zero values select defaults.
type Options struct {
	Issuer      string        // default DefaultIssuer
	HTTPClient  *http.Client  // default: 30s timeout client
	Clock       core.Clock    // default core.SystemClock
	Logger      *slog.Logger  // default slog.Default()
	RefreshSkew time.Duration // refresh when access token expires within this; default 5m
}

// Manager implements core.CredentialSource for Codex (OAuth) and static-key
// accounts, and runs the Codex device login.
type Manager struct {
	opts     Options
	accounts map[string]core.Account
	keys     map[string]StaticKey
	store    *Store
	sf       singleflight.Group

	mu     sync.Mutex // guards codex and static maps
	codex  map[string]*codexState
	static map[string]*staticState
}

// codexState is the in-memory token cache of one Codex account. mu serializes
// load, refresh and persist so a rotated refresh token is on disk before any
// caller sees the new access token.
type codexState struct {
	mu          sync.Mutex
	tok         *Token
	invalidated bool
	refreshedAt time.Time // last successful refresh or login (in-process)

	// Cached refresh failure. A login-required failure (failLogin) holds
	// until the token file changes (failStamp) or Login succeeds; any other
	// failure holds until failUntil.
	failErr   error
	failLogin bool
	failStamp fileStamp
	failUntil time.Time
}

type staticState struct {
	mu    sync.Mutex
	key   string
	mtime time.Time
	size  int64
}

var _ core.CredentialSource = (*Manager)(nil)

// New returns a Manager for accounts. keys maps non-Codex account ids to
// their static key location. store may be nil if no Codex accounts are used.
func New(accounts []core.Account, keys map[string]StaticKey, store *Store, opts Options) *Manager {
	if opts.Issuer == "" {
		opts.Issuer = DefaultIssuer
	}
	opts.Issuer = strings.TrimRight(opts.Issuer, "/")
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: refreshTimeout}
	}
	if opts.Clock == nil {
		opts.Clock = core.SystemClock{}
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.RefreshSkew <= 0 {
		opts.RefreshSkew = 5 * time.Minute
	}
	m := &Manager{
		opts:     opts,
		accounts: make(map[string]core.Account, len(accounts)),
		keys:     make(map[string]StaticKey, len(keys)),
		store:    store,
		codex:    map[string]*codexState{},
		static:   map[string]*staticState{},
	}
	for _, a := range accounts {
		m.accounts[a.ID] = a
	}
	for id, k := range keys {
		m.keys[id] = k
	}
	return m
}

// Credential returns a valid credential for accountID, refreshing Codex
// tokens if they expire within RefreshSkew or were invalidated.
//
// Codex refresh failures are cached per account so callers do not hammer the
// issuer: after ErrLoginRequired the same error is returned without network
// calls until the token file changes on disk or Login succeeds; after any
// other refresh failure no new attempt is made for 30s and the last error is
// returned wrapped.
func (m *Manager) Credential(ctx context.Context, accountID string) (core.Credential, error) {
	a, ok := m.accounts[accountID]
	if !ok {
		return core.Credential{}, fmt.Errorf("auth: unknown account %q", accountID)
	}
	if a.Provider == core.ProviderCodex {
		return m.codexCredential(ctx, accountID)
	}
	return m.staticCredential(accountID)
}

// Invalidate forces the next Credential call for accountID to refresh (Codex)
// or re-read the key (static).
//
// For Codex accounts, Invalidate is ignored if a refresh or login completed
// within the last 30s: the 401 that triggered it was almost certainly for a
// request sent with the previous access token, and refreshing again would
// needlessly rotate the refresh token. Invalidate also does not bypass a
// cached refresh failure (see Credential).
func (m *Manager) Invalidate(accountID string) {
	a, ok := m.accounts[accountID]
	if !ok {
		return
	}
	if a.Provider == core.ProviderCodex {
		st := m.codexState(accountID)
		st.mu.Lock()
		if !st.refreshedAt.IsZero() && m.opts.Clock.Now().Sub(st.refreshedAt) < staleInvalidateWindow {
			m.opts.Logger.Debug("codex invalidate ignored: token refreshed recently", "account", accountID)
		} else {
			st.invalidated = true
		}
		st.mu.Unlock()
		return
	}
	st := m.staticState(accountID)
	st.mu.Lock()
	st.key, st.mtime, st.size = "", time.Time{}, 0
	st.mu.Unlock()
}

func (m *Manager) codexState(id string) *codexState {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.codex[id]
	if !ok {
		st = &codexState{}
		m.codex[id] = st
	}
	return st
}

func (m *Manager) staticState(id string) *staticState {
	m.mu.Lock()
	defer m.mu.Unlock()
	st, ok := m.static[id]
	if !ok {
		st = &staticState{}
		m.static[id] = st
	}
	return st
}

func codexCred(t *Token) core.Credential {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+t.AccessToken)
	h.Set("ChatGPT-Account-Id", t.AccountID)
	h.Set("originator", originator)
	return core.Credential{Headers: h, Identity: t.AccountID}
}

// loadLocked ensures st.tok is populated from the store. st.mu must be held.
func (m *Manager) loadLocked(id string, st *codexState) error {
	if st.tok != nil {
		return nil
	}
	if m.store == nil {
		return fmt.Errorf("auth: codex account %s: no token store configured", id)
	}
	t, err := m.store.Load(id)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("auth: codex account %s: no token stored: %w; run: localrouter login %s", id, ErrLoginRequired, id)
	}
	if err != nil {
		return err
	}
	if t.ExpiresAt.IsZero() {
		t.ExpiresAt = jwtExpiry(t.AccessToken)
	}
	if t.AccountID == "" {
		t.AccountID = chatGPTAccountID(t.IDToken, t.AccessToken)
	}
	st.tok = &t
	return nil
}

// needsRefreshLocked reports whether st.tok must be refreshed. An unknown
// expiry is treated as valid; a 401 upstream leads to Invalidate.
func (m *Manager) needsRefreshLocked(st *codexState) bool {
	if st.invalidated || st.tok.AccessToken == "" {
		return true
	}
	exp := st.tok.ExpiresAt
	return !exp.IsZero() && !m.opts.Clock.Now().Add(m.opts.RefreshSkew).Before(exp)
}

func (m *Manager) codexCredential(ctx context.Context, id string) (core.Credential, error) {
	st := m.codexState(id)
	st.mu.Lock()
	if err := m.cachedFailureLocked(id, st); err != nil {
		st.mu.Unlock()
		return core.Credential{}, err
	}
	if err := m.loadLocked(id, st); err != nil {
		st.mu.Unlock()
		return core.Credential{}, err
	}
	if !m.needsRefreshLocked(st) {
		c := codexCred(st.tok)
		st.mu.Unlock()
		return c, nil
	}
	st.mu.Unlock()

	ch := m.sf.DoChan(id, func() (any, error) { return m.refresh(id, st) })
	select {
	case r := <-ch:
		if r.Err != nil {
			return core.Credential{}, r.Err
		}
		return r.Val.(core.Credential), nil
	case <-ctx.Done():
		return core.Credential{}, ctx.Err()
	}
}

// refresh exchanges the refresh token, persists the result, then updates the
// cache. Runs under singleflight and holds st.mu throughout so the rotated
// refresh token is durable before anyone receives the new access token.
func (m *Manager) refresh(id string, st *codexState) (core.Credential, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := m.cachedFailureLocked(id, st); err != nil {
		return core.Credential{}, err
	}
	if err := m.loadLocked(id, st); err != nil {
		return core.Credential{}, err
	}
	// A refresh that completed just before this flight started may have
	// already satisfied us.
	if !m.needsRefreshLocked(st) {
		return codexCred(st.tok), nil
	}
	c, err := m.refreshLocked(id, st)
	if err != nil {
		m.recordFailureLocked(id, st, err)
		return core.Credential{}, err
	}
	return c, nil
}

// cachedFailureLocked returns the cached refresh failure for id if it still
// applies, clearing it otherwise. A login-required failure is cleared (and
// the token reloaded) once the token file changes on disk. st.mu must be held.
func (m *Manager) cachedFailureLocked(id string, st *codexState) error {
	if st.failErr == nil {
		return nil
	}
	if st.failLogin {
		if m.store == nil || m.store.stamp(id) == st.failStamp {
			return st.failErr
		}
		st.tok = nil
	} else if m.opts.Clock.Now().Before(st.failUntil) {
		return fmt.Errorf("auth: codex account %s: refresh backing off until %s: %w",
			id, st.failUntil.UTC().Format(time.RFC3339), st.failErr)
	}
	m.clearFailureLocked(st)
	return nil
}

func (m *Manager) recordFailureLocked(id string, st *codexState, err error) {
	st.failErr = err
	st.failLogin = errors.Is(err, ErrLoginRequired)
	if st.failLogin {
		st.failStamp = m.store.stamp(id)
	} else {
		st.failUntil = m.opts.Clock.Now().Add(refreshBackoff)
	}
}

func (m *Manager) clearFailureLocked(st *codexState) {
	st.failErr, st.failLogin, st.failStamp, st.failUntil = nil, false, fileStamp{}, time.Time{}
}

// refreshLocked performs the refresh request and persists the result. st.mu
// must be held and st.tok loaded.
func (m *Manager) refreshLocked(id string, st *codexState) (core.Credential, error) {
	if st.tok.RefreshToken == "" {
		return core.Credential{}, fmt.Errorf("auth: codex account %s: no refresh token: %w; run: localrouter login %s", id, ErrLoginRequired, id)
	}

	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {st.tok.RefreshToken},
		"client_id":     {ClientID},
	}
	var tr tokenResponse
	status, code, err := m.postForm(ctx, m.opts.Issuer+"/oauth/token", form, &tr)
	if err != nil {
		m.opts.Logger.Warn("codex token refresh failed", "account", id, "err", err)
		return core.Credential{}, fmt.Errorf("auth: codex account %s: refresh: %w", id, err)
	}
	if status != http.StatusOK {
		if status == http.StatusUnauthorized || isLoginRequiredCode(code) {
			m.opts.Logger.Warn("codex refresh token rejected", "account", id, "status", status, "error_code", code)
			return core.Credential{}, fmt.Errorf("auth: codex account %s: refresh token rejected (status %d %s): %w; run: localrouter login %s",
				id, status, code, ErrLoginRequired, id)
		}
		m.opts.Logger.Warn("codex token refresh failed", "account", id, "status", status, "error_code", code)
		return core.Credential{}, fmt.Errorf("auth: codex account %s: refresh failed: status %d %s", id, status, code)
	}
	if tr.AccessToken == "" {
		return core.Credential{}, fmt.Errorf("auth: codex account %s: refresh response missing access_token", id)
	}

	next := *st.tok
	next.AccessToken = tr.AccessToken
	if tr.RefreshToken != "" {
		next.RefreshToken = tr.RefreshToken
	}
	if tr.IDToken != "" {
		next.IDToken = tr.IDToken
	}
	if acct := chatGPTAccountID(next.IDToken, next.AccessToken); acct != "" {
		next.AccountID = acct
	}
	now := m.opts.Clock.Now()
	next.ExpiresAt = tokenExpiry(tr, now)
	next.LastRefresh = now.UTC()
	if err := m.store.Save(id, next); err != nil {
		// Do not hand out a token whose rotated refresh token is not durable.
		m.opts.Logger.Error("codex token persist failed", "account", id, "err", err)
		return core.Credential{}, fmt.Errorf("auth: codex account %s: persist refreshed token: %w", id, err)
	}
	st.tok = &next
	st.invalidated = false
	st.refreshedAt = now
	m.opts.Logger.Info("codex token refreshed", "account", id, "expires_at", next.ExpiresAt)
	return codexCred(st.tok), nil
}

// tokenResponse is the OAuth token endpoint success body.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	ExpiresIn    int64  `json:"expires_in"`
}

func tokenExpiry(tr tokenResponse, now time.Time) time.Time {
	if exp := jwtExpiry(tr.AccessToken); !exp.IsZero() {
		return exp
	}
	if tr.ExpiresIn > 0 {
		return now.Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return time.Time{}
}

func isLoginRequiredCode(code string) bool {
	switch code {
	case "invalid_grant", "refresh_token_expired", "refresh_token_reused", "refresh_token_invalidated":
		return true
	}
	return false
}

var errorCodeRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// errorCode extracts a short OAuth error code from a JSON error body. Only a
// syntactically safe code is returned; the raw body is never surfaced.
func errorCode(body []byte) string {
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	var code string
	switch v := e.Error.(type) {
	case string:
		code = v
	case map[string]any:
		code, _ = v["code"].(string)
	}
	if !errorCodeRe.MatchString(code) {
		return ""
	}
	return code
}

// postForm POSTs a form and decodes a 200 JSON body into out. For non-200 it
// returns the status and a sanitized error code; err is for transport/decode
// failures only and never contains body content.
func (m *Manager) postForm(ctx context.Context, u string, form url.Values, out any) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(form.Encode()))
	if err != nil {
		return 0, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	return m.do(req, out)
}

func (m *Manager) postJSON(ctx context.Context, u string, body, out any) (int, string, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, "", fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(b))
	if err != nil {
		return 0, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	return m.do(req, out)
}

func (m *Manager) do(req *http.Request, out any) (int, string, error) {
	resp, err := m.opts.HTTPClient.Do(req)
	if err != nil {
		// Report only method+path; never the full URL.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, "", fmt.Errorf("%s %s: %w", req.Method, req.URL.Path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, "", fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, errorCode(body), nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return 0, "", errors.New("decode response: invalid JSON")
	}
	return resp.StatusCode, "", nil
}

// staticCredential reads the account's key from File (cached by mtime/size)
// or Env.
func (m *Manager) staticCredential(id string) (core.Credential, error) {
	k, ok := m.keys[id]
	if !ok || (k.File == "" && k.Env == "") {
		return core.Credential{}, fmt.Errorf("auth: account %s: no api key configured", id)
	}
	var key string
	if k.File != "" {
		var err error
		if key, err = m.readKeyFile(id, k.File); err != nil {
			return core.Credential{}, err
		}
	} else {
		key = strings.TrimSpace(os.Getenv(k.Env))
		if key == "" {
			return core.Credential{}, fmt.Errorf("auth: account %s: env %s is empty", id, k.Env)
		}
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer "+key)
	return core.Credential{Headers: h, Identity: id}, nil
}

func (m *Manager) readKeyFile(id, path string) (string, error) {
	st := m.staticState(id)
	st.mu.Lock()
	defer st.mu.Unlock()
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("auth: account %s: api key file: %w", id, err)
	}
	if st.key != "" && fi.ModTime().Equal(st.mtime) && fi.Size() == st.size {
		return st.key, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("auth: account %s: api key file: %w", id, err)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", fmt.Errorf("auth: account %s: api key file %s is empty", id, path)
	}
	st.key, st.mtime, st.size = key, fi.ModTime(), fi.Size()
	return key, nil
}
