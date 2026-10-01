package proxy

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func TestDefaultClientSettings(t *testing.T) {
	p := New(Deps{}, Options{})
	c := p.opts.HTTPClient
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport %T", c.Transport)
	}
	if tr == http.DefaultTransport {
		t.Fatal("default transport mutated/shared")
	}
	if c.Timeout != 0 || tr.ResponseHeaderTimeout != defaultResponseHeaderTimeout ||
		tr.TLSHandshakeTimeout != tlsHandshakeTimeout || tr.DialContext == nil || c.CheckRedirect == nil {
		t.Fatalf("client %+v transport rht=%v tls=%v", c, tr.ResponseHeaderTimeout, tr.TLSHandshakeTimeout)
	}
	if err := c.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatalf("CheckRedirect = %v", err)
	}
	if p.opts.StreamIdleTimeout != defaultStreamIdleTimeout {
		t.Fatalf("StreamIdleTimeout = %v", p.opts.StreamIdleTimeout)
	}
	p = New(Deps{}, Options{ResponseHeaderTimeout: time.Second, StreamIdleTimeout: -1})
	if rht := p.opts.HTTPClient.Transport.(*http.Transport).ResponseHeaderTimeout; rht != time.Second {
		t.Fatalf("custom ResponseHeaderTimeout = %v", rht)
	}
	if p.opts.StreamIdleTimeout != 0 {
		t.Fatalf("negative StreamIdleTimeout not disabled: %v", p.opts.StreamIdleTimeout)
	}
}

func TestResponseHeaderTimeoutFailsOver(t *testing.T) {
	h := newHarness(t)
	unblock := make(chan struct{})
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-unblock:
		case <-time.After(5 * time.Second):
		}
	})
	t.Cleanup(func() { close(unblock) }) // runs before the upstream server closes
	h.upstream("b", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a", "b")
	h.opts.ResponseHeaderTimeout = 100 * time.Millisecond
	h.start()

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	rows := h.waitRows(2)
	if rows[0].AccountID != "a" || rows[0].Error == "" || rows[1].AccountID != "b" || rows[1].Error != "" {
		t.Fatalf("rows %+v", rows)
	}
}

func TestStreamIdleTimeout(t *testing.T) {
	h := newHarness(t)
	upstreamDone := make(chan struct{})
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	singleRoute(h, "a")
	h.opts.StreamIdleTimeout = 100 * time.Millisecond
	h.start()

	resp := h.post("/v1/responses", clientKey, `{"model":"gpt-x","stream":true}`, nil)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "event: response.created\ndata: {\"type\":\"response.created\"}\n\n" {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	select {
	case l := <-h.policy.releasedCh:
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.released != 1 || l.outcome.UsageKnown {
			t.Fatalf("lease released=%d outcome %+v", l.released, l.outcome)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("lease not released after idle timeout")
	}
	rows := h.waitRows(1)
	if rows[0].UsageKnown || rows[0].Error != "stream idle timeout" {
		t.Fatalf("row %+v", rows[0])
	}
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request not canceled")
	}
}

// A stream that keeps producing bytes more often than the idle timeout must
// not be aborted, even if its total duration exceeds it.
func TestStreamIdleTimeoutResetOnRead(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderCodex, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i := 0; i < 6; i++ {
			_, _ = io.WriteString(w, ": ping\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
	})
	singleRoute(h, "a")
	h.opts.StreamIdleTimeout = 200 * time.Millisecond
	h.start()

	resp := h.post("/v1/responses", clientKey, `{"model":"gpt-x","stream":true}`, nil)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	rows := h.waitRows(1)
	if !rows[0].UsageKnown || rows[0].Error != "" || rows[0].Usage.InputTokens != 2 {
		t.Fatalf("row %+v", rows[0])
	}
}

// Policy requests a quota refresh on lease release; the proxy must not
// duplicate it.
func TestProxyDoesNotRequestRefresh(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	h.upstream("b", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a", "b")
	h.start()

	resp := h.post("/v1/responses", clientKey, respBody, nil)
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	h.waitRows(2)
	h.quota.mu.Lock()
	defer h.quota.mu.Unlock()
	if len(h.quota.refresh) != 0 {
		t.Fatalf("proxy requested refresh: %v", h.quota.refresh)
	}
}
