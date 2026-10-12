package control

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hpst3r/localrouter/internal/core"
)

func ingestBody(userID, keyID string) string {
	return `{"schema_version":1,"host":"h2","records":[{"id":"cc:1","account_id":"claude-a",` +
		`"started_at":"` + t0.Add(-time.Hour).Format(time.RFC3339) + `","usage_known":true,` +
		`"usage":{"input_tokens":5,"output_tokens":1},"model":"claude-x",` +
		`"user_id":"` + userID + `","UserID":"` + userID + `","key_id":"` + keyID + `","KeyID":"` + keyID + `",` +
		`"client":"forged"}]}`
}

func TestMultiUserIngestCannotForgeOwner(t *testing.T) {
	f := newMUFixture(t, nil)
	alice := f.addUser("alice", "user")

	rec := f.bearerPost("/control/v1/ingest", ingestServiceToken, ingestBody(alice.ID, alice.KeyID))
	wantStatus(t, rec, http.StatusOK)

	ctx := context.Background()
	own, err := f.led.SummaryScoped(ctx, t0.Add(-48*time.Hour), "model", core.DataScope{UserID: alice.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(own) != 0 {
		t.Fatalf("forged ingest attributed to alice: %+v", own)
	}
	all, err := f.led.SummaryScoped(ctx, t0.Add(-48*time.Hour), "user", core.DataScope{AllUsers: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Key != "" || all[0].Requests != 1 {
		t.Fatalf("ingested row owner = %+v, want unowned", all)
	}
	byClient, _ := f.led.SummaryScoped(ctx, t0.Add(-48*time.Hour), "client", core.DataScope{AllUsers: true})
	if len(byClient) != 1 || byClient[0].Key != "agent" {
		t.Fatalf("ingested client = %+v, want the authenticated service", byClient)
	}
}

func TestMultiUserIngestRequiresIngestService(t *testing.T) {
	f := newMUFixture(t, nil)
	alice := f.addUser("alice", "user")
	for _, tok := range []string{serviceToken, alice.Key} {
		rec := f.bearerPost("/control/v1/ingest", tok, ingestBody("", ""))
		wantStatus(t, rec, http.StatusForbidden)
	}
	rec := f.bearerPost("/control/v1/ingest", "", ingestBody("", ""))
	wantStatus(t, rec, http.StatusUnauthorized)
	if strings.Contains(rec.Body.String(), "forged") {
		t.Fatal("body reflected")
	}
}
