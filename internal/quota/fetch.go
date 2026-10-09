package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const maxBodyBytes = 1 << 20

// Sanitized fetch errors. These strings end up in Snapshot.Err and logs, so
// they must never include upstream bodies, URLs with queries, or credentials.
var (
	errCredential = errors.New("usage api: credential unavailable")
	errTimeout    = errors.New("usage api: timeout")
	errCanceled   = errors.New("usage api: canceled")
	errTransport  = errors.New("usage api: transport error")
	errMalformed  = errors.New("usage api: malformed response")
)

// httpStatusError is a non-200 usage API answer. Its text carries only the
// status code, never the body.
type httpStatusError struct{ code int }

func (e *httpStatusError) Error() string { return fmt.Sprintf("usage api: http %d", e.code) }

// get performs an authenticated GET. On 401 it invalidates the credential and
// retries once.
func (m *Manager) get(ctx context.Context, id, url string, extra http.Header) ([]byte, error) {
	return m.getWith(ctx, m.creds, id, url, extra)
}

// getWith is get using the given credential source.
func (m *Manager) getWith(ctx context.Context, creds core.CredentialSource, id, url string, extra http.Header) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		cred, err := creds.Credential(ctx, id)
		if err != nil {
			return nil, errCredential
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, errTransport
		}
		for k, vs := range cred.Headers {
			req.Header[k] = append([]string(nil), vs...)
		}
		for k, vs := range extra {
			req.Header[k] = append([]string(nil), vs...)
		}
		resp, err := m.opts.HTTPClient.Do(req)
		if err != nil {
			return nil, classifyTransport(ctx, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			creds.Invalidate(id)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return nil, &httpStatusError{code: resp.StatusCode}
		}
		if err != nil {
			return nil, classifyTransport(ctx, err)
		}
		return body, nil
	}
}

func classifyTransport(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return errCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errTimeout
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return errTimeout
	}
	return errTransport
}

// --- Codex ---

type codexUsage struct {
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		Allowed         *bool        `json:"allowed"`
		LimitReached    *bool        `json:"limit_reached"`
		PrimaryWindow   *codexWindow `json:"primary_window"`
		SecondaryWindow *codexWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

type codexWindow struct {
	UsedPercent        float64         `json:"used_percent"`
	LimitWindowSeconds int64           `json:"limit_window_seconds"`
	ResetAfterSeconds  *float64        `json:"reset_after_seconds"`
	ResetAt            json.RawMessage `json:"reset_at"`
}

func (m *Manager) fetchCodex(ctx context.Context, id string) (core.Snapshot, error) {
	now := m.opts.Clock.Now()
	body, err := m.get(ctx, id, m.opts.CodexUsageURL, http.Header{
		"Accept":     {"application/json"},
		"User-Agent": {"codex-cli"},
	})
	if err != nil {
		return core.Snapshot{}, err
	}
	var u codexUsage
	if err := json.Unmarshal(body, &u); err != nil {
		return core.Snapshot{}, errMalformed
	}
	snap := core.Snapshot{AccountID: id, FetchedAt: now, Source: SourceUsageAPI, Plan: u.PlanType}
	if rl := u.RateLimit; rl != nil {
		if w := rl.PrimaryWindow; w != nil {
			snap.Windows = append(snap.Windows, w.toWindow(core.Window5h, now))
		}
		if w := rl.SecondaryWindow; w != nil {
			snap.Windows = append(snap.Windows, w.toWindow(core.WindowWeekly, now))
		}
		if rl.Allowed != nil || rl.LimitReached != nil {
			allowed := (rl.Allowed == nil || *rl.Allowed) && (rl.LimitReached == nil || !*rl.LimitReached)
			snap.Allowed = &allowed
		}
	}
	return snap, nil
}

func (w *codexWindow) toWindow(kind string, now time.Time) core.Window {
	out := core.Window{Kind: kind, UsedFrac: clampFrac(w.UsedPercent / 100), WindowSeconds: w.LimitWindowSeconds}
	var unix float64
	if len(w.ResetAt) > 0 && json.Unmarshal(w.ResetAt, &unix) == nil && unix > 0 {
		out.ResetAt = time.Unix(int64(unix), 0)
	} else if w.ResetAfterSeconds != nil && *w.ResetAfterSeconds >= 0 {
		out.ResetAt = now.Add(time.Duration(*w.ResetAfterSeconds * float64(time.Second)))
	}
	return out
}

// --- Ollama ---

type ollamaUsage struct {
	Limits struct {
		Session *ollamaLimit `json:"session"`
		Weekly  *ollamaLimit `json:"weekly"`
	} `json:"limits"`
}

type ollamaLimit struct {
	Usage  *float64 `json:"usage"`
	Models []struct {
		Name         string `json:"name"`
		RequestCount int64  `json:"request_count"`
	} `json:"models"`
}

// ollamaModelCounts converts a window's models[] into sorted counts
// (descending by requests, then name); nil when the provider sent none.
func ollamaModelCounts(l *ollamaLimit) []core.ModelCount {
	if l == nil || len(l.Models) == 0 {
		return nil
	}
	out := make([]core.ModelCount, 0, len(l.Models))
	for _, m := range l.Models {
		if m.Name == "" || m.RequestCount < 0 {
			continue
		}
		out = append(out, core.ModelCount{Model: m.Name, Requests: m.RequestCount})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		return out[i].Model < out[j].Model
	})
	return out
}

func (m *Manager) fetchOllama(ctx context.Context, id string) (core.Snapshot, error) {
	now := m.opts.Clock.Now()
	body, err := m.get(ctx, id, m.opts.OllamaUsageURL, http.Header{"Accept": {"application/json"}})
	if err != nil {
		return core.Snapshot{}, err
	}
	var u ollamaUsage
	if err := json.Unmarshal(body, &u); err != nil {
		return core.Snapshot{}, errMalformed
	}
	snap := core.Snapshot{AccountID: id, FetchedAt: now, Source: SourceUsageAPI}
	// Values are already fractions; no reset time is reported.
	if l := u.Limits.Session; l != nil && l.Usage != nil {
		snap.Windows = append(snap.Windows, core.Window{Kind: core.Window5h, UsedFrac: clampFrac(*l.Usage), WindowSeconds: 18000})
	}
	if l := u.Limits.Weekly; l != nil && l.Usage != nil {
		snap.Windows = append(snap.Windows, core.Window{Kind: core.WindowWeekly, UsedFrac: clampFrac(*l.Usage), WindowSeconds: 604800})
	}
	for kind, l := range map[string]*ollamaLimit{core.Window5h: u.Limits.Session, core.WindowWeekly: u.Limits.Weekly} {
		if mc := ollamaModelCounts(l); mc != nil {
			if snap.ModelRequests == nil {
				snap.ModelRequests = map[string][]core.ModelCount{}
			}
			snap.ModelRequests[kind] = mc
		}
	}
	return snap, nil
}
