package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// Sanitized Claude errors. The token is read from the Claude Code credentials
// file, which LocalRouter never writes; refreshing is left to the claude CLI.
var (
	// ErrClaudeTokenExpired is returned by FetchClaudeSnapshot, without any
	// request, when the credential's expiresAt has passed.
	ErrClaudeTokenExpired = errors.New("claude token expired; run claude to refresh")
	// ErrClaudeTokenRejected is returned when the usage API answers 401.
	ErrClaudeTokenRejected = errors.New("claude token rejected; run claude to refresh")
	// ErrClaudeCredentialsMalformed is returned by ParseClaudeCredentials for
	// unparseable JSON or a missing access token.
	ErrClaudeCredentialsMalformed = errors.New("claude credentials: malformed")

	errClaudeCredMissing = errors.New("claude credentials: unavailable")
)

// ClaudeCredential is the part of a Claude Code OAuth credential that the
// usage fetch needs. ExpiresAt is unix milliseconds (0 = unknown).
type ClaudeCredential struct {
	AccessToken      string
	ExpiresAt        int64
	SubscriptionType string
}

// claudeCreds is the subset of ~/.claude/.credentials.json that we read.
type claudeCreds struct {
	ClaudeAiOauth *struct {
		AccessToken      string `json:"accessToken"`
		ExpiresAt        int64  `json:"expiresAt"` // unix ms
		SubscriptionType string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

// ParseClaudeCredentials parses Claude Code credentials JSON: the contents of
// ~/.claude/.credentials.json, or the macOS keychain item's password, which
// holds the same JSON. It returns ErrClaudeCredentialsMalformed if data is not
// valid JSON or has no access token; the error never includes data.
func ParseClaudeCredentials(data []byte) (ClaudeCredential, error) {
	var cr claudeCreds
	if err := json.Unmarshal(data, &cr); err != nil || cr.ClaudeAiOauth == nil || cr.ClaudeAiOauth.AccessToken == "" {
		return ClaudeCredential{}, ErrClaudeCredentialsMalformed
	}
	oa := cr.ClaudeAiOauth
	return ClaudeCredential{AccessToken: oa.AccessToken, ExpiresAt: oa.ExpiresAt, SubscriptionType: oa.SubscriptionType}, nil
}

// claudeCredCache caches the parsed credentials file by (mtime, size).
type claudeCredCache struct {
	path  string
	mtime time.Time
	size  int64
	cred  ClaudeCredential
}

// loadClaudeCreds reads the account's credentials file, reusing the cached
// parse while its mtime and size are unchanged. Read-only.
func (m *Manager) loadClaudeCreds(id string) (ClaudeCredential, error) {
	if m.opts.ClaudeCredentialsFile == nil {
		return ClaudeCredential{}, errClaudeCredMissing
	}
	path := m.opts.ClaudeCredentialsFile(id)
	if path == "" {
		return ClaudeCredential{}, errClaudeCredMissing
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return ClaudeCredential{}, errClaudeCredMissing
	}
	m.claudeMu.Lock()
	c := m.claudeCache[id]
	m.claudeMu.Unlock()
	if c != nil && c.path == path && c.mtime.Equal(fi.ModTime()) && c.size == fi.Size() {
		return c.cred, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return ClaudeCredential{}, errClaudeCredMissing
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBodyBytes))
	if err != nil {
		return ClaudeCredential{}, errClaudeCredMissing
	}
	cred, err := ParseClaudeCredentials(data)
	if err != nil {
		return ClaudeCredential{}, err
	}
	m.claudeMu.Lock()
	m.claudeCache[id] = &claudeCredCache{path: path, mtime: fi.ModTime(), size: fi.Size(), cred: cred}
	m.claudeMu.Unlock()
	return cred, nil
}

type claudeWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type claudeLimit struct {
	Kind     string   `json:"kind"`
	Percent  *float64 `json:"percent"`
	ResetsAt *string  `json:"resets_at"`
	Scope    *struct {
		Model *struct {
			DisplayName *string `json:"display_name"`
		} `json:"model"`
	} `json:"scope"`
}

func (m *Manager) fetchClaude(ctx context.Context, id string) (core.Snapshot, error) {
	now := m.opts.Clock.Now()
	cred, err := m.loadClaudeCreds(id)
	if err != nil {
		return core.Snapshot{}, err
	}
	return FetchClaudeSnapshot(ctx, m.opts.HTTPClient, m.opts.ClaudeUsageURL, m.opts.ClaudeUserAgent, cred, id, now)
}

// FetchClaudeSnapshot fetches the Claude usage API at url (normally
// DefaultClaudeUsageURL) with cred's access token and returns a snapshot for
// accountID with FetchedAt=now and Source=SourceUsageAPI. An expired cred
// returns ErrClaudeTokenExpired without any request; a 401 returns
// ErrClaudeTokenRejected. Errors are sanitized: never the token or the body.
// A nil client uses a 30s-timeout client. Redirects are never followed (a
// 3xx is an "http 3xx" error); client itself is not modified. The Manager polls through this same
// function; it is exported for the per-host agent.
func FetchClaudeSnapshot(ctx context.Context, client *http.Client, url, userAgent string, cred ClaudeCredential, accountID string, now time.Time) (core.Snapshot, error) {
	if cred.ExpiresAt > 0 && !now.Before(time.UnixMilli(cred.ExpiresAt)) {
		return core.Snapshot{}, ErrClaudeTokenExpired
	}
	if client == nil {
		client = &http.Client{Timeout: defaultHTTPTimeout}
	}
	client = noRedirect(client)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return core.Snapshot{}, errTransport
	}
	req.Header.Set("Authorization", "Bearer "+cred.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := client.Do(req)
	if err != nil {
		return core.Snapshot{}, classifyTransport(ctx, err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return core.Snapshot{}, ErrClaudeTokenRejected
	case resp.StatusCode != http.StatusOK:
		return core.Snapshot{}, fmt.Errorf("usage api: http %d", resp.StatusCode)
	case err != nil:
		return core.Snapshot{}, classifyTransport(ctx, err)
	}
	windows, err := parseClaudeUsage(body)
	if err != nil {
		return core.Snapshot{}, err
	}
	return core.Snapshot{AccountID: accountID, FetchedAt: now, Source: SourceUsageAPI, Plan: cred.SubscriptionType, Windows: windows}, nil
}

// parseClaudeUsage maps an /api/oauth/usage body to windows. Utilization
// values are percentages (0–100).
func parseClaudeUsage(body []byte) ([]core.Window, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, errMalformed
	}
	var out []core.Window
	seen := map[string]bool{}
	add := func(kind string, pct *float64, resetsAt *string, secs int64) {
		if pct == nil || seen[kind] {
			return
		}
		w := core.Window{Kind: kind, UsedFrac: clampFrac(*pct / 100), WindowSeconds: secs}
		if resetsAt != nil {
			if t, err := time.Parse(time.RFC3339Nano, *resetsAt); err == nil {
				w.ResetAt = t
			}
		}
		seen[kind] = true
		out = append(out, w)
	}
	window := func(key string) *claudeWindow {
		var w *claudeWindow
		if v, ok := raw[key]; ok && json.Unmarshal(v, &w) != nil {
			return nil
		}
		return w
	}
	if w := window("five_hour"); w != nil {
		add(core.Window5h, w.Utilization, w.ResetsAt, 18000)
	}
	if w := window("seven_day"); w != nil {
		add(core.WindowWeekly, w.Utilization, w.ResetsAt, 604800)
	}
	var extras []string
	for k := range raw {
		if name, ok := strings.CutPrefix(k, "seven_day_"); ok && name != "" {
			extras = append(extras, k)
		}
	}
	sort.Strings(extras)
	for _, k := range extras {
		if w := window(k); w != nil {
			add("weekly_"+strings.TrimPrefix(k, "seven_day_"), w.Utilization, w.ResetsAt, 604800)
		}
	}
	var limits []claudeLimit
	if v, ok := raw["limits"]; ok && json.Unmarshal(v, &limits) == nil {
		for _, l := range limits {
			if l.Kind != "weekly_scoped" || l.Scope == nil || l.Scope.Model == nil || l.Scope.Model.DisplayName == nil {
				continue
			}
			name := strings.Join(strings.Fields(strings.ToLower(*l.Scope.Model.DisplayName)), "_")
			if name == "" {
				continue
			}
			add("weekly_"+name, l.Percent, l.ResetsAt, 604800)
		}
	}
	return out, nil
}
