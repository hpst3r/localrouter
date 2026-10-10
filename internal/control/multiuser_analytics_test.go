package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/hpst3r/localrouter/internal/core"
)

func decodeAnalytics(t *testing.T, body []byte) core.AnalyticsResult {
	t.Helper()
	var res core.AnalyticsResult
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatalf("decode analytics: %v", err)
	}
	return res
}

func breakdownKeys(res core.AnalyticsResult) map[string]int64 {
	out := map[string]int64{}
	for _, r := range res.Breakdown {
		out[r.Key] = r.Requests
	}
	return out
}

func TestMultiUserAnalyticsScopedBeforeAggregation(t *testing.T) {
	f := newMUFixture(t, nil)
	alice, bob := f.addUser("alice", "user"), f.addUser("bob", "user")
	seedTwoOwners(f, alice, bob)

	rec := f.bearerGet("/control/v1/analytics?range=24h&group=model", alice.Key)
	wantStatus(t, rec, http.StatusOK)
	res := decodeAnalytics(t, rec.Body.Bytes())
	if got := breakdownKeys(res); len(got) != 1 || got["gpt-alice"] != 2 {
		t.Fatalf("alice model breakdown = %v", got)
	}
	if res.Totals.Requests != 2 || res.Totals.InputTokens != 150 {
		t.Fatalf("alice totals = %+v (other owners counted)", res.Totals)
	}
	if strings.Contains(rec.Body.String(), "gpt-bob") || strings.Contains(rec.Body.String(), "gpt-legacy") {
		t.Fatalf("foreign rows leaked: %s", rec.Body.String())
	}
}

func TestMultiUserAnalyticsCrossFiltersCannotWidenScope(t *testing.T) {
	f := newMUFixture(t, nil)
	alice, bob := f.addUser("alice", "user"), f.addUser("bob", "user")
	seedTwoOwners(f, alice, bob)

	// Owner override is not a dimension for a user scope.
	for _, q := range []string{"filter.user=" + bob.ID, "group=user", "filter.user="} {
		rec := f.bearerGet("/control/v1/analytics?range=24h&"+q, alice.Key)
		wantStatus(t, rec, http.StatusBadRequest)
		if strings.Contains(rec.Body.String(), bob.ID) {
			t.Fatalf("%s: bob id echoed: %s", q, rec.Body.String())
		}
	}
	// Filtering by another owner's key or labels finds nothing.
	for _, q := range []string{"filter.key=" + bob.KeyID, "filter.client=" + bob.KeyID, "filter.model=gpt-bob",
		"filter.model=gpt-legacy", "filter.client=legacy-client"} {
		rec := f.bearerGet("/control/v1/analytics?range=24h&group=model&"+q, alice.Key)
		wantStatus(t, rec, http.StatusOK)
		res := decodeAnalytics(t, rec.Body.Bytes())
		if res.Totals.Requests != 0 || len(res.Breakdown) != 0 {
			t.Fatalf("%s: alice saw foreign rows: %+v %v", q, res.Totals, breakdownKeys(res))
		}
	}
	// Own key filter works.
	rec := f.bearerGet("/control/v1/analytics?range=24h&group=key&filter.key="+alice.KeyID, alice.Key)
	wantStatus(t, rec, http.StatusOK)
	if got := breakdownKeys(decodeAnalytics(t, rec.Body.Bytes())); len(got) != 1 || got[alice.KeyID] != 1 {
		t.Fatalf("own key filter = %v", got)
	}
	// A global reader can filter by user, including the unowned legacy rows.
	rec = f.bearerGet("/control/v1/analytics?range=24h&group=model&filter.user="+bob.ID, serviceToken)
	wantStatus(t, rec, http.StatusOK)
	if got := breakdownKeys(decodeAnalytics(t, rec.Body.Bytes())); len(got) != 1 || got["gpt-bob"] != 1 {
		t.Fatalf("service filter.user = %v", got)
	}
}

func TestMultiUserDimensionsScoped(t *testing.T) {
	f := newMUFixture(t, nil)
	alice, bob := f.addUser("alice", "user"), f.addUser("bob", "user")
	seedTwoOwners(f, alice, bob)

	rec := f.bearerGet("/control/v1/analytics/dimensions?range=24h", alice.Key)
	wantStatus(t, rec, http.StatusOK)
	var doc dimensionsDoc
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Dimensions["user"]; ok {
		t.Fatal("user dimension exposed to a user scope")
	}
	keys := doc.Dimensions["key"]
	if len(keys) != 2 { // alice's key and her unkeyed static-client row ("")
		t.Fatalf("key dimension = %+v", keys)
	}
	for _, needle := range []string{bob.ID, bob.KeyID, "gpt-bob", "gpt-legacy", "legacy-client"} {
		if strings.Contains(rec.Body.String(), needle) {
			t.Fatalf("dimensions leaked %q: %s", needle, rec.Body.String())
		}
	}
	for _, dim := range core.AnalyticsDimensions {
		if _, ok := doc.Dimensions[dim]; !ok {
			t.Fatalf("missing legacy dimension %s", dim)
		}
	}

	rec = f.bearerGet("/control/v1/analytics/dimensions?range=24h", serviceToken)
	wantStatus(t, rec, http.StatusOK)
	doc = dimensionsDoc{}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Dimensions["user"]) != 3 {
		t.Fatalf("service user dimension = %+v", doc.Dimensions["user"])
	}
}
