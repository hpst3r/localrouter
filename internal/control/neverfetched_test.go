package control

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

// A snapshot whose first fetch failed has zero FetchedAt; status must report
// snapshot_age_s null and stale, not an enormous age.
func TestStatusNeverFetchedHasNullAge(t *testing.T) {
	q := &fakeQuota{snaps: map[string]core.Snapshot{
		"a": {AccountID: "a", Err: "usage api: credential unavailable"},
	}}
	s := New(Deps{
		Accounts: []core.Account{{ID: "a", Provider: core.ProviderCodex}},
		Quota:    q,
		Policy:   &fakePolicy{states: map[string]core.AccountState{}},
		Ledger:   &fakeLedger{},
		Clock:    fakeClock{t0},
	}, Options{})
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/control/v1/status", nil))
	var body struct {
		Accounts []struct {
			SnapshotAgeS *int64  `json:"snapshot_age_s"`
			Stale        bool    `json:"stale"`
			Error        *string `json:"error"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	a := body.Accounts[0]
	if a.SnapshotAgeS != nil || !a.Stale || a.Error == nil {
		t.Fatalf("got age=%v stale=%v err=%v", a.SnapshotAgeS, a.Stale, a.Error)
	}
}
