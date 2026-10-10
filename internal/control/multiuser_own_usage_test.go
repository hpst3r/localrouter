package control

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// The owner predicate applies before ranking, top-N and __other__: another
// owner's heavier traffic can neither take a top slot in nor be folded into
// the user's own analytics, on the session and the bearer read paths.
func TestOwnUsageScopedBeforeRankingAndOther(t *testing.T) {
	f := newMUFixture(t, nil)
	alice, bob := f.addUser("alice", core.RoleUser), f.addUser("bob", core.RoleUser)
	n := 0
	rec := func(account, model string, u muUser, times int) {
		for i := 0; i < times; i++ {
			n++
			f.record(fmt.Sprintf("r%d", n), account, model, u.KeyID, u.ID, u.KeyID, 10)
		}
	}
	rec("primary", "m-alice-big", alice, 3)
	rec("primary", "m-alice-small", alice, 1)
	for _, m := range []string{"m-bob-1", "m-bob-2", "m-bob-3"} {
		rec("claude-a", m, bob, 10)
	}
	f.record("legacy", "claude-a", "m-legacy", "legacy-client", "", "", 10_000)

	foreign := []string{"m-bob", "m-legacy", "claude-a", bob.ID, bob.KeyID, "legacy-client"}
	reads := map[string]func(string) *httptest.ResponseRecorder{
		"session": func(q string) *httptest.ResponseRecorder { return f.session(http.MethodGet, "/ui/v1/me/"+q, alice, "") },
		"bearer":  func(q string) *httptest.ResponseRecorder { return f.bearerGet("/control/v1/"+q, alice.Key) },
	}
	for name, get := range reads {
		r := get("analytics?range=24h&group=model&top=1")
		wantStatus(t, r, http.StatusOK)
		res := decodeAnalytics(t, r.Body.Bytes())
		if res.Totals.Requests != 4 {
			t.Errorf("%s totals = %+v, want alice's 4 requests", name, res.Totals)
		}
		var keys []string
		for _, s := range res.Series {
			keys = append(keys, s.Key)
			if s.Key == core.AnalyticsOtherKey && s.Total.Requests != 1 {
				t.Errorf("%s __other__ = %d requests, want alice's 1", name, s.Total.Requests)
			}
		}
		if strings.Join(keys, ",") != "m-alice-big,"+core.AnalyticsOtherKey {
			t.Errorf("%s top-1 series = %v", name, keys)
		}
		for _, q := range []string{"usage?group=model", "usage?group=client", "usage?group=key", "usage?group=account",
			"analytics?range=24h&group=account", "analytics/dimensions?range=24h"} {
			r := get(q)
			wantStatus(t, r, http.StatusOK)
			for _, s := range foreign {
				if strings.Contains(r.Body.String(), s) {
					t.Errorf("%s %s leaks %q", name, q, s)
				}
			}
		}
	}
}
