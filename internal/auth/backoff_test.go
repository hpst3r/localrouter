package auth

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func (f *fakeIssuer) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshCalls
}

func rejectRefresh(h *harness) {
	h.iss.mu.Lock()
	defer h.iss.mu.Unlock()
	h.iss.refreshStatus = 400
	h.iss.refreshBody = `{"error":"invalid_grant"}`
}

func acceptRefresh(h *harness) {
	h.iss.mu.Lock()
	defer h.iss.mu.Unlock()
	h.iss.refreshStatus = 0
	h.iss.refreshBody = ""
}

func TestLoginRequiredCachedUntilTokenFileChanges(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "a1", t0.Add(time.Minute), "chatgpt-1", "old")
	h.iss.newAccess = makeJWT(t0.Add(time.Hour), "chatgpt-1", "access-new")
	rejectRefresh(h)
	ctx := context.Background()

	_, err1 := h.m.Credential(ctx, "a1")
	h.clock.advance(time.Hour) // login-required does not expire with time
	h.m.Invalidate("a1")
	_, err2 := h.m.Credential(ctx, "a1")
	if !errors.Is(err1, ErrLoginRequired) || !errors.Is(err2, ErrLoginRequired) {
		t.Fatalf("want ErrLoginRequired, got %v / %v", err1, err2)
	}
	if err1.Error() != err2.Error() || h.iss.calls() != 1 {
		t.Fatalf("cached error differs or refetched: calls=%d %q %q", h.iss.calls(), err1, err2)
	}

	// Token file replaced on disk (e.g. another process logged in): retry.
	acceptRefresh(h)
	h.seed(t, "a1", t0.Add(time.Minute), "chatgpt-1", "replaced-on-disk")
	c, err := h.m.Credential(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if h.iss.calls() != 2 || c.Headers.Get("Authorization") != "Bearer "+h.iss.newAccess {
		t.Fatalf("expected a fresh refresh after file change; calls=%d", h.iss.calls())
	}
}

func TestLoginClearsLoginRequiredCache(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "a1", t0.Add(time.Minute), "chatgpt-2", "old")
	h.iss.loginAccess = makeJWT(t0.Add(time.Hour), "chatgpt-2", "login-access")
	h.iss.loginRefresh = "rt-login"
	rejectRefresh(h)
	ctx := context.Background()
	if _, err := h.m.Credential(ctx, "a1"); !errors.Is(err, ErrLoginRequired) {
		t.Fatalf("want ErrLoginRequired, got %v", err)
	}
	var out bytes.Buffer
	if err := h.m.LoginWithOptions(ctx, "a1", &out, LoginOptions{PollInterval: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	c, err := h.m.Credential(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if c.Headers.Get("Authorization") != "Bearer "+h.iss.loginAccess || h.iss.calls() != 1 {
		t.Fatalf("login token not used; calls=%d", h.iss.calls())
	}
}

func TestTransientRefreshFailureBacksOff(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "a1", t0.Add(time.Minute), "chatgpt-1", "old")
	h.iss.newAccess = makeJWT(t0.Add(time.Hour), "chatgpt-1", "access-new")
	h.iss.refreshStatus = 503
	ctx := context.Background()

	_, err1 := h.m.Credential(ctx, "a1")
	if err1 == nil || errors.Is(err1, ErrLoginRequired) {
		t.Fatalf("want transient error, got %v", err1)
	}
	h.clock.advance(29 * time.Second)
	_, err2 := h.m.Credential(ctx, "a1")
	if err2 == nil || !errors.Is(err2, err1) || h.iss.calls() != 1 {
		t.Fatalf("want wrapped cached error without a new attempt; calls=%d err=%v", h.iss.calls(), err2)
	}

	acceptRefresh(h)
	h.clock.advance(2 * time.Second)
	c, err := h.m.Credential(ctx, "a1")
	if err != nil {
		t.Fatal(err)
	}
	if h.iss.calls() != 2 || c.Headers.Get("Authorization") != "Bearer "+h.iss.newAccess {
		t.Fatalf("expected retry after backoff; calls=%d", h.iss.calls())
	}
}

func TestInvalidateHonoredAfterStaleWindow(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "a1", t0.Add(time.Minute), "chatgpt-1", "old")
	h.iss.newAccess = makeJWT(t0.Add(2*time.Hour), "chatgpt-1", "access-new")
	ctx := context.Background()
	if _, err := h.m.Credential(ctx, "a1"); err != nil || h.iss.calls() != 1 {
		t.Fatalf("initial refresh: calls=%d err=%v", h.iss.calls(), err)
	}
	h.m.Invalidate("a1") // within 30s of refresh: ignored
	if _, err := h.m.Credential(ctx, "a1"); err != nil || h.iss.calls() != 1 {
		t.Fatalf("stale invalidate refreshed: calls=%d err=%v", h.iss.calls(), err)
	}
	h.clock.advance(31 * time.Second)
	h.m.Invalidate("a1")
	if _, err := h.m.Credential(ctx, "a1"); err != nil || h.iss.calls() != 2 {
		t.Fatalf("invalidate after window ignored: calls=%d err=%v", h.iss.calls(), err)
	}
}
