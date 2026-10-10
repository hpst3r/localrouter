package proxy

// Multi-user request-body field policy (AP-1). Upstream accounts are shared,
// so a user key must not reach provider routing/cost controls (OpenRouter
// "models"/"provider"/..., OpenAI "service_tier"/"background") or references
// to state stored on the shared account ("previous_response_id", ...). Such
// requests are refused before any lease, budget hold or upstream call; legacy
// single-user mode forwards them unchanged.

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func multiUserFieldHarness(t *testing.T) (*harness, func() int32, chan []byte) {
	h := newHarness(t)
	auth := newPrincipalAuth()
	h.multiUser, h.authPrincipal = true, auth.authenticate
	hits, bodies := countingUpstream(h, "a", core.ProviderOpenRouter)
	singleRoute(h, "a")
	h.start()
	return h, hits.Load, bodies
}

func TestMultiUserRefusesUpstreamControlFields(t *testing.T) {
	h, hits, _ := multiUserFieldHarness(t)
	cases := []struct{ key, value string }{
		{"models", `["gpt-x","o1-pro"]`},
		{"route", `"fallback"`},
		{"provider", `{"only":["openai"]}`},
		{"plugins", `[{"id":"web"}]`},
		{"transforms", `["middle-out"]`},
		{"preset", `"p"`},
		{"models", `null`}, // always refused, whatever the value
		{"background", `true`},
		{"background", `"false"`},
		{"service_tier", `"priority"`},
		{"service_tier", `"flex"`},
		{"service_tier", `"AUTO"`},
		{"previous_response_id", `"resp_other_user"`},
		{"conversation", `"conv_1"`},
		{"conversation", `{"id":"conv_1"}`},
		{"prompt", `{"id":"pmpt_1"}`},
		// Case variants that a case-insensitive upstream decoder would match.
		{"Models", `["o1-pro"]`},
		{"SERVICE_TIER", `"priority"`},
		{"Previous_Response_ID", `"resp_x"`},
		{"PROVIDER", `{}`},
	}
	for _, path := range []string{"/v1/chat/completions", "/v1/responses"} {
		for _, tc := range cases {
			body := `{"model":"gpt-x","` + tc.key + `":` + tc.value + `,"messages":[]}`
			resp := h.post(path, userTokenA1, body, nil)
			if resp.StatusCode != http.StatusBadRequest {
				resp.Body.Close()
				t.Fatalf("%s %s=%s: status %d, want 400", path, tc.key, tc.value, resp.StatusCode)
			}
			e := decodeErr(t, resp)
			want := `localrouter: field "` + tc.key + `" is not allowed on this gateway`
			if e.Error.Type != "invalid_request_error" || e.Error.Message != want {
				t.Fatalf("%s %s: error %+v, want %q", path, tc.key, e.Error, want)
			}
		}
	}
	if n := hits(); n != 0 {
		t.Fatalf("upstream hits = %d, want 0", n)
	}
	h.policy.mu.Lock()
	leases := len(h.policy.leases)
	h.policy.mu.Unlock()
	if leases != 0 {
		t.Fatalf("leases = %d, want 0", leases)
	}
	if rows := h.ledger.all(); len(rows) != 0 {
		t.Fatalf("ledger rows = %d, want 0", len(rows))
	}
}

// Go's encoding/json folds keys with Unicode simple folding (U+017F "ſ"
// matches "s"), so such keys are refused too; the echoed name goes through
// core.TruncateLabel. A key that merely contains a policy name is not refused.
func TestMultiUserRefusesUnicodeFoldedFieldNames(t *testing.T) {
	h, hits, _ := multiUserFieldHarness(t)
	key := "ſervice_tier"
	resp := h.post("/v1/chat/completions", userTokenA1, `{"model":"gpt-x","`+key+`":"priority"}`, nil)
	if resp.StatusCode != http.StatusBadRequest {
		resp.Body.Close()
		t.Fatalf("status %d, want 400", resp.StatusCode)
	}
	e := decodeErr(t, resp)
	if want := `localrouter: field "` + core.TruncateLabel(key) + `" is not allowed on this gateway`; e.Error.Message != want {
		t.Fatalf("message %q, want %q", e.Error.Message, want)
	}
	if hits() != 0 {
		t.Fatal("upstream hit")
	}
	resp = h.post("/v1/chat/completions", userTokenA1, `{"model":"gpt-x","modelsx":1,"providers":1}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unrelated keys: status %d", resp.StatusCode)
	}
}

func TestMultiUserAllowsBenignFieldValues(t *testing.T) {
	h, hits, bodies := multiUserFieldHarness(t)
	allowed := []string{
		`"service_tier":"auto"`,
		`"service_tier":"default"`,
		`"service_tier":null`,
		`"background":false`,
		`"background":null`,
		`"previous_response_id":null`,
		`"conversation":null`,
		`"prompt":null`,
		`"metadata":{"models":["x"],"provider":"y"}`, // nested keys are not top-level
	}
	for i, f := range allowed {
		body := `{"model":"gpt-x",` + f + `,"messages":[]}`
		resp := h.post("/v1/chat/completions", userTokenA1, body, nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", f, resp.StatusCode)
		}
		if n := hits(); n != int32(i+1) {
			t.Fatalf("%s: hits %d", f, n)
		}
		select {
		case got := <-bodies:
			if string(got) != body {
				t.Fatalf("%s: forwarded %s, want unchanged %s", f, got, body)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: no upstream body", f)
		}
	}
}

// Legacy single-user mode keeps forwarding every field unchanged.
func TestLegacyForwardsUpstreamControlFields(t *testing.T) {
	h := newHarness(t)
	hits, bodies := countingUpstream(h, "a", core.ProviderOpenRouter)
	singleRoute(h, "a")
	h.start()
	body := `{"model":"gpt-x","models":["gpt-x","o1-pro"],"provider":{"only":["openai"]},` +
		`"route":"fallback","plugins":[{"id":"web"}],"transforms":[],"preset":"p",` +
		`"service_tier":"priority","background":true,"previous_response_id":"resp_1",` +
		`"conversation":"conv_1","prompt":{"id":"pmpt_1"},"messages":[]}`
	resp := h.post("/v1/chat/completions", clientKey, body, nil)
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || hits.Load() != 1 {
		t.Fatalf("status %d hits %d", resp.StatusCode, hits.Load())
	}
	got := <-bodies
	if string(got) != body {
		var m map[string]json.RawMessage
		_ = json.Unmarshal(got, &m)
		t.Fatalf("forwarded %s, want unchanged", got)
	}
}
