package control

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestMultiUserDiagnosticsIdentityHealthNoPII(t *testing.T) {
	f := newMUFixture(t, nil)
	root := f.addUser("root", "admin")

	rec := f.session(http.MethodGet, "/ui/v1/admin/diagnostics", root, "")
	wantStatus(t, rec, http.StatusOK)
	var doc struct {
		Identity *storageHealth `json:"identity"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Identity == nil || !doc.Identity.Configured || !doc.Identity.OK {
		t.Fatalf("identity health = %+v", doc.Identity)
	}
	for _, pii := range []string{muIssuer, muClientID, "root@example.test", "Name root", root.ID} {
		if strings.Contains(rec.Body.String(), pii) {
			t.Fatalf("diagnostics leaked %q", pii)
		}
	}

	_ = f.ids.Close()
	rec = f.bearerGet("/control/v1/diagnostics", serviceToken)
	wantStatus(t, rec, http.StatusOK)
	doc.Identity = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Identity == nil || doc.Identity.OK || doc.Identity.Error != storageErrorUnavailable {
		t.Fatalf("closed identity health = %+v", doc.Identity)
	}
	if strings.Contains(rec.Body.String(), "closed") {
		t.Fatalf("raw store error leaked: %s", rec.Body.String())
	}
}

func TestLegacyDiagnosticsHasNoIdentityBlock(t *testing.T) {
	f := newFixture(false)
	rec := f.do(t, http.MethodGet, "/control/v1/diagnostics", "", "")
	if strings.Contains(rec.Body.String(), `"identity"`) {
		t.Fatalf("legacy diagnostics gained an identity block: %s", rec.Body.String())
	}
}

func TestMultiUserReadinessFoldsIdentityStore(t *testing.T) {
	f := newMUFixture(t, nil)
	wantStatus(t, f.bearerGet("/readyz", ""), http.StatusOK)
	_ = f.ids.Close()
	rec := f.bearerGet("/readyz", "")
	wantStatus(t, rec, http.StatusServiceUnavailable)
	if strings.TrimSpace(rec.Body.String()) != `{"ready":false}` {
		t.Fatalf("readyz body = %s", rec.Body.String())
	}
	rec = f.bearerGet("/control/v1/diagnostics", serviceToken)
	if !strings.Contains(rec.Body.String(), `"ready":false`) {
		t.Fatalf("diagnostics ready with identity down: %s", rec.Body.String())
	}

	g := newMUFixture(t, func(d *Deps) { d.Identity = nil })
	wantStatus(t, g.bearerGet("/readyz", ""), http.StatusServiceUnavailable)
}
