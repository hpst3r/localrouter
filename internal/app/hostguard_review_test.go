package app

// Adversarial review tests; skipped until fixed. Run with LOCALROUTER_REVIEW=1
// to see them fail.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func reviewBug(t *testing.T, msg string) {
	t.Helper()
	if os.Getenv("LOCALROUTER_REVIEW") == "" {
		t.Skip("BUG: " + msg)
	}
}

// A fully-qualified Host with a trailing dot ("router.tail.", "localhost.")
// names the same host but is rejected, so agents/browsers configured with an
// absolute FQDN get 403. Fail-closed (not a rebinding bypass) but surprising.
func TestReviewHostGuardTrailingDot(t *testing.T) {
	h := HostGuard([]string{"Router.Tail"}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	for _, host := range []string{"router.tail.", "router.tail.:8787", "localhost.:8787"} {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Host = host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("Host %q: %d, want 200", host, rec.Code)
		}
	}
}
