package proxy

// Multi-user concurrency: the proxy admits through PrincipalLimiter so a
// user's limit spans all of their keys. Uses the real connlim Controller/View.

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/connlim"
	"github.com/hpst3r/localrouter/internal/core"
)

func waitUserActive(t *testing.T, c *connlim.Controller, user string, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for c.UserActive(user) != n {
		if time.Now().After(deadline) {
			t.Fatalf("user active = %d, want %d", c.UserActive(user), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A second key of the same user is refused at the user's cap with the same
// generic concurrency answer as any other limit; another user is admitted;
// the slot returns once the first request ends.
func TestMultiUserConcurrencyCapSpansKeys(t *testing.T) {
	h := newHarness(t)
	auth := newPrincipalAuth()
	h.multiUser, h.authPrincipal = true, auth.authenticate
	gate := make(chan struct{})
	var hits atomic.Int32
	h.blockingUpstream("a", gate, &hits)
	singleRoute(h, "a")
	ctl, err := connlim.New(0, nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := ctl.ViewWithUsers(0, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	h.limiter = view
	h.start()

	held := h.async("/v1/responses", userTokenA1, respBody)
	waitUserActive(t, ctl, puserA, 1)

	resp, err := h.do("/v1/responses", userTokenA2, respBody)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second key status %d, want 429", resp.StatusCode)
	}
	for _, leak := range []string{puserA, pkeyA1, pkeyA2, "user"} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("429 body discloses %q: %s", leak, body)
		}
	}
	other := h.async("/v1/responses", userTokenB1, respBody)
	waitUserActive(t, ctl, puserB, 1)
	unblock(gate)
	drainClose(t, held)
	drainClose(t, other)
	waitUserActive(t, ctl, puserA, 0)
	if st := view.Stats(); st.GlobalActive != 0 {
		t.Fatalf("leaked slots: %+v", st)
	}
	resp, err = h.do("/v1/responses", userTokenA2, respBody)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("after release status %d", resp.StatusCode)
	}
}

// legacyOnlyLimiter has no AcquirePrincipal.
type legacyOnlyLimiter struct{ *fakeLimiter }

func (l legacyOnlyLimiter) Acquire(client string) (func(), bool) {
	return l.fakeLimiter.Acquire(client)
}

// In multi-user mode a limiter that cannot count users is a wiring error and
// fails closed rather than silently dropping the per-user limit.
func TestMultiUserLimiterWithoutPrincipalSupportFailsClosed(t *testing.T) {
	h, _ := multiUserHarness(t)
	h.limiter = legacyOnlyLimiter{newFakeLimiter(0, nil)}
	h.start()
	resp := h.post("/v1/responses", userTokenA1, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", resp.StatusCode)
	}
	if len(h.policy.classes()) != 0 {
		t.Fatal("reached policy")
	}
}

// Legacy mode keeps using Acquire(client name) even when the limiter also
// supports principals.
func TestLegacyModeUsesClientAcquire(t *testing.T) {
	h := newHarness(t)
	h.upstream("a", core.ProviderOpenAICompat, okJSON)
	singleRoute(h, "a")
	ctl, err := connlim.New(0, map[string]int{"alice": 1})
	if err != nil {
		t.Fatal(err)
	}
	h.limiter = ctl
	h.start()
	resp := h.post("/v1/responses", clientKey, respBody, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if st := ctl.Stats(); st.GlobalPeak != 1 || len(st.Clients) != 1 || st.Clients[0].Name != "alice" {
		t.Fatalf("legacy admission stats %+v", st)
	}
}
