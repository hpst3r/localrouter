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
	errClaudeExpired     = errors.New("claude token expired; run claude to refresh")
	errClaudeRejected    = errors.New("claude token rejected; run claude to refresh")
	errClaudeCredMissing = errors.New("claude credentials: unavailable")
	errClaudeCredBad     = errors.New("claude credentials: malformed")
)

// claudeCreds is the subset of ~/.claude/.credentials.json that we read.
type claudeCreds struct {
	ClaudeAiOauth *struct {
		AccessToken      string `json:"accessToken"`
		ExpiresAt        int64  `json:"expiresAt"` // unix ms
		SubscriptionType string `json:"subscriptionType"`
	} `json:"claudeAiOauth"`
}

// claudeCredCache caches the parsed credentials file by (mtime, size).
type claudeCredCache struct {
	path  string
	mtime time.Time
	size  int64
	creds claudeCreds
}

// loadClaudeCreds reads the account's credentials file, reusing the cached
// parse while its mtime and size are unchanged. Read-only.
func (m *Manager) loadClaudeCreds(id string) (claudeCreds, error) {
	if m.opts.ClaudeCredentialsFile == nil {
		return claudeCreds{}, errClaudeCredMissing
	}
	path := m.opts.ClaudeCredentialsFile(id)
	if path == "" {
		return claudeCreds{}, errClaudeCredMissing
	}
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return claudeCreds{}, errClaudeCredMissing
	}
	m.claudeMu.Lock()
	c := m.claudeCache[id]
	m.claudeMu.Unlock()
	if c != nil && c.path == path && c.mtime.Equal(fi.ModTime()) && c.size == fi.Size() {
		return c.creds, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return claudeCreds{}, errClaudeCredMissing
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxBodyBytes))
	if err != nil {
		return claudeCreds{}, errClaudeCredMissing
	}
	var cr claudeCreds
	if err := json.Unmarshal(data, &cr); err != nil || cr.ClaudeAiOauth == nil || cr.ClaudeAiOauth.AccessToken == "" {
		return claudeCreds{}, errClaudeCredBad
	}
	m.claudeMu.Lock()
	m.claudeCache[id] = &claudeCredCache{path: path, mtime: fi.ModTime(), size: fi.Size(), creds: cr}
	m.claudeMu.Unlock()
	return cr, nil
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
	cr, err := m.loadClaudeCreds(id)
	if err != nil {
		return core.Snapshot{}, err
	}
	oa := cr.ClaudeAiOauth
	if oa.ExpiresAt > 0 && !now.Before(time.UnixMilli(oa.ExpiresAt)) {
		return core.Snapshot{}, errClaudeExpired
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.opts.ClaudeUsageURL, nil)
	if err != nil {
		return core.Snapshot{}, errTransport
	}
	req.Header.Set("Authorization", "Bearer "+oa.AccessToken)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", m.opts.ClaudeUserAgent)
	resp, err := m.opts.HTTPClient.Do(req)
	if err != nil {
		return core.Snapshot{}, classifyTransport(ctx, err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return core.Snapshot{}, errClaudeRejected
	case resp.StatusCode != http.StatusOK:
		return core.Snapshot{}, fmt.Errorf("usage api: http %d", resp.StatusCode)
	case err != nil:
		return core.Snapshot{}, classifyTransport(ctx, err)
	}
	windows, err := parseClaudeUsage(body)
	if err != nil {
		return core.Snapshot{}, err
	}
	return core.Snapshot{AccountID: id, FetchedAt: now, Source: SourceUsageAPI, Plan: oa.SubscriptionType, Windows: windows}, nil
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
