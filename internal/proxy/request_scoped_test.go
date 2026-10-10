package proxy

// Request-shaped 429/403 from non-OpenRouter providers (AP-3). In multi-user
// mode one user's oversized request must not cool a shared account down for
// everyone: a 429/403 whose body names a request-size limit is request-scoped
// and final (any other account would answer the same). Ordinary rate limits
// stay account-level, and legacy mode is unchanged.

import (
	"bytes"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

const tooLarge429 = `{"error":{"message":"Request too large for gpt-x in organization org-1 on tokens per min (TPM): Limit 30000, Requested 50000.","type":"tokens","code":"rate_limit_exceeded"}}`

func statusUpstream(h *harness, id, provider string, status int, body string) *atomic.Int32 {
	var hits atomic.Int32
	h.upstream(id, provider, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	})
	return &hits
}

func requestScopedHarness(t *testing.T, multiUser bool, provider string, status int, body string) (*harness, *atomic.Int32, *atomic.Int32) {
	h := newHarness(t)
	if multiUser {
		auth := newPrincipalAuth()
		h.multiUser, h.authPrincipal = true, auth.authenticate
	}
	aHits := statusUpstream(h, "a", provider, status, body)
	bHits, _ := countingUpstream(h, "b", provider)
	singleRoute(h, "a", "b")
	h.start()
	return h, aHits, bHits
}

func TestMultiUserRequestSizedRejectionIsRequestScoped(t *testing.T) {
	cases := []struct {
		name, provider string
		status         int
		body           string
	}{
		{"openai_compat 429 TPM request too large", core.ProviderOpenAICompat, 429, tooLarge429},
		{"openai_compat 429 context_length_exceeded", core.ProviderOpenAICompat, 429, `{"error":{"code":"context_length_exceeded","message":"This model's maximum context length is 8192 tokens."}}`},
		{"ollama 403 maximum context length", core.ProviderOllama, 403, `{"error":"maximum context length exceeded"}`},
		{"codex 429 request_too_large", core.ProviderCodex, 429, `{"error":{"type":"request_too_large"}}`},
		{"openai_compat 403 string_above_max_length", core.ProviderOpenAICompat, 403, `{"error":{"code":"string_above_max_length"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, aHits, bHits := requestScopedHarness(t, true, tc.provider, tc.status, tc.body)
			resp := h.post("/v1/responses", userTokenA1, respBody, nil)
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status %d body %s", resp.StatusCode, body)
			}
			if bytes.Contains(body, []byte(tc.body)) {
				t.Fatalf("upstream body relayed verbatim in multi-user mode: %s", body)
			}
			if aHits.Load() != 1 || bHits.Load() != 0 || h.creds.invalidated["a"] != 0 {
				t.Fatalf("a=%d b=%d invalidated=%d; want one try, no failover or refresh",
					aHits.Load(), bHits.Load(), h.creds.invalidated["a"])
			}
			h.waitRows(1)
			if o := h.policy.leases[0].outcome; o.Status != tc.status || !o.RequestScoped {
				t.Fatalf("outcome %+v, want request-scoped", o)
			}
		})
	}
}

func TestMultiUserOrdinaryRateLimitStaysAccountLevel(t *testing.T) {
	cases := []struct{ name, provider, body string }{
		{"openai_compat rate limit", core.ProviderOpenAICompat, `{"error":{"message":"Rate limit reached for gpt-x in organization org-1 on tokens per min (TPM): Limit 30000, Used 29000, Requested 2000.","code":"rate_limit_exceeded"}}`},
		{"codex usage limit", core.ProviderCodex, `{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, aHits, bHits := requestScopedHarness(t, true, tc.provider, 429, tc.body)
			resp := h.post("/v1/responses", userTokenA1, respBody, nil)
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK || aHits.Load() != 1 || bHits.Load() != 1 {
				t.Fatalf("status %d a=%d b=%d; want failover", resp.StatusCode, aHits.Load(), bHits.Load())
			}
			if o := h.policy.leases[0].outcome; o.Status != 429 || o.RequestScoped {
				t.Fatalf("outcome %+v, want account-level", o)
			}
		})
	}
}

// The classification peek is idle-bounded: a 429 whose body stalls is
// abandoned after StreamIdleTimeout and handled as account-level.
func TestMultiUserRequestSizedPeekHonorsIdleTimeout(t *testing.T) {
	h := newHarness(t)
	auth := newPrincipalAuth()
	h.multiUser, h.authPrincipal = true, auth.authenticate
	h.opts.StreamIdleTimeout = 50 * time.Millisecond
	gate := make(chan struct{})
	h.gates = append(h.gates, gate)
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.(http.Flusher).Flush()
		select {
		case <-gate:
		case <-r.Context().Done():
		}
	})
	bHits, _ := countingUpstream(h, "b", core.ProviderOpenAICompat)
	singleRoute(h, "a", "b")
	h.start()
	start := time.Now()
	resp := h.post("/v1/responses", userTokenA1, respBody, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || bHits.Load() != 1 {
		t.Fatalf("status %d b=%d; want failover", resp.StatusCode, bHits.Load())
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("took %v; peek not idle-bounded", d)
	}
	if o := h.policy.leases[0].outcome; o.Status != 429 || o.RequestScoped {
		t.Fatalf("outcome %+v, want account-level", o)
	}
}

// Legacy single-user mode keeps every non-OpenRouter 429 account-level.
func TestLegacyRequestSized429StaysAccountLevel(t *testing.T) {
	h, aHits, bHits := requestScopedHarness(t, false, core.ProviderOpenAICompat, 429, tooLarge429)
	resp := h.post("/v1/responses", clientKey, respBody, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || aHits.Load() != 1 || bHits.Load() != 1 {
		t.Fatalf("status %d a=%d b=%d; want failover", resp.StatusCode, aHits.Load(), bHits.Load())
	}
	if o := h.policy.leases[0].outcome; o.Status != 429 || o.RequestScoped {
		t.Fatalf("outcome %+v, want account-level", o)
	}
}
