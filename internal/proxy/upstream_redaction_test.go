package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

// In multi-user mode the upstream accounts are shared, so nothing an upstream
// says about its account may reach a caller: organization ids, account-wide
// rate-limit state, cookies, request ids, redirects, debug headers, or the
// text of an error body. Legacy mode is a single trusted operator and keeps
// relaying upstream bytes verbatim.

const (
	redactMarker = "org-REDACTMARKER91"
	redactAlias  = "secret-backend-alias-77"
)

// leakHeaders are upstream response headers that name or describe the
// account. Every value carries redactMarker.
var leakHeaders = []string{
	"Openai-Organization", "Anthropic-Organization-Id", "Openai-Project",
	"X-Ratelimit-Remaining-Tokens", "X-Ratelimit-Limit-Requests", "Anthropic-Ratelimit-Requests-Remaining",
	"Set-Cookie", "X-Request-Id", "Request-Id", "Cf-Ray", "Location", "X-Debug-Trace",
	"Openai-Processing-Ms", "Www-Authenticate", "Retry-After-Ms",
}

func setLeakHeaders(h http.Header) {
	for _, k := range leakHeaders {
		h.Set(k, redactMarker)
	}
}

// leakyError answers status with a provider-style error body naming the
// account, the backend alias and the balance, plus every leak header and a
// valid Retry-After.
func leakyError(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		setLeakHeaders(w.Header())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "17")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","code":402,"message":"Limit reached for `+redactAlias+
			` in organization `+redactMarker+`: This request requires more credits, or fewer max_tokens. You requested up to 64 tokens, but can only afford 3."}}`)
	}
}

// allowedErrorHeaders and allowedSuccessHeaders are every response header a
// multi-user caller may see (Date and Content-Length are set by net/http).
var (
	allowedErrorHeaders   = map[string]bool{"Content-Type": true, "Content-Length": true, "Date": true, "Retry-After": true}
	allowedSuccessHeaders = map[string]bool{"Content-Type": true, "Content-Length": true, "Date": true, "Content-Encoding": true}
)

// assertNoLeak fails if resp or body carries the marker, the alias, or the
// balance, or if resp has a header outside allowed.
func assertNoLeak(t *testing.T, resp *http.Response, body []byte, allowed map[string]bool) {
	t.Helper()
	for _, s := range []string{redactMarker, redactAlias, "can only afford"} {
		if bytes.Contains(body, []byte(s)) {
			t.Errorf("body leaks %q: %s", s, body)
		}
	}
	for k, vs := range resp.Header {
		if !allowed[k] {
			t.Errorf("header %s=%q relayed to a multi-user caller", k, vs)
		}
		for _, v := range vs {
			if strings.Contains(v, redactMarker) {
				t.Errorf("header %s leaks the marker", k)
			}
		}
	}
	if resp.ContentLength >= 0 && resp.ContentLength != int64(len(body)) {
		t.Errorf("Content-Length %d does not describe the %d-byte body", resp.ContentLength, len(body))
	}
}

// assertGenericError checks the localrouter envelope for an upstream failure.
func assertGenericError(t *testing.T, resp *http.Response, body []byte, status int) {
	t.Helper()
	if resp.StatusCode != status {
		t.Fatalf("status %d, want %d (body %s)", resp.StatusCode, status, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type %q, want application/json", ct)
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" {
		t.Errorf("Content-Encoding %q on a generated error body", ce)
	}
	var e errorBody
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("error body is not the localrouter envelope: %v: %s", err, body)
	}
	if e.Error.Type != "upstream_error" || !strings.HasPrefix(e.Error.Message, "localrouter: ") {
		t.Errorf("envelope %+v, want type upstream_error and a localrouter message", e.Error)
	}
}

func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return b
}

// redactPrincipals are the multi-user callers: every one gets the redacted
// relay, the operator's own service key included.
var redactPrincipals = []struct{ name, token string }{
	{"user key", userTokenA1},
	{"user-owned static client", ownedTok},
	{"service static client", serviceTok},
}

func TestMultiUserUpstreamErrorsAreGeneric(t *testing.T) {
	for _, pr := range redactPrincipals {
		t.Run(pr.name, func(t *testing.T) {
			t.Run("429 relayed after failover is exhausted", func(t *testing.T) {
				h, _ := multiUserHarness(t)
				var hitsA, hitsB atomic.Int32
				h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
					hitsA.Add(1)
					leakyError(http.StatusTooManyRequests)(w, r)
				})
				h.upstream("b", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
					hitsB.Add(1)
					leakyError(http.StatusTooManyRequests)(w, r)
				})
				singleRoute(h, "a", "b")
				h.start()
				resp := h.post("/v1/responses", pr.token, respBody, nil)
				body := readBody(t, resp)
				assertGenericError(t, resp, body, http.StatusTooManyRequests)
				assertNoLeak(t, resp, body, allowedErrorHeaders)
				if got := resp.Header.Get("Retry-After"); got != "17" {
					t.Errorf("Retry-After %q, want the upstream's 17", got)
				}
				rows := h.waitRows(2)
				if hitsA.Load() != 1 || hitsB.Load() != 1 || len(rows) != 2 ||
					rows[0].Status != 429 || rows[1].Status != 429 || rows[1].FailoverOf != rows[0].ID {
					t.Fatalf("failover changed: hits a=%d b=%d rows=%+v", hitsA.Load(), hitsB.Load(), rows)
				}
			})
			t.Run("final 400", func(t *testing.T) {
				h, _ := multiUserHarness(t)
				h.upstream("a", core.ProviderOpenAICompat, leakyError(http.StatusBadRequest))
				h.start()
				resp := h.post("/v1/responses", pr.token, respBody, nil)
				body := readBody(t, resp)
				assertGenericError(t, resp, body, http.StatusBadRequest)
				assertNoLeak(t, resp, body, allowedErrorHeaders)
				rows := h.waitRows(1)
				if rows[0].Status != 400 || rows[0].BytesOut != int64(len(body)) {
					t.Fatalf("ledger row %+v, want status 400 and the %d bytes sent", rows[0], len(body))
				}
			})
			t.Run("401 after one credential refresh", func(t *testing.T) {
				h, _ := multiUserHarness(t)
				var hits atomic.Int32
				h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					leakyError(http.StatusUnauthorized)(w, r)
				})
				h.start()
				resp := h.post("/v1/responses", pr.token, respBody, nil)
				body := readBody(t, resp)
				assertGenericError(t, resp, body, http.StatusUnauthorized)
				assertNoLeak(t, resp, body, allowedErrorHeaders)
				h.waitRows(2)
				if hits.Load() != 2 || h.creds.invalidated["a"] != 1 {
					t.Fatalf("auth retry changed: hits=%d invalidated=%d", hits.Load(), h.creds.invalidated["a"])
				}
			})
			t.Run("OpenRouter 402 affordability still classified", func(t *testing.T) {
				h, _ := multiUserHarness(t)
				h.upstream("a", core.ProviderOpenRouter, leakyError(http.StatusPaymentRequired))
				h.start()
				resp := h.post("/v1/responses", pr.token, respBody, nil)
				body := readBody(t, resp)
				assertGenericError(t, resp, body, http.StatusPaymentRequired)
				assertNoLeak(t, resp, body, allowedErrorHeaders)
				h.waitRows(1)
				if l := h.policy.leases[0]; !l.outcome.RequestScoped || l.outcome.Status != 402 {
					t.Fatalf("402 affordability body no longer classified: outcome %+v", l.outcome)
				}
			})
		})
	}
}

// A per-candidate backend alias named in an upstream error never reaches the
// caller.
func TestMultiUserUpstreamErrorHidesBackendAlias(t *testing.T) {
	h, _ := multiUserHarness(t)
	h.upstream("a", core.ProviderOllama, leakyError(http.StatusNotFound))
	capRoute(h, map[string]core.UpstreamSpec{"a": {UpstreamModel: redactAlias, Protocols: []string{"responses"}}}, "a")
	h.start()
	resp := h.post("/v1/responses", userTokenA1, respBody, nil)
	body := readBody(t, resp)
	assertGenericError(t, resp, body, http.StatusNotFound)
	assertNoLeak(t, resp, body, allowedErrorHeaders)
	if rows := h.waitRows(1); rows[0].UpstreamModel != redactAlias {
		t.Fatalf("ledger lost the alias: %+v", rows[0])
	}
}

// After a failover the successful answer is relayed byte for byte with only
// the allowlisted headers; a Content-Type parameter is not a side channel.
func TestMultiUserFailoverSuccessHeadersAllowlisted(t *testing.T) {
	const okBody = `{"id":"r1","usage":{"input_tokens":10,"output_tokens":3}}`
	for _, pr := range redactPrincipals {
		t.Run(pr.name, func(t *testing.T) {
			h, _ := multiUserHarness(t)
			h.upstream("a", core.ProviderOpenAICompat, leakyError(http.StatusTooManyRequests))
			h.upstream("b", core.ProviderOpenAICompat, func(w http.ResponseWriter, _ *http.Request) {
				setLeakHeaders(w.Header())
				w.Header().Set("Content-Type", "application/json; charset=utf-8; org="+redactMarker)
				_, _ = io.WriteString(w, okBody)
			})
			singleRoute(h, "a", "b")
			h.start()
			resp := h.post("/v1/responses", pr.token, respBody, nil)
			body := readBody(t, resp)
			if resp.StatusCode != http.StatusOK || string(body) != okBody {
				t.Fatalf("success altered: %d %s", resp.StatusCode, body)
			}
			assertNoLeak(t, resp, body, allowedSuccessHeaders)
			if ct := resp.Header.Get("Content-Type"); ct != "application/json; charset=utf-8" {
				t.Errorf("Content-Type %q, want the media type and charset only", ct)
			}
			rows := h.waitRows(2)
			if rows[1].Status != 200 || !rows[1].UsageKnown || rows[1].Usage.InputTokens != 10 {
				t.Fatalf("success row %+v", rows[1])
			}
		})
	}
}

// A successful event stream keeps its bytes and its event-stream type, loses
// every account header, and still yields usage.
func TestMultiUserStreamHeadersAllowlisted(t *testing.T) {
	stream := "event: response.created\n" +
		"data: {\"type\":\"response.created\",\"response\":{\"usage\":null}}\n\n" +
		"event: response.completed\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":12,\"output_tokens\":5}}}\n\n"
	h, _ := multiUserHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, _ *http.Request) {
		setLeakHeaders(w.Header())
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fl := w.(http.Flusher)
		for i := 0; i < len(stream); i += 9 {
			_, _ = io.WriteString(w, stream[i:min(i+9, len(stream))])
			fl.Flush()
		}
	})
	h.start()
	resp := h.post("/v1/responses", userTokenA1, `{"model":"gpt-x","stream":true}`, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || string(body) != stream {
		t.Fatalf("stream altered: %d %q", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type %q, want text/event-stream", ct)
	}
	assertNoLeak(t, resp, body, allowedSuccessHeaders)
	rows := h.waitRows(1)
	if !rows[0].UsageKnown || rows[0].Usage.InputTokens != 12 || rows[0].BytesOut != int64(len(stream)) {
		t.Fatalf("stream row %+v", rows[0])
	}
}

// A body the transport did not decode keeps its Content-Encoding (the bytes
// are relayed as is); a gzip body the transport decoded is relayed decoded
// with no Content-Encoding. Neither carries the upstream's Content-Length.
func TestMultiUserContentEncodingMatchesRelayedBytes(t *testing.T) {
	t.Run("undecoded encoding is preserved", func(t *testing.T) {
		raw := []byte("\x1b\x07\x00\xf8opaque-br-bytes")
		h, _ := multiUserHarness(t)
		h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, _ *http.Request) {
			setLeakHeaders(w.Header())
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "br")
			w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
			_, _ = w.Write(raw)
		})
		h.start()
		resp := h.post("/v1/responses", userTokenA1, respBody, nil)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK || !bytes.Equal(body, raw) {
			t.Fatalf("encoded body altered: %d %q", resp.StatusCode, body)
		}
		if ce := resp.Header.Get("Content-Encoding"); ce != "br" {
			t.Fatalf("Content-Encoding %q, want br for undecoded bytes", ce)
		}
		assertNoLeak(t, resp, body, allowedSuccessHeaders)
	})
	t.Run("transport-decoded gzip is relayed decoded", func(t *testing.T) {
		const plain = `{"id":"r1","usage":{"input_tokens":7,"output_tokens":2}}`
		var gz bytes.Buffer
		zw := gzip.NewWriter(&gz)
		_, _ = io.WriteString(zw, plain)
		_ = zw.Close()
		h, _ := multiUserHarness(t)
		h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, _ *http.Request) {
			setLeakHeaders(w.Header())
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", strconv.Itoa(gz.Len()))
			_, _ = w.Write(gz.Bytes())
		})
		h.start()
		resp := h.post("/v1/responses", userTokenA1, respBody, nil)
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK || string(body) != plain || resp.Uncompressed {
			t.Fatalf("decoded body altered: %d uncompressed=%v %q", resp.StatusCode, resp.Uncompressed, body)
		}
		assertNoLeak(t, resp, body, allowedSuccessHeaders)
		if rows := h.waitRows(1); !rows[0].UsageKnown || rows[0].Usage.InputTokens != 7 {
			t.Fatalf("row %+v", rows[0])
		}
	})
}

// An error body that never ends is drained only up to a bound: the caller gets
// the envelope and the request finishes.
func TestMultiUserUpstreamErrorDrainIsBounded(t *testing.T) {
	h, _ := multiUserHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		setLeakHeaders(w.Header())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		chunk := bytes.Repeat([]byte(redactMarker), 1024)
		for r.Context().Err() == nil {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
		}
	})
	h.start()
	done := make(chan struct{})
	var resp *http.Response
	var body []byte
	go func() {
		defer close(done)
		resp = h.post("/v1/responses", userTokenA1, respBody, nil)
		defer resp.Body.Close()
		// Bounded read so an unfixed relay cannot exhaust test memory.
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("endless upstream error body was not bounded")
	}
	if len(body) >= 1<<20 {
		t.Fatalf("endless upstream error body relayed to the caller (%d+ bytes)", len(body))
	}
	assertGenericError(t, resp, body, http.StatusBadRequest)
	assertNoLeak(t, resp, body, allowedErrorHeaders)
	h.waitHandled(1)
	if rows := h.waitRows(1); rows[0].Status != 400 || rows[0].Error != "upstream 400" {
		t.Fatalf("row %+v", rows[0])
	}
}

// Legacy mode relays upstream error bodies and headers verbatim.
func TestLegacyUpstreamRelayUnchanged(t *testing.T) {
	t.Run("final 400", func(t *testing.T) {
		h := newHarness(t)
		h.upstream("a", core.ProviderOpenAICompat, leakyError(http.StatusBadRequest))
		singleRoute(h, "a")
		h.start()
		resp := h.post("/v1/responses", clientKey, respBody, nil)
		body := readBody(t, resp)
		if resp.StatusCode != 400 || !bytes.Contains(body, []byte(redactMarker)) || !bytes.Contains(body, []byte("can only afford 3")) {
			t.Fatalf("legacy error body changed: %d %s", resp.StatusCode, body)
		}
		for _, k := range leakHeaders {
			if k == "Set-Cookie" {
				continue // stripped in every mode
			}
			if resp.Header.Get(k) != redactMarker {
				t.Errorf("legacy dropped header %s", k)
			}
		}
	})
	t.Run("429 after failover", func(t *testing.T) {
		h := newHarness(t)
		h.upstream("a", core.ProviderOpenAICompat, leakyError(http.StatusTooManyRequests))
		h.upstream("b", core.ProviderOpenAICompat, leakyError(http.StatusTooManyRequests))
		singleRoute(h, "a", "b")
		h.start()
		resp := h.post("/v1/responses", clientKey, respBody, nil)
		body := readBody(t, resp)
		if resp.StatusCode != 429 || !bytes.Contains(body, []byte(redactMarker)) ||
			resp.Header.Get("Openai-Organization") != redactMarker || resp.Header.Get("X-Ratelimit-Remaining-Tokens") != redactMarker {
			t.Fatalf("legacy relay changed: %d %v %s", resp.StatusCode, resp.Header, body)
		}
	})
	t.Run("success headers", func(t *testing.T) {
		h := newHarness(t)
		h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
			setLeakHeaders(w.Header())
			okJSON(w, r)
		})
		singleRoute(h, "a")
		h.start()
		resp := h.post("/v1/responses", clientKey, respBody, nil)
		_ = readBody(t, resp)
		if resp.Header.Get("Openai-Organization") != redactMarker || resp.Header.Get("X-Request-Id") != redactMarker {
			t.Fatalf("legacy success headers changed: %v", resp.Header)
		}
	})
}
