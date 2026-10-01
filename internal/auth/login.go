package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const (
	loginTimeout        = 15 * time.Minute
	defaultPollInterval = 5 * time.Second
)

// LoginOptions adjusts LoginWithOptions.
type LoginOptions struct {
	// Force allows replacing a stored token that belongs to a different
	// ChatGPT account.
	Force bool
	// PollInterval overrides the server-provided poll interval (tests).
	PollInterval time.Duration
}

// Login runs the Codex device-code flow for accountID, printing the
// verification URL and user code to out, and persists the resulting token.
func (m *Manager) Login(ctx context.Context, accountID string, out io.Writer) error {
	return m.LoginWithOptions(ctx, accountID, out, LoginOptions{})
}

// LoginWithOptions is Login with options. It refuses to overwrite a stored
// token for a different ChatGPT account unless opts.Force, and always refuses
// if the ChatGPT account is already stored under another account id.
func (m *Manager) LoginWithOptions(ctx context.Context, accountID string, out io.Writer, opts LoginOptions) error {
	if a, ok := m.accounts[accountID]; !ok || a.Provider != core.ProviderCodex {
		return fmt.Errorf("auth: %q is not a configured codex account", accountID)
	}
	if m.store == nil {
		return errors.New("auth: no token store configured")
	}
	ctx, cancel := context.WithTimeout(ctx, loginTimeout)
	defer cancel()

	var uc struct {
		DeviceAuthID string       `json:"device_auth_id"`
		UserCode     string       `json:"user_code"`
		Interval     flexDuration `json:"interval"`
	}
	status, code, err := m.postJSON(ctx, m.opts.Issuer+"/api/accounts/deviceauth/usercode",
		map[string]string{"client_id": ClientID}, &uc)
	if err != nil {
		return fmt.Errorf("auth: device login: request user code: %w", err)
	}
	if status != http.StatusOK || uc.DeviceAuthID == "" || uc.UserCode == "" {
		return fmt.Errorf("auth: device login: user code request failed: status %d %s", status, code)
	}

	fmt.Fprintf(out, "To sign in account %s, open:\n\n  %s/codex/device\n\nand enter code: %s\n\nWaiting for authorization...\n",
		accountID, m.opts.Issuer, uc.UserCode)

	interval := time.Duration(uc.Interval)
	if opts.PollInterval > 0 {
		interval = opts.PollInterval
	}
	if interval <= 0 {
		interval = defaultPollInterval
	}

	var dt struct {
		AuthorizationCode string `json:"authorization_code"`
		CodeVerifier      string `json:"code_verifier"`
	}
	poll := map[string]string{"device_auth_id": uc.DeviceAuthID, "user_code": uc.UserCode}
	for {
		status, code, err := m.postJSON(ctx, m.opts.Issuer+"/api/accounts/deviceauth/token", poll, &dt)
		if err != nil {
			if ctx.Err() != nil {
				return loginCtxErr(ctx)
			}
			return fmt.Errorf("auth: device login: poll: %w", err)
		}
		if status == http.StatusOK {
			break
		}
		if status != http.StatusForbidden && status != http.StatusNotFound {
			return fmt.Errorf("auth: device login: poll failed: status %d %s", status, code)
		}
		select {
		case <-ctx.Done():
			return loginCtxErr(ctx)
		case <-time.After(interval):
		}
	}
	if dt.AuthorizationCode == "" || dt.CodeVerifier == "" {
		return errors.New("auth: device login: token response missing authorization_code or code_verifier")
	}

	var tr tokenResponse
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {dt.AuthorizationCode},
		"redirect_uri":  {m.opts.Issuer + "/deviceauth/callback"},
		"client_id":     {ClientID},
		"code_verifier": {dt.CodeVerifier},
	}
	status, code, err = m.postForm(ctx, m.opts.Issuer+"/oauth/token", form, &tr)
	if err != nil {
		return fmt.Errorf("auth: device login: code exchange: %w", err)
	}
	if status != http.StatusOK || tr.AccessToken == "" || tr.RefreshToken == "" {
		return fmt.Errorf("auth: device login: code exchange failed: status %d %s", status, code)
	}
	acct := chatGPTAccountID(tr.IDToken, tr.AccessToken)
	if acct == "" {
		return errors.New("auth: device login: tokens carry no chatgpt_account_id")
	}

	st := m.codexState(accountID)
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := m.checkLoginTarget(accountID, acct, opts.Force); err != nil {
		return err
	}
	now := m.opts.Clock.Now()
	t := Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		IDToken:      tr.IDToken,
		AccountID:    acct,
		ExpiresAt:    tokenExpiry(tr, now),
		LastRefresh:  now.UTC(),
	}
	if err := m.store.Save(accountID, t); err != nil {
		return err
	}
	st.tok = &t
	st.invalidated = false
	m.opts.Logger.Info("codex login complete", "account", accountID, "chatgpt_account_id", acct)
	fmt.Fprintf(out, "Logged in account %s (ChatGPT account %s).\n", accountID, acct)
	return nil
}

// checkLoginTarget enforces the overwrite and duplicate-account guards.
func (m *Manager) checkLoginTarget(accountID, acct string, force bool) error {
	existing, err := m.store.Load(accountID)
	switch {
	case err == nil:
		prev := existing.AccountID
		if prev == "" {
			prev = chatGPTAccountID(existing.IDToken, existing.AccessToken)
		}
		if prev != "" && prev != acct && !force {
			return fmt.Errorf("auth: account %s already holds ChatGPT account %s, refusing to replace with %s (use force to override)",
				accountID, prev, acct)
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	ids, err := m.store.Accounts()
	if err != nil {
		return err
	}
	for _, other := range ids {
		if other == accountID {
			continue
		}
		t, err := m.store.Load(other)
		if err != nil {
			continue
		}
		oacct := t.AccountID
		if oacct == "" {
			oacct = chatGPTAccountID(t.IDToken, t.AccessToken)
		}
		if oacct == acct {
			return fmt.Errorf("auth: ChatGPT account %s is already configured as account %s", acct, other)
		}
	}
	return nil
}

func loginCtxErr(ctx context.Context) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New("auth: device login: timed out waiting for authorization")
	}
	return fmt.Errorf("auth: device login: %w", ctx.Err())
}

// flexDuration decodes an interval in seconds given as a JSON number or string.
type flexDuration time.Duration

func (d *flexDuration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	n, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return errors.New("invalid interval")
	}
	*d = flexDuration(time.Duration(n * float64(time.Second)))
	return nil
}

var _ json.Unmarshaler = (*flexDuration)(nil)
