package auth

import (
	"context"
	"testing"
	"time"
)

// After the refresh token is rejected, every Credential call re-POSTs the
// dead refresh token to the issuer: there is no negative caching/backoff, so
// each proxied request and each quota poll hammers auth.openai.com.
func TestReviewRejectedRefreshIsRetriedEveryCall(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "a1", t0.Add(time.Minute), "chatgpt-1", "old")
	h.iss.refreshStatus = 400
	h.iss.refreshBody = `{"error":"invalid_grant"}`
	for range 5 {
		if _, err := h.m.Credential(context.Background(), "a1"); err == nil {
			t.Fatal("expected error")
		}
	}
	h.iss.mu.Lock()
	defer h.iss.mu.Unlock()
	if h.iss.refreshCalls > 1 {
		t.Fatalf("refresh calls = %d after the refresh token was rejected; want 1", h.iss.refreshCalls)
	}
}

// Invalidate cannot tell which token the upstream rejected. A 401 for a
// request sent with the previous access token, arriving after another
// request already refreshed, forces a second (rotating) refresh.
func TestReviewStale401TriggersRedundantRefresh(t *testing.T) {
	h := newHarness(t)
	h.seed(t, "a1", t0.Add(time.Hour), "chatgpt-1", "old")
	h.iss.newAccess = makeJWT(t0.Add(2*time.Hour), "chatgpt-1", "access-new")
	h.iss.newRefresh = "rt-new"
	ctx := context.Background()
	if _, err := h.m.Credential(ctx, "a1"); err != nil { // requests A and B both get the old token
		t.Fatal(err)
	}
	h.m.Invalidate("a1") // A: 401
	if _, err := h.m.Credential(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	h.m.Invalidate("a1") // B: 401 for the OLD token, after A refreshed
	if _, err := h.m.Credential(ctx, "a1"); err != nil {
		t.Fatal(err)
	}
	h.iss.mu.Lock()
	defer h.iss.mu.Unlock()
	if h.iss.refreshCalls != 1 {
		t.Fatalf("refresh calls = %d; want 1", h.iss.refreshCalls)
	}
}
